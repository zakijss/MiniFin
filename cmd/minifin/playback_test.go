package main

import (
	"bytes"
	"context"
	"io"
	"log"
	"net/http"
	"net/url"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/zakijss/minideck/ui"
)

// script writes an executable shell script.
func script(t *testing.T, dir, name, body string) string {
	t.Helper()
	p := filepath.Join(dir, name)
	if err := os.WriteFile(p, []byte("#!/bin/sh\n"+body), 0755); err != nil {
		t.Fatal(err)
	}
	return p
}

// minifin-player's lines, and a player that counts from the stream's
// start instead of the video's.
func TestPlayerLines(t *testing.T) {
	st := &playerState{start: 600}
	for _, l := range []string{"dur 5400.5", "pos 601", "paused 1", "paused 0", "pos 612.25", "junk", "quit 615"} {
		st.line(l)
	}
	if st.Dur != 5400.5 || st.Pos != 615 || st.Paused || !st.Quit || st.relative {
		t.Fatalf("state %+v", st)
	}
	rel := &playerState{start: 600}
	rel.line("pos 1.5")
	rel.line("pos 12")
	if !rel.relative || rel.Pos != 612 {
		t.Fatalf("relative %+v", rel)
	}
	e := &playerState{}
	e.line("error ")
	e.line("end")
	if e.Err == "" || !e.Ended {
		t.Fatalf("end/error %+v", e)
	}
	if (&playerState{}).line("pos -3") || (&playerState{}).line("pos nan") || (&playerState{}).line("paused 2") {
		t.Fatal("took a bad line")
	}
}

// ffplay's positions: showinfo's pts_time and the -stats line, from the
// first timestamp seen.
func TestFFplayClock(t *testing.T) {
	c := &ffplayClock{start: 300}
	recs := []string{
		"ffplay version 2.4.14 Copyright (c) 2003-2014 the FFmpeg developers",
		"[Parsed_showinfo_3 @ 0x1a2b] n:   0 pts: 126000 pts_time:1.4 pos:  564 fmt:yuv420p",
		"   2.40 A-V:  0.012 fd=   0 aq=   12KB vq=  110KB sq=    0B f=0/0   ",
		"[Parsed_showinfo_3 @ 0x1a2b] n:  24 pts: 216000 pts_time:11.4 pos: 99999",
		"    nan A-V:    nan fd=   0 aq=    0KB vq=    0KB sq=    0B f=0/0",
		"  13.40 M-A:  0.000 fd=   0 aq=   10KB vq=    0KB sq=    0B f=0/0",
	}
	var got []float64
	for _, r := range recs {
		if c.record(r) {
			got = append(got, c.Pos)
		}
	}
	want := []float64{300, 301, 310, 312}
	if len(got) != len(want) {
		t.Fatalf("positions %v", got)
	}
	for i := range want {
		if d := got[i] - want[i]; d > 0.01 || d < -0.01 {
			t.Fatalf("positions %v, want %v", got, want)
		}
	}
	// ffplay ends status lines with \r
	var recsOut []string
	data := []byte("a\rb\nc\r\rd")
	for len(data) > 0 {
		n, tok, _ := splitRecords(data, true)
		recsOut = append(recsOut, string(tok))
		data = data[n:]
	}
	if strings.Join(recsOut, ",") != "a,b,c,,d" {
		t.Fatalf("records %q", recsOut)
	}
}

// ffplay's command line: fullscreen, quits at the end, the picture flipped
// for the panel, a frame sampled each second for the position.
func TestFFplayArgs(t *testing.T) {
	args := strings.Join(ffplayArgs(playRequest{URL: "http://127.0.0.1:5/v/x.ts", Start: 30}), " ")
	for _, want := range []string{"-fs", "-autoexit", "hflip,vflip", "showinfo", "-i http://127.0.0.1:5/v/x.ts"} {
		if !strings.Contains(args, want) {
			t.Errorf("no %q in %s", want, args)
		}
	}
	if strings.Contains(args, "-ss") {
		t.Error("ffplay shouldn't seek: the stream already starts there")
	}
}

