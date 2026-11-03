package main

import (
	"io"
	"net/http"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/zakijss/minideck/ui"
)

// A seek mid-stream: the player opens the relay's URL with ?t=, the
// server gets a new PlaybackInfo and transcode from there, the old
// transcode is stopped, and the reports follow the new session and place.
func TestSeekStopsOldEncoding(t *testing.T) {
	f := newFakeJellyfin(t)
	a, _ := testApp(t, f)
	urlFile, goFile := filepath.Join(a.dir, "url"), filepath.Join(a.dir, "go")
	a.pl.own = script(t, a.dir, "minifin-player", `
while [ $# -gt 0 ]; do case "$1" in --url) url=$2;; esac; shift; done
echo "$url" > `+urlFile+`.tmp && mv `+urlFile+`.tmp `+urlFile+`
echo "pos 100"
echo "pos 101"
i=0; while [ ! -f `+goFile+` ] && [ $i -lt 200 ]; do sleep 0.05; i=$((i+1)); done
echo "pos 501"
echo "pos 502"
echo "quit 503"
`)
	seekErr := make(chan string, 1)
	go func() {
		defer os.WriteFile(goFile, nil, 0644)
		var local string
		for i := 0; i < 200 && local == ""; i++ {
			b, _ := os.ReadFile(urlFile)
			local = strings.TrimSpace(string(b))
			time.Sleep(20 * time.Millisecond)
		}
		resp, err := http.Get(local + "?t=500.5")
		if err != nil {
			seekErr <- err.Error()
			return
		}
		io.ReadFull(resp.Body, make([]byte, 4096))
		resp.Body.Close()
		if resp.StatusCode != 200 || resp.Header.Get(startHeader) != "500.500" {
			seekErr <- "HTTP " + resp.Status + " start " + resp.Header.Get(startHeader)
			return
		}
		// opening the same place again reuses the transcode
		resp, err = http.Get(local + "?t=500.5")
		if err == nil {
			resp.Body.Close()
		}
		seekErr <- ""
	}()
	it, _ := a.c.Item("m1")
	a.play(it, 100, -1, -1)
	if e := <-seekErr; e != "" {
		t.Fatal(e)
	}
	f.mu.Lock()
	defer f.mu.Unlock()
	if len(f.encStops) != 1 || f.encStops[0] != "ps-1 minifin-test" {
		t.Fatalf("encodings stopped: %v", f.encStops)
	}
	if len(f.playInfo) != 2 || f.playInfo[1]["StartTimeTicks"].(float64) != 5005000000 {
		t.Fatalf("PlaybackInfo calls %d", len(f.playInfo))
	}
	last := f.streams[len(f.streams)-1].Query
	if last["StartTimeTicks"] != "5005000000" || last["PlaySessionId"] != "ps-2" {
		t.Fatalf("the new transcode: %v", last)
	}
	var seekReport, stopped map[string]any
	for _, r := range f.reports {
		switch {
		case r.Path == "/Sessions/Playing/Progress" && seekReport == nil:
			seekReport = r.Body
		case r.Path == "/Sessions/Playing/Stopped":
			stopped = r.Body
		}
	}
	if seekReport == nil || seekReport["PositionTicks"].(float64) != 501*ticksPerSecond || seekReport["PlaySessionId"] != "ps-2" {
		t.Fatalf("the Progress after the seek: %v", seekReport)
	}
	if stopped["PlaySessionId"] != "ps-2" || stopped["PositionTicks"].(float64) != 503*ticksPerSecond {
		t.Fatalf("stopped %v", stopped)
	}
	if strings.Contains(strings.Join(f.requests, "\n"), f.token) {
		t.Fatal("the token is in a URL")
	}
}

