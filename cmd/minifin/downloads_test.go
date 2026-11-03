package main

import (
	"bytes"
	"io"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/zakijss/minideck/ui"
)

// testDownloads is a download manager on f, in a test folder, with all the
// room in the world unless free says otherwise.
func testDownloads(t *testing.T, f *fakeJellyfin) (*downloads, string) {
	t.Helper()
	testEnv(t)
	dir := t.TempDir()
	c := NewClient(f.URL(), f.token, "user-1", "minifin-test")
	d := openDownloads(dir, c, func() int { return 1200 })
	d.free = func(string) int64 { return 50 << 30 }
	d.retry = func(int) time.Duration { return 10 * time.Millisecond }
	return d, dir
}

// waitState runs until the download reaches one of the states.
func waitState(t *testing.T, d *downloads, id string, states ...string) Download {
	t.Helper()
	deadline := time.Now().Add(20 * time.Second)
	for time.Now().Before(deadline) {
		if dl, ok := d.Get(id); ok {
			for _, s := range states {
				if dl.State == s {
					return dl
				}
			}
		}
		time.Sleep(10 * time.Millisecond)
	}
	dl, _ := d.Get(id)
	t.Fatalf("download %s is %s (%s), not %v", id, dl.State, dl.Reason, states)
	return dl
}

// shortItem is a two-minute video with a text subtitle.
func shortItem() Item {
	return Item{ID: "m3", Name: "Low Orbit", Type: "Movie", RunTimeTicks: 120 * ticksPerSecond,
		MediaSources: []MediaSource{{ID: "ms-m3", MediaStreams: []MediaStream{
			{Type: "Video", Index: 0}, {Type: "Audio", Index: 1},
			{Type: "Subtitle", Index: 2, Codec: "srt", DisplayTitle: "English", IsTextSubtitleStream: true},
			{Type: "Subtitle", Index: 3, Codec: "pgssub", DisplayTitle: "English (PGS)"}}}}}
}

// A whole download: one part, the text subtitle beside it, and it plays
// back byte for byte.
func TestDownloadWhole(t *testing.T) {
	f := newFakeJellyfin(t)
	d, _ := testDownloads(t, f)
	go d.Run()
	defer d.Stop()
	if err := d.Add(shortItem()); err != nil {
		t.Fatal(err)
	}
	dl := waitState(t, d, "m3", dlDone)
	if len(dl.Parts) != 1 || !dl.Parts[0].Done || dl.Covered() < 118 {
		t.Fatalf("parts %+v, covered %.1f", dl.Parts, dl.Covered())
	}
	if len(dl.Subs) != 1 || dl.Subs[0].Index != 2 {
		t.Fatalf("subtitles kept: %+v (an image track can't be)", dl.Subs)
	}
	if b, err := os.ReadFile(filepath.Join(dl.dir, dl.Subs[0].File)); err != nil || !strings.Contains(string(b), "fog") {
		t.Fatalf("subtitle file %q %v", b, err)
	}
	ls, at, err := openLocal(dl, 0)
	if err != nil || at != 0 {
		t.Fatalf("open: %v at %v", err, at)
	}
	defer ls.Close()
	got, _ := io.ReadAll(io.NewSectionReader(ls, ls.from, ls.size-ls.from))
	if !bytes.Equal(got, makeTS(0, 120)) {
		t.Fatalf("plays back %d bytes, not the %d downloaded", len(got), len(makeTS(0, 120)))
	}
	// from the middle: the keyframe before 61 s
	ls2, at, _ := openLocal(dl, 61)
	defer ls2.Close()
	if at < 58 || at > 61 {
		t.Fatalf("starts at %.1f for 61", at)
	}
	// it's still there after a restart
	d2 := openDownloads(filepath.Dir(dl.dir), d.c, d.kbps)
	if got, ok := d2.Get("m3"); !ok || got.State != dlDone || got.Bytes() != dl.Bytes() {
		t.Fatalf("after reopening: %+v", got)
	}
	// and it can be deleted
	d.Remove("m3")
	if _, err := os.Stat(dl.dir); !os.IsNotExist(err) {
		t.Fatal("the folder is still there")
	}
}