// minifin-player is run with the contract's arguments, and its lines come
// through as positions.
func TestRunOwnPlayer(t *testing.T) {
	dir := t.TempDir()
	argsFile := filepath.Join(dir, "args")
	own := script(t, dir, "minifin-player", `printf '%s\n' "$@" > `+argsFile+`
echo "dur 120"
echo "decoder hw"
echo "pos 30"
echo "pos 31"
echo "paused 1"
echo "paused 0"
echo "pos 32.5"
echo "quit 33"
exit 0
`)
	pl := &players{own: own, ffplay: "/nonexistent", startWait: 4 * time.Second}
	in := make(chan ui.Event, 4)
	var ups []string
	out := pl.play(context.Background(), playRequest{URL: "http://127.0.0.1:9/v/a.ts", Start: 30, Title: "Low Orbit (2011)", Sub: "/tmp/s.srt", Runtime: 120},
		in, func(pos float64, paused bool) {
			ups = append(ups, strconv.FormatFloat(pos, 'f', -1, 64)+map[bool]string{true: "p", false: ""}[paused])
		})
	if out.Player != "minifin-player" || out.Pos != 33 || out.Ended || out.Err != "" || out.Decoder != "hw" {
		t.Fatalf("outcome %+v", out)
	}
	b, _ := os.ReadFile(argsFile)
	if got := strings.Fields(string(b)); strings.Join(got, " ") != "--url http://127.0.0.1:9/v/a.ts --start 30.000 --title Low Orbit (2011) --sub /tmp/s.srt --duration 120.000" {
		t.Fatalf("args %q", got)
	}
	if strings.Join(ups, ",") != "31,31p,31,32.5,33" {
		t.Fatalf("updates %v", ups)
	}
}

// When minifin-player isn't there, or dies at once, ffplay plays, and the
// outcome says why.
func TestFFplayFallback(t *testing.T) {
	dir := t.TempDir()
	ff := script(t, dir, "ffplay", `echo "ffplay version 2.4.14" >&2
printf '[Parsed_showinfo_3 @ 0x1] n:   0 pts: 126000 pts_time:1.4 pos: 1\n' >&2
printf '   5.40 A-V:  0.001 fd=   0 aq=   1KB vq=  1KB sq=    0B f=0/0   \r' >&2
printf '[Parsed_showinfo_3 @ 0x1] n:   9 pts: 126000 pts_time:118.9 pos: 1\n' >&2
exit 0
`)
	pl := &players{own: filepath.Join(dir, "missing"), ffplay: ff, startWait: 4 * time.Second}
	in := make(chan ui.Event, 4)
	out := pl.play(context.Background(), playRequest{URL: "http://x/v/1.ts", Runtime: 120}, in, func(float64, bool) {})
	if out.Player != "ffplay" || !out.Ended || out.Why != "minifin-player isn't installed" {
		t.Fatalf("outcome %+v", out)
	}
	if got := describePlayer(out); got != "Played with ffplay: minifin-player isn't installed." {
		t.Fatalf("note %q", got)
	}
	// a player that fails at once
	pl.own = script(t, dir, "broken-player", "echo 'error no SDL' \nexit 3\n")
	out = pl.play(context.Background(), playRequest{URL: "http://x/v/1.ts", Start: 10, Runtime: 600}, in, func(float64, bool) {})
	if out.Player != "ffplay" || !strings.Contains(out.Why, "didn't start") {
		t.Fatalf("broken player outcome %+v", out)
	}
	if out.Ended || out.Pos < 100 {
		t.Fatalf("ffplay position %+v", out)
	}
	// forced in Settings
	pl.own, pl.force = script(t, dir, "good-player", "echo 'end'\n"), "ffplay"
	out = pl.play(context.Background(), playRequest{URL: "http://x/v/1.ts", Runtime: 120}, in, func(float64, bool) {})
	if out.Player != "ffplay" || !strings.Contains(out.Why, "Settings") {
		t.Fatalf("forced %+v", out)
	}
}