// A download seeks too: ?t= serves it from the keyframe at or before t,
// and says where that is.
func TestLocalSeek(t *testing.T) {
	f := newFakeJellyfin(t)
	f.streamCut = int64(len(makeTS(0, 120)) / 2) // two parts, to seek across
	d, _ := testDownloads(t, f)
	go d.Run()
	defer d.Stop()
	d.Add(shortItem())
	dl := waitState(t, d, "m3", dlDone)
	if len(dl.Parts) != 2 {
		t.Fatalf("%d parts", len(dl.Parts))
	}
	r := testRelay(t)
	ls, _, _ := openLocal(dl, 0)
	local := r.Local(ls, func(t float64) (*localStream, float64, error) { return openLocal(dl, t) })
	for _, want := range []float64{91.3, 20} { // in the second part, then the first
		resp, err := http.Get(local + "?t=" + strconv.FormatFloat(want, 'f', -1, 64))
		if err != nil {
			t.Fatal(err)
		}
		b, _ := io.ReadAll(resp.Body)
		resp.Body.Close()
		start := resp.Header.Get(startHeader)
		at, err := strconv.ParseFloat(start, 64)
		if err != nil || at > want || at < want-2.1 {
			t.Fatalf("t=%v starts at %q", want, start)
		}
		pts := videoPTS(b)
		// the first frame is at the keyframe's time on the first part's clock
		first := float64(ptsDiff(pts[0], dl.Parts[0].FirstPTS)) / ptsHz
		if d := first - at; d > 0.01 || d < -0.01 {
			t.Fatalf("t=%v: first frame at %.3f, header says %.3f", want, first, at)
		}
		for i := 1; i < len(pts); i++ {
			if step := ptsDiff(pts[i], pts[i-1]); step <= 0 || step > ptsHz {
				t.Fatalf("t=%v: timestamp jump at frame %d", want, i)
			}
		}
	}
	// bad values
	for _, bad := range []string{"x", "-3", "NaN"} {
		if resp, _ := http.Get(local + "?t=" + bad); resp.StatusCode != http.StatusBadRequest {
			t.Errorf("t=%s: HTTP %d", bad, resp.StatusCode)
		}
	}
	plain := r.Stream("http://127.0.0.1:1/x.ts", nil)
	if resp, _ := http.Get(plain + "?t=5"); resp.StatusCode != http.StatusBadRequest {
		t.Errorf("a stream that can't seek: HTTP %d", resp.StatusCode)
	}
}

// The countdown: A plays at once, B stops, and with no press it plays
// when the time runs out.
func TestCountdown(t *testing.T) {
	f := newFakeJellyfin(t)
	a, in := testApp(t, f)
	old := nextWait
	defer func() { nextWait = old }()
	next := f.byID["ep3"]
	nextWait = 5 * time.Second
	in <- ui.Event{Button: ui.A, Pressed: true}
	if start := time.Now(); !a.countdown(next) || time.Since(start) > time.Second {
		t.Fatal("A didn't play it at once")
	}
	in <- ui.Event{Button: ui.B, Pressed: true}
	if a.countdown(next) {
		t.Fatal("B didn't stop it")
	}
	nextWait = 80 * time.Millisecond
	if !a.countdown(next) {
		t.Fatal("the countdown didn't play it")
	}
}

// After an episode plays to the end, the next one plays (after the
// countdown), and so on until the show's last episode.
func TestNextEpisodeChain(t *testing.T) {
	f := newFakeJellyfin(t)
	a, _ := testApp(t, f)
	old := nextWait
	nextWait = 50 * time.Millisecond
	defer func() { nextWait = old }()
	titles := filepath.Join(a.dir, "titles")
	a.pl.own = script(t, a.dir, "minifin-player", `
while [ $# -gt 0 ]; do case "$1" in --title) echo "$2" >> `+titles+`;; esac; shift; done
echo "pos 10"
echo "end"
`)
	a.push(newHome(a))
	ep2 := f.byID["ep2"]
	a.play(ep2, ep2.Resume(), -1, -1)
	b, _ := os.ReadFile(titles)
	got := strings.Split(strings.TrimSpace(string(b)), "\n")
	if len(got) != 2 || !strings.HasSuffix(got[0], "The Long Wave") || !strings.HasSuffix(got[1], "Interference Patterns on the Northern Relay") {
		t.Fatalf("played %q", got)
	}
	if d, ok := a.top().(*detailPage); !ok || d.it.ID != "ep3" {
		t.Fatalf("ends on %T", a.top())
	}
	// a movie doesn't go on
	os.Remove(titles)
	a.play(f.byID["m2"], 0, -1, -1)
	if b, _ := os.ReadFile(titles); strings.Count(string(b), "\n") != 1 {
		t.Fatalf("after a movie: %q", b)
	}
}