// An interrupted download goes on from its last keyframe, and its parts
// play as one stream with no jump in the timestamps.
func TestDownloadResume(t *testing.T) {
	f := newFakeJellyfin(t)
	f.streamCut = int64(len(makeTS(0, 120)) * 2 / 5)
	d, _ := testDownloads(t, f)
	go d.Run()
	defer d.Stop()
	d.Add(shortItem())
	dl := waitState(t, d, "m3", dlDone)
	if len(dl.Parts) != 2 {
		t.Fatalf("%d parts", len(dl.Parts))
	}
	p0, p1 := dl.Parts[0], dl.Parts[1]
	if p0.Done || p0.Size%tsPacket != 0 || p1.Start90 != p0.end90() || p1.Start90 <= 0 {
		t.Fatalf("parts %+v %+v", p0, p1)
	}
	f.mu.Lock()
	second := f.streams[len(f.streams)-1].Query["StartTimeTicks"]
	f.mu.Unlock()
	if want := float64(p1.Start90) / ptsHz; second != strconv.FormatInt(toTicks(want), 10) {
		t.Fatalf("asked again from %s, the part starts at %.3f", second, want)
	}
	if dl.Covered() < 118 {
		t.Fatalf("covered %.1f", dl.Covered())
	}
	ls, _, err := openLocal(dl, 0)
	if err != nil {
		t.Fatal(err)
	}
	defer ls.Close()
	all, _ := io.ReadAll(io.NewSectionReader(ls, 0, ls.size))
	pts := videoPTS(all)
	for i := 1; i < len(pts); i++ {
		if step := ptsDiff(pts[i], pts[i-1]); step <= 0 || step > ptsHz {
			t.Fatalf("timestamp jump at frame %d: %d -> %d", i, pts[i-1], pts[i])
		}
	}
	if span := float64(ptsDiff(pts[len(pts)-1], pts[0])) / ptsHz; span < 118 {
		t.Fatalf("the stream spans %.1f s", span)
	}
}

// No room: Add refuses with the sizes, and a card filling up mid-way
// pauses the download with the reason.
func TestDownloadNoSpace(t *testing.T) {
	f := newFakeJellyfin(t)
	d, _ := testDownloads(t, f)
	d.free = func(string) int64 { return 300 << 20 }
	big := f.byID["m1"]
	err := d.Add(big)
	if err == nil || !strings.Contains(err.Error(), "needs about 1.3 GB") || !strings.Contains(err.Error(), "100 MB free") {
		t.Fatalf("Add: %v", err)
	}
	if _, ok := d.Get("m1"); ok {
		t.Fatal("listed anyway")
	}
	// room at the start, then the card fills
	var free int64 = 10 << 30
	d.free = func(string) int64 { return free }
	f.runtime = 600
	it := shortItem()
	it.RunTimeTicks = 600 * ticksPerSecond
	if err := d.Add(it); err != nil {
		t.Fatal(err)
	}
	free = 1 << 20
	go d.Run()
	defer d.Stop()
	dl := waitState(t, d, "m3", dlPaused)
	if !strings.Contains(dl.Reason, "card is full") {
		t.Fatalf("reason %q", dl.Reason)
	}
	// room again: Y (Toggle) goes on
	free = 10 << 30
	d.Toggle("m3")
	waitState(t, d, "m3", dlDone)
}