// B stops ffplay (to ffplay it is nothing), and A presses are pauses.
func TestFFplayButtons(t *testing.T) {
	dir := t.TempDir()
	ff := script(t, dir, "ffplay", `trap 'exit 123' TERM
printf '[Parsed_showinfo_3 @ 0x1] n:   0 pts: 126000 pts_time:1.4 pos: 1\n' >&2
printf '[Parsed_showinfo_3 @ 0x1] n:   0 pts: 126000 pts_time:41.4 pos: 1\n' >&2
i=0; while [ $i -lt 100 ]; do sleep 0.1; i=$((i+1)); done
`)
	pl := &players{own: "/missing", ffplay: ff, startWait: time.Second}
	in := make(chan ui.Event, 4)
	go func() {
		time.Sleep(400 * time.Millisecond)
		in <- ui.Event{Button: ui.A, Pressed: true}
		time.Sleep(100 * time.Millisecond)
		in <- ui.Event{Button: ui.B, Pressed: true}
	}()
	var paused []bool
	start := time.Now()
	out := pl.play(context.Background(), playRequest{URL: "http://x/v/1.ts", Start: 100, Runtime: 5000}, in, func(_ float64, p bool) { paused = append(paused, p) })
	if time.Since(start) > 5*time.Second {
		t.Fatal("B didn't stop ffplay")
	}
	if out.Ended || out.Err != "" || out.Pos != 140 {
		t.Fatalf("outcome %+v", out)
	}
	if len(paused) == 0 || !paused[len(paused)-1] {
		t.Fatalf("pause not seen: %v", paused)
	}
}

// The reporter sends Playing, Progress on pause and every 10 s, and
// Stopped with the position (the runtime when it ended).
func TestReporter(t *testing.T) {
	f := newFakeJellyfin(t)
	testEnv(t)
	c := NewClient(f.URL(), f.token, "user-1", "minifin-test")
	r := newReporter(c, streamPlan{ItemID: "m1", MediaSourceID: "ms1", PlaySessionID: "ps-1", Audio: 1, Sub: -1}, "Transcode")
	r.every = 50 * time.Millisecond
	r.update(10, false)
	r.update(11, false)
	time.Sleep(60 * time.Millisecond)
	r.update(12, false) // 10 s (here 50 ms) later: progress
	r.update(12, true)  // pause: at once
	r.update(13, true)
	if !r.stop(14, true, 7920, false) {
		t.Fatal("reports didn't arrive")
	}
	r.update(15, false) // after stop: nothing, and no panic
	f.mu.Lock()
	defer f.mu.Unlock()
	var paths []string
	for _, rp := range f.reports {
		paths = append(paths, strings.TrimPrefix(rp.Path, "/Sessions/Playing"))
	}
	if strings.Join(paths, ",") != ",/Progress,/Progress,/Stopped" {
		t.Fatalf("reports %v", paths)
	}
	first, pause, stop := f.reports[0].Body, f.reports[2].Body, f.reports[3].Body
	if first["ItemId"] != "m1" || first["PlaySessionId"] != "ps-1" || first["PositionTicks"].(float64) != 10*ticksPerSecond || first["PlayMethod"] != "Transcode" {
		t.Fatalf("playing %v", first)
	}
	if pause["IsPaused"] != true || pause["EventName"] != "Pause" {
		t.Fatalf("pause %v", pause)
	}
	if stop["PositionTicks"].(float64) != 7920*ticksPerSecond || stop["MediaSourceId"] != "ms1" {
		t.Fatalf("stopped %v", stop)
	}
}