// Offline, the next episode is the next one downloaded.
func TestNextEpisodeOffline(t *testing.T) {
	f := newFakeJellyfin(t)
	a, _ := testApp(t, f)
	a.offline = true
	a.dl.mu.Lock()
	for _, id := range []string{"ep1", "ep3"} {
		a.dl.list[id] = &Download{ID: id, Item: f.byID[id], State: dlDone}
	}
	a.dl.mu.Unlock()
	if n := a.nextEpisode(f.byID["ep1"]); n == nil || n.ID != "ep3" {
		t.Fatalf("next %+v", n)
	}
	if n := a.nextEpisode(f.byID["ep3"]); n != nil {
		t.Fatalf("after the last: %+v", n)
	}
}

// After a video, the library's watched marks are read again, and the grid
// stays where it was.
func TestLibraryRefresh(t *testing.T) {
	f := newFakeJellyfin(t)
	a, _ := testApp(t, f)
	lib := newLibrary(a, Item{ID: "lib-movies", Name: "Movies", CollectionType: "movies"})
	a.push(lib)
	a.pump(t, func() bool { return lib.total == 5 && len(lib.loading) == 0 })
	lib.sel = 3
	it := lib.items[3]
	if it.Played() {
		t.Fatal("already watched")
	}
	movies := sampleMovies()
	for i := range movies {
		if movies[i].ID == it.ID {
			movies[i].UserData = &UserData{Played: true}
		}
	}
	f.setLibrary(movies)
	a.afterPlay(it, playOutcome{Player: "minifin-player", Ended: true})
	a.pump(t, func() bool { return len(lib.loading) == 0 && lib.items[3].Played() })
	if lib.sel != 3 {
		t.Fatalf("selection moved to %d", lib.sel)
	}
}

// In the player, X / SELECT pick other tracks (the video starts again
// where it was) and L2 / R2 go to the previous or next episode.
func TestPlayerControls(t *testing.T) {
	f := newFakeJellyfin(t)
	a, _ := testApp(t, f)
	runs := filepath.Join(a.dir, "runs")
	a.pl.own = script(t, a.dir, "minifin-player", `
printf '%s\n' "$@" | tr '\n' ' ' >> `+runs+`; echo >> `+runs+`
n=$(wc -l < `+runs+`)
case "$n" in
*1) echo "pos 2712"; echo "audio 0"; echo "subtitle 2"; echo "quit 3000";;
*3) echo "pos 900"; echo "prev"; echo "quit 905";;
*) echo "quit 1";;
esac
`)
	it, _ := a.c.Item("m1")
	a.push(newDetail(a, it))
	a.play(it, 2712, -1, -1)
	b, _ := os.ReadFile(runs)
	got := strings.Split(strings.TrimSpace(string(b)), "\n")
	if len(got) != 2 {
		t.Fatalf("runs %q", got)
	}
	for _, want := range []string{"--audio English - EAC3 5.1|French - AAC Stereo --audio-current 1", "--subs Off|English - SUBRIP|English SDH - PGS --subs-current 0"} {
		if !strings.Contains(got[0], want) {
			t.Errorf("first run %q: no %q", got[0], want)
		}
	}
	for _, want := range []string{"--start 3000.000", "--audio-current 0", "--subs-current 2"} {
		if !strings.Contains(got[1], want) {
			t.Errorf("second run %q: no %q", got[1], want)
		}
	}
	if strings.Contains(got[0], "--next") {
		t.Error("a movie has no next episode")
	}
	ep2, _ := a.c.Item("ep2")
	a.play(ep2, 900, -1, -1)
	b, _ = os.ReadFile(runs)
	got = strings.Split(strings.TrimSpace(string(b)), "\n")
	if len(got) != 4 || !strings.Contains(got[2], "--next S2:E3 Interference") || !strings.Contains(got[2], "--prev S2:E1 Dead Air") ||
		!strings.Contains(got[3], "Dead Air") || !strings.Contains(got[3], "--next S2:E2 The Long Wave") {
		t.Fatalf("episode runs %q", got)
	}
}
