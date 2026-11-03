package main

import (
	"errors"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/zakijss/minideck/ui"
)

// shoot draws page p (after its data and pictures have loaded) to
// DIR/name.png.
func shoot(t *testing.T, a *app, dir, name string, draw func()) {
	t.Helper()
	t.Setenv("MINIDECK_UI_PNG", filepath.Join(dir, name+".png"))
	t.Setenv("MINIDECK_UI_NULL", "")
	s, err := ui.Open()
	if err != nil {
		t.Fatal(err)
	}
	a.scr = s
	// draw until the pictures it asks for are in
	for i := 0; i < 100; i++ {
		t.Setenv("MINIDECK_UI_PNG", os.DevNull)
		draw()
		time.Sleep(15 * time.Millisecond)
		for len(a.done) > 0 {
			(<-a.done)()
		}
		if a.img.idle() && i > 2 {
			break
		}
	}
	draw()
	s.Present()
}

// settle runs background results until the page stops loading.
func settle(t *testing.T, a *app, p page) {
	t.Helper()
	a.pump(t, func() bool { return !p.busy() && len(a.done) == 0 })
	time.Sleep(30 * time.Millisecond)
	for len(a.done) > 0 {
		(<-a.done)()
	}
}

// TestScreens draws every screen to PNG files in DIR for review. It runs
// only with MINIDECK_GALLERY=DIR set.
func TestScreens(t *testing.T) {
	dir := os.Getenv("MINIDECK_GALLERY")
	if dir == "" {
		t.Skip("set MINIDECK_GALLERY=DIR to draw the screens")
	}
	if err := os.MkdirAll(dir, 0o755); err != nil {
		t.Fatal(err)
	}
	f := newFakeJellyfin(t)
	a, _ := testApp(t, f)
	page := func(name string, p page) {
		a.stack = []page{p}
		if p.busy() {
			settle(t, a, p)
		}
		shoot(t, a, dir, name, func() { p.draw(a) })
	}

	// setup
	servers := []Found{
		{Address: "http://192.168.0.21:8096", ID: "srv-0001", Name: "Jellyfin on homeserver", Where: "Wi-Fi"},
		{Address: "https://homeserver.tail1234.ts.net:8084", ID: "srv-0001", Name: "Jellyfin on homeserver", Where: "tailnet"},
	}
	var l ui.List
	shoot(t, a, dir, "minifin-welcome", func() { a.drawServers(&l, servers, 0, true) })
	shoot(t, a, dir, "minifin-welcome-searching", func() { a.drawServers(&l, nil, 2, true) })
	l = ui.List{}
	info := PublicInfo{ServerName: "Jellyfin on homeserver", Version: "12.4.0", ID: "srv-0001"}
	shoot(t, a, dir, "minifin-sign-in", func() { a.drawSignIn(&l, info, f.URL(), true, nil) })
	shoot(t, a, dir, "minifin-sign-in-no-qc", func() { a.drawSignIn(&l, info, f.URL(), false, nil) })
	qc := QuickConnect{Code: "482915", Secret: "s"}
	shoot(t, a, dir, "minifin-quick-connect", func() { a.drawQuickConnect("Jellyfin on homeserver", qc, "", false, true) })
	shoot(t, a, dir, "minifin-quick-connect-expired", func() { a.drawQuickConnect("Jellyfin on homeserver", qc, "", true, false) })

	// browsing
	h := newHome(a)
	page("minifin-home", h)
	h.row, h.rails[railResume].sel = 1, 0
	h.rails[railNextUp].sel = 0
	shoot(t, a, dir, "minifin-home-next-up", func() { h.draw(a) })
	h.row = 3
	shoot(t, a, dir, "minifin-home-libraries", func() { h.draw(a) })
	loading := &homePage{rails: []*rail{{title: "Continue watching", loading: true}, {title: "Next up", loading: true},
		{title: "Recently added", loading: true}, {title: "Libraries", always: true, loading: true}}}
	shoot(t, a, dir, "minifin-home-loading", func() { loading.draw(a) })

	f.setLibrary(append(bigLibrary(140), sampleMovies()...))
	lib := newLibrary(a, Item{ID: "lib-movies", Name: "Movies", CollectionType: "movies"})
	page("minifin-library", lib)
	lib.sel = 8
	shoot(t, a, dir, "minifin-library-selected", func() { lib.draw(a) })
	f.setLibrary(sampleMovies())
	small := newLibrary(a, Item{ID: "lib-movies", Name: "Movies", CollectionType: "movies"})
	page("minifin-library-small", small)
	small.sel = 3
	shoot(t, a, dir, "minifin-library-text-poster", func() { small.draw(a) })

	m := newDetail(a, f.byID["m1"])
	page("minifin-movie", m)
	m.list.Sel = 2
	a.playedID, a.played = "m1", "Played with ffplay: minifin-player isn't installed."
	shoot(t, a, dir, "minifin-movie-played", func() { m.draw(a) })
	a.played = ""
	ep := newDetail(a, f.byID["ep3"])
	page("minifin-episode", ep)
	sh := newDetail(a, f.byID["sh1"])
	page("minifin-show", sh)
	se := newSeason(a, f.byID["se2"])
	page("minifin-season", se)
	sp := &searchPage{query: "orbit", th: 44}
	sp.search(a)
	f.setLibrary(append(bigLibrary(30), sampleMovies()...))
	sp.query = "a"
	sp.search(a)
	page("minifin-search", sp)
	page("minifin-overview", &textPage{title: "The Night Harbor", text: f.byID["m1"].Overview + " " + f.byID["m1"].Overview + " " + f.byID["m1"].Overview})

	// downloads, in every state
	go a.dl.Run()
	a.dl.Add(shortItem())
	waitState(t, a.dl, "m3", dlDone)
	a.dl.Stop() // the states below stay as set
	a.dl.mu.Lock()
	for i, it := range []Item{f.byID["m4"], f.byID["ep2"], f.byID["m2"], f.byID["ep1"]} {
		st := []string{dlRunning, dlQueued, dlPaused, dlFailed}[i]
		dd := filepath.Join(a.dl.dir, it.ID)
		os.MkdirAll(dd, 0755)
		dl := &Download{ID: it.ID, Item: it, Kbps: 1200, State: st, Need: 420 << 20, dir: dd,
			Added: time.Now().Add(time.Duration(i) * time.Minute)}
		switch st {
		case dlRunning:
			dl.Parts = []dlPart{{Start90: 0, FirstPTS: 0, EndPTS: int64(it.Runtime() * 0.42 * ptsHz), Size: 176 << 20}}
		case dlPaused:
			dl.Reason = errNoSpace.Error()
		case dlFailed:
			dl.Reason = "the server said HTTP 500"
		}
		a.dl.list[it.ID] = dl
	}
	a.dl.mu.Unlock()
	a.dl.SetPosition("m3", 50, false)
	page("minifin-downloads", newDownloadsPage(false))
	a.offline = true
	page("minifin-offline", newDownloadsPage(true))
	a.offline = false
	empty := &downloads{list: map[string]*Download{}}
	keep := a.dl
	a.dl = empty
	page("minifin-downloads-empty", newDownloadsPage(false))
	a.dl = keep

	page("minifin-settings", newSettingsPage())

	next := f.byID["ep3"]
	next.Overview = "A storm knocks the relay mast down, and the station has one night to get a signal across the bay before the network notices."
	shoot(t, a, dir, "minifin-up-next", func() { a.drawNext(next, 7300*time.Millisecond) })

	// messages
	shoot(t, a, dir, "minifin-starting", func() { a.scr.Alert("Starting the video", "Asking Jellyfin on homeserver to prepare it…") })
	shoot(t, a, dir, "minifin-signed-out", func() { a.scr.Alert("Signed out", errSignedOut.Error(), "OK") })
	shoot(t, a, dir, "minifin-no-space", func() {
		a.scr.Alert("Can't download it", errText(errors.New("it needs about 1.3 GB and the card has 420 MB free. Free some space or pick a lower quality")), "OK")
	})
}