// A stream is planned with the device's profile, and the URL asks for
// exactly what the device plays, whatever the server put in it.
func TestPlanProfileAndURL(t *testing.T) {
	f := newFakeJellyfin(t)
	testEnv(t)
	c := NewClient(f.URL(), f.token, "user-1", "minifin-test")
	it, _ := c.Item("m1")
	p, err := c.Plan(it, 2712, 2, 3, 1200, true)
	if err != nil {
		t.Fatal(err)
	}
	u, _ := url.Parse(p.URL)
	q := u.Query()
	want := map[string]string{"MaxWidth": "640", "MaxHeight": "480", "MaxFramerate": "30", "VideoBitRate": "1200000",
		"AudioBitRate": "96000", "AudioChannels": "2", "VideoCodec": "h264", "AudioCodec": "aac", "h264-level": "40",
		"h264-profile": "main,baseline,constrainedbaseline", "StartTimeTicks": "27120000000", "AudioStreamIndex": "2",
		"PlaySessionId": "ps-1", "DeviceId": "minifin-test", "MediaSourceId": "ms1", "AllowVideoStreamCopy": "false",
		"SubtitleStreamIndex": "-1"}
	for k, v := range want {
		if q.Get(k) != v {
			t.Errorf("%s = %q, want %q", k, q.Get(k), v)
		}
	}
	for k := range q {
		if strings.EqualFold(k, "api_key") || strings.EqualFold(k, "VideoBitrate") && k != "VideoBitRate" {
			t.Errorf("left in the URL: %s", k)
		}
	}
	if !strings.HasPrefix(p.URL, f.URL()+"/videos/m1/stream.ts?") {
		t.Errorf("URL %s", p.URL)
	}
	if p.SubURL != f.URL()+"/Videos/m1/ms1/Subtitles/3/0/Stream.srt" {
		t.Errorf("subtitles %s", p.SubURL)
	}
	// what PlaybackInfo was sent
	f.mu.Lock()
	body := f.playInfo[len(f.playInfo)-1]
	f.mu.Unlock()
	dp := body["DeviceProfile"].(map[string]any)
	tp := dp["TranscodingProfiles"].([]any)[0].(map[string]any)
	if tp["Container"] != "ts" || tp["VideoCodec"] != "h264" || tp["AudioCodec"] != "aac" || tp["Protocol"] != "http" {
		t.Errorf("transcoding profile %v", tp)
	}
	conds := map[string]string{}
	for _, cp := range dp["CodecProfiles"].([]any) {
		for _, cd := range cp.(map[string]any)["Conditions"].([]any) {
			m := cd.(map[string]any)
			conds[m["Property"].(string)] = m["Value"].(string)
		}
	}
	for k, v := range map[string]string{"Width": "640", "Height": "480", "VideoFramerate": "30", "VideoLevel": "40", "VideoBitrate": "1200000", "AudioChannels": "2"} {
		if conds[k] != v {
			t.Errorf("condition %s = %q", k, conds[k])
		}
	}
	if body["StartTimeTicks"].(float64) != 27120000000 || body["EnableDirectPlay"] != false || body["MaxStreamingBitrate"].(float64) != 1296000 {
		t.Errorf("playback info body %v", body)
	}
	// a PGS track is burned in by the server
	p, _ = c.Plan(it, 0, -1, 4, 600, true)
	u, _ = url.Parse(p.URL)
	if u.Query().Get("SubtitleMethod") != "Encode" || u.Query().Get("SubtitleStreamIndex") != "4" || p.SubURL != "" {
		t.Errorf("burn-in: %s / %s", p.URL, p.SubURL)
	}
	// with ffplay (no external subtitles), a text track is burned in too
	p, _ = c.Plan(it, 0, -1, 3, 600, false)
	u, _ = url.Parse(p.URL)
	if u.Query().Get("SubtitleMethod") != "Encode" || p.SubURL != "" {
		t.Errorf("ffplay subtitles: %s", p.URL)
	}
}