// A damaged meta.json gives an entry that can be deleted; a part cut short
// on the card (pulled mid-write) is cut back to a keyframe and goes on.
func TestDownloadCorrupt(t *testing.T) {
	f := newFakeJellyfin(t)
	d, dir := testDownloads(t, f)
	go d.Run()
	d.Add(shortItem())
	dl := waitState(t, d, "m3", dlDone)
	d.Stop()

	// truncate the part to 30 % of its size
	part := filepath.Join(dl.dir, dl.Parts[0].File)
	os.Truncate(part, dl.Parts[0].Size*3/10)
	// and a second download whose meta.json is junk
	os.MkdirAll(filepath.Join(dir, "broken"), 0755)
	os.WriteFile(filepath.Join(dir, "broken", "meta.json"), []byte("{not json"), 0644)

	d2 := openDownloads(dir, d.c, d.kbps)
	d2.free, d2.retry = d.free, d.retry
	got, _ := d2.Get("m3")
	if got.State != dlQueued || len(got.Parts) != 1 || got.Parts[0].Done || got.Covered() > 40 || got.Covered() < 20 {
		t.Fatalf("cut back: %s, covered %.1f, %+v", got.State, got.Covered(), got.Parts)
	}
	if fi, _ := os.Stat(part); fi.Size() != got.Parts[0].Size {
		t.Fatalf("file %d bytes, meta %d", fi.Size(), got.Parts[0].Size)
	}
	b, _ := d2.Get("broken")
	if b.State != dlDamaged || !strings.Contains(dlStatus(b), "damaged") {
		t.Fatalf("broken: %+v", b)
	}
	go d2.Run()
	defer d2.Stop()
	fixed := waitState(t, d2, "m3", dlDone)
	if len(fixed.Parts) != 2 || fixed.Covered() < 118 {
		t.Fatalf("after going on: %+v", fixed.Parts)
	}
	d2.Remove("broken")
	if _, err := os.Stat(filepath.Join(dir, "broken")); !os.IsNotExist(err) {
		t.Fatal("damaged entry not deleted")
	}
}

// Offline: Home can't reach the server and shows the downloads; one plays
// from the card, its position is kept and sent once the server is back.
func TestOfflineMode(t *testing.T) {
	f := newFakeJellyfin(t)
	a, in := testApp(t, f)
	go a.dl.Run()
	a.dl.Add(shortItem())
	waitState(t, a.dl, "m3", dlDone)

	a.c.SetLogin("http://127.0.0.1:1", f.token, "user-1") // the server is gone
	a.stack = nil
	a.push(newHome(a))
	a.pump(t, func() bool { _, ok := a.top().(*downloadsPage); return ok })
	if !a.offline {
		t.Fatal("not offline")
	}
	p := a.top().(*downloadsPage)
	p.draw(a)
	if len(p.list.Items) != 1 || !strings.HasPrefix(p.list.Items[0].Sub, "Ready") {
		t.Fatalf("offline list %+v", p.list.Items)
	}
	a.pl.own = script(t, a.dir, "minifin-player", "echo 'pos 40'\necho 'quit 45'\n")
	press(a, ui.A)
	dl, _ := a.dl.Get("m3")
	if dl.Position != 45 {
		t.Fatalf("position kept %.1f", dl.Position)
	}
	if e := a.pend.load()["m3"]; e.Pos != 45 {
		t.Fatalf("pending %+v", a.pend.load())
	}
	// a video not on the card can't play: a notice, which B closes
	in <- ui.Event{Button: ui.B, Pressed: true}
	a.play(f.byID["m4"], 0, -1, -1)
	if len(in) != 0 {
		t.Fatal("no notice for a video that isn't on the card")
	}
	// back online: the position goes to the server
	a.c.SetLogin(f.URL(), f.token, "user-1")
	a.pend.flush(a.c)
	f.mu.Lock()
	ud := f.userData["m3"]
	f.mu.Unlock()
	if ud == nil || ud["PlaybackPositionTicks"].(float64) != 45*ticksPerSecond {
		t.Fatalf("server got %v", ud)
	}
	if len(a.pend.load()) != 0 {
		t.Fatal("pending not cleared")
	}
}