// A whole play: the plan, the relay, the player, the reports, and the
// token in none of the URLs or the log.
func TestPlayEndToEnd(t *testing.T) {
	f := newFakeJellyfin(t)
	a, _ := testApp(t, f)
	var logs bytes.Buffer
	log.SetOutput(&logs)
	defer log.SetOutput(os.Stderr)
	urlFile := filepath.Join(a.dir, "url")
	a.pl.own = script(t, a.dir, "minifin-player", `
while [ $# -gt 0 ]; do case "$1" in --url) url=$2;; --sub) sub=$2;; esac; shift; done
echo "$url" > `+urlFile+`
[ -n "$sub" ] && cat "$sub" > `+urlFile+`.srt
echo "pos 2712"
echo "pos 2713"
echo "quit 2790"
`)
	it, _ := a.c.Item("m1")
	a.push(newDetail(a, it))
	a.play(it, 2712, 2, 3)
	b, _ := os.ReadFile(urlFile)
	local := strings.TrimSpace(string(b))
	if !strings.HasPrefix(local, "http://127.0.0.1:") || !strings.Contains(local, "/v/") {
		t.Fatalf("the player got %q", local)
	}
	if srt, _ := os.ReadFile(urlFile + ".srt"); !strings.Contains(string(srt), "The fog came in") {
		t.Fatalf("subtitles %q", srt)
	}
	if !strings.Contains(a.played, "minifin-player") || a.playedID != "m1" {
		t.Fatalf("note %q", a.played)
	}
	f.mu.Lock()
	var stopped map[string]any
	for _, r := range f.reports {
		if r.Path == "/Sessions/Playing/Stopped" {
			stopped = r.Body
		}
	}
	reqs := strings.Join(f.requests, "\n")
	f.mu.Unlock()
	if stopped == nil || stopped["PositionTicks"].(float64) != 2790*ticksPerSecond {
		t.Fatalf("stopped %v", stopped)
	}
	if strings.Contains(reqs, f.token) || strings.Contains(logs.String(), f.token) {
		t.Fatal("the token leaked into a URL or the log")
	}
	// the relay's URL is gone once the video ends
	resp, err := http.Get(local)
	if err == nil {
		resp.Body.Close()
		if resp.StatusCode != http.StatusNotFound {
			t.Fatalf("old relay URL still answers %d", resp.StatusCode)
		}
	}
}

// The relay passes the stream through with the header the server needs.
func TestRelayStreamsWithAuth(t *testing.T) {
	f := newFakeJellyfin(t)
	testEnv(t)
	c := NewClient(f.URL(), f.token, "user-1", "minifin-test")
	r, err := startRelay(c.Stream, c.AuthHeader)
	if err != nil {
		t.Fatal(err)
	}
	defer r.close()
	local := r.Stream(f.URL()+"/Videos/m1/stream.ts?StartTimeTicks=0", nil)
	resp, err := http.Get(local)
	if err != nil {
		t.Fatal(err)
	}
	b, _ := io.ReadAll(resp.Body)
	resp.Body.Close()
	if resp.StatusCode != 200 || resp.Header.Get("Content-Type") != "video/mp2t" || !bytes.Equal(b, makeTS(0, f.runtime)) {
		t.Fatalf("HTTP %d, %d bytes, %s", resp.StatusCode, len(b), resp.Header.Get("Content-Type"))
	}
	if resp, _ := http.Get("http://" + r.addr + "/v/unknown.ts"); resp.StatusCode != 404 {
		t.Fatalf("unknown id: %d", resp.StatusCode)
	}
}

// "stream ended early" is an error, not the end: the place is kept and
// ffplay isn't tried, since the player did play.
func TestPlayerEndedEarly(t *testing.T) {
	dir := t.TempDir()
	own := script(t, dir, "minifin-player", `echo "dur 7920.0"
echo "decoder sw"
echo "pos 100.0"
echo "pos 101.0"
echo "error stream ended early at 101.0 of 7920.0 s"
exit 1
`)
	pl := &players{own: own, ffplay: "/nonexistent", startWait: 4 * time.Second, killWait: time.Second}
	out := pl.play(context.Background(), playRequest{URL: "http://x/v/1.ts", Start: 100, Runtime: 7920}, make(chan ui.Event), func(float64, bool) {})
	if out.Ended || out.Player != "minifin-player" || !strings.Contains(out.Err, "ended early") || out.Pos != 101 {
		t.Fatalf("outcome %+v", out)
	}
	if got := describePlayer(out); got != "Played with minifin-player, software decoding." {
		t.Fatalf("note %q", got)
	}
	st := &playerState{}
	st.line("end")
	st.line("error stream ended early at 5.0 of 60.0 s")
	if st.Ended {
		t.Fatal("an error after end still counts as the end")
	}
}

// Stopping a player: SIGTERM first, and SIGKILL only if it is still
// there killWait later (kill -9 mid-video can wedge the device's MI
// drivers).
func TestPlayerStopOrder(t *testing.T) {
	dir := t.TempDir()
	signals := filepath.Join(dir, "signals")
	stubborn := script(t, dir, "stubborn-player", `trap 'echo TERM >> `+signals+`' TERM
echo "pos 10.0"
while :; do sleep 0.05; done
`)
	polite := script(t, dir, "polite-player", `trap 'echo TERM >> `+signals+`; echo "quit 11.0"; exit 0' TERM
echo "pos 10.0"
while :; do sleep 0.05; done
`)
	for _, c := range []struct {
		player string
		killed bool
	}{{stubborn, true}, {polite, false}} {
		os.Remove(signals)
		pl := &players{own: c.player, ffplay: "/nonexistent", startWait: 50 * time.Millisecond, killWait: 1500 * time.Millisecond}
		ctx, cancel := context.WithCancel(context.Background())
		seen := make(chan struct{}, 1)
		go func() {
			<-seen
			cancel()
		}()
		start := time.Now()
		out := pl.play(ctx, playRequest{URL: "http://x/v/1.ts", Runtime: 60}, make(chan ui.Event), func(pos float64, _ bool) {
			select {
			case seen <- struct{}{}:
			default:
			}
		})
		took := time.Since(start)
		b, _ := os.ReadFile(signals)
		if !strings.HasPrefix(string(b), "TERM") {
			t.Fatalf("%s: no SIGTERM first (%q)", filepath.Base(c.player), b)
		}
		if c.killed && took < 1500*time.Millisecond {
			t.Fatalf("stubborn player killed after %v, before the wait", took)
		}
		// the polite one leaves at once (plus MiniFin's 300 ms of ignoring
		// the buttons after a player)
		if !c.killed && (took > time.Second || out.Pos != 11) {
			t.Fatalf("polite player: %v, %+v", took, out)
		}
		if took > 4*time.Second {
			t.Fatalf("%s: stopping took %v", filepath.Base(c.player), took)
		}
		if out.Player != "minifin-player" {
			t.Fatalf("fell back to %s after a stop", out.Player)
		}
	}
}

// minifin-player loads libpadsp itself: no LD_PRELOAD for it.
func TestPlayEnv(t *testing.T) {
	t.Setenv("LD_PRELOAD", "/somewhere/else.so")
	t.Setenv("SDL_VIDEODRIVER", "dummy")
	for _, kv := range playEnv(false) {
		if strings.HasPrefix(kv, "LD_PRELOAD=") || strings.HasPrefix(kv, "SDL_VIDEODRIVER=") {
			t.Fatalf("minifin-player gets %s", kv)
		}
	}
	found := false
	for _, kv := range playEnv(true) {
		found = found || strings.HasPrefix(kv, "LD_LIBRARY_PATH=/lib:/config/lib:")
	}
	if !found {
		t.Fatal("no Onion library path")
	}
}
