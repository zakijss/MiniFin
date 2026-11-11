package main

import (
	"bytes"
	"context"
	"encoding/json"
	"log"
	"net"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/zakijss/minideck/secrets"
	"github.com/zakijss/minideck/ui"
)

// testEnv points MiniFin at a test folder: no device key, proxies or
// framebuffer of the real device.
func testEnv(t testing.TB) string {
	t.Helper()
	dir := t.TempDir()
	t.Setenv("MINIDECK_DEVKEY", filepath.Join(dir, "devkey"))
	t.Setenv("MINIDECK_SOCKS", "off")
	t.Setenv("MINIDECK_HTTP_PROXY", "off")
	t.Setenv("MINIDECK_UI_NULL", "1")
	t.Setenv("MINIDECK_PEERS_JSON", filepath.Join(dir, "no-peers.json"))
	t.Setenv("MINIFIN_PLAYER", filepath.Join(dir, "no-player"))
	t.Setenv("MINIDECK_FFPLAY", filepath.Join(dir, "no-ffplay"))
	return dir
}

// testApp is an app signed in to f, drawing to nowhere. input is its
// buttons.
func testApp(t testing.TB, f *fakeJellyfin) (*app, chan ui.Event) {
	t.Helper()
	dir := testEnv(t)
	cfg := Settings{Server: f.URL(), ServerName: "Jellyfin on homeserver", ServerID: "srv-0001", UserID: "user-1", UserName: "zaki", DeviceID: "minifin-test"}
	if err := saveSettings(dir, &cfg); err != nil {
		t.Fatal(err)
	}
	if err := secrets.Save(filepath.Join(dir, "secrets.enc"), map[string]string{"token": f.token}); err != nil {
		t.Fatal(err)
	}
	scr, err := ui.Open()
	if err != nil {
		t.Fatal(err)
	}
	in := make(chan ui.Event, 64)
	a := newApp(scr, in, dir)
	t.Cleanup(a.close)
	return a, in
}

// pump runs the event loop's background part, drawing the top page each
// time, until cond holds.
func (a *app) pump(t testing.TB, cond func() bool) {
	t.Helper()
	deadline := time.After(10 * time.Second)
	for !cond() {
		if p := a.top(); p != nil {
			p.draw(a)
		}
		select {
		case apply := <-a.done:
			apply()
		case <-a.redraw:
		case <-time.After(20 * time.Millisecond):
		case <-deadline:
			t.Fatal("timed out waiting")
		}
	}
	if p := a.top(); p != nil {
		p.draw(a)
	}
}

func press(a *app, b ui.Button) { a.press(ui.Event{Button: b, Pressed: true}) }

// The Authorization header is the one Jellyfin 12 asks for, token last.
func TestAuthHeader(t *testing.T) {
	got := authHeader("minifin-abc", "tok1")
	want := `MediaBrowser Client="MiniFin", Device="Miyoo Mini Plus", DeviceId="minifin-abc", Version="dev", Token="tok1"`
	if got != want {
		t.Fatalf("header\n got %s\nwant %s", got, want)
	}
	if h := authHeader("minifin-abc", ""); strings.Contains(h, "Token") {
		t.Fatalf("a header without a token names one: %s", h)
	}
	if h := authHeader(`a"b`, "t\r\nX: y"); strings.Count(h, `"`) != 10 || strings.ContainsAny(h, "\r\n") {
		t.Fatalf("a value broke out of its quotes: %q", h)
	}
	if !authRE.MatchString(got) {
		t.Fatal("the fake server wouldn't take the header")
	}
}

// Every request carries the header, and the token is never in a URL.
func TestTokenOnlyInHeader(t *testing.T) {
	f := newFakeJellyfin(t)
	a, _ := testApp(t, f)
	var logs bytes.Buffer
	log.SetOutput(&logs)
	defer log.SetOutput(os.Stderr)
	h := newHome(a)
	a.push(h)
	a.pump(t, func() bool { return !h.busy() && !h.rails[railLatest].loading && !h.rails[railLibraries].loading })
	it, _ := a.c.Item("m1")
	plan, err := a.c.Plan(it, 60, -1, -1, 1200, true)
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(plan.URL, f.token) || strings.Contains(strings.ToLower(plan.URL), "api_key") {
		t.Fatalf("the stream URL carries the token: %s", plan.URL)
	}
	f.mu.Lock()
	defer f.mu.Unlock()
	for i, r := range f.requests {
		if strings.Contains(r, f.token) {
			t.Errorf("request %s carries the token", r)
		}
		if !authRE.MatchString(f.auths[i]) {
			t.Errorf("request %s: header %q", r, f.auths[i])
		}
	}
	if strings.Contains(logs.String(), f.token) {
		t.Fatalf("the token is in the log:\n%s", logs.String())
	}
}

// Quick Connect: a code, polls until it is approved, then a token.
func TestQuickConnectFlow(t *testing.T) {
	f := newFakeJellyfin(t)
	testEnv(t)
	c := NewClient(f.URL(), "", "", "minifin-test")
	on, err := c.QuickConnectEnabled()
	if err != nil || !on {
		t.Fatalf("enabled: %v %v", on, err)
	}
	qc, err := c.QuickConnectInitiate()
	if err != nil || qc.Code != "482915" || qc.Secret == "" {
		t.Fatalf("initiate: %+v %v", qc, err)
	}
	st, err := c.QuickConnectState(qc.Secret)
	if err != nil || st.Authenticated {
		t.Fatalf("not approved yet, got %+v %v", st, err)
	}
	if _, err := c.AuthenticateQuickConnect(qc.Secret); err == nil {
		t.Fatal("signed in before the code was approved")
	}
	f.mu.Lock()
	f.approved = true
	f.mu.Unlock()
	st, err = c.QuickConnectState(qc.Secret)
	if err != nil || !st.Authenticated {
		t.Fatalf("approved: %+v %v", st, err)
	}
	ar, err := c.AuthenticateQuickConnect(qc.Secret)
	if err != nil || ar.AccessToken != f.token || ar.User.ID != "user-1" {
		t.Fatalf("auth: %+v %v", ar, err)
	}
	// an unknown secret: the code expired
	if _, err := c.QuickConnectState("old"); err != errCodeExpired {
		t.Fatalf("expired code: %v", err)
	}
	// turned off on the server
	f.qcOn = false
	if _, err := c.QuickConnectInitiate(); err == nil || !strings.Contains(err.Error(), "turned off") {
		t.Fatalf("off: %v", err)
	}
}

// The Quick Connect screen polls in the background and signs in once the
// code is approved; the screen keeps answering the buttons meanwhile.
func TestQuickConnectScreen(t *testing.T) {
	f := newFakeJellyfin(t)
	a, in := testApp(t, f)
	go func() {
		for {
			f.mu.Lock()
			polls := f.polls
			f.mu.Unlock()
			if polls >= 1 {
				f.mu.Lock()
				f.approved = true
				f.mu.Unlock()
				return
			}
			time.Sleep(50 * time.Millisecond)
		}
	}()
	_ = in
	c := a.newClient(f.URL())
	done := make(chan AuthResult, 1)
	go func() {
		ar, ok := a.quickConnect(c, "Jellyfin on homeserver")
		if ok {
			done <- ar
		}
		close(done)
	}()
	select {
	case ar := <-done:
		if ar.AccessToken != f.token {
			t.Fatalf("token %q", ar.AccessToken)
		}
	case <-time.After(15 * time.Second):
		t.Fatal("Quick Connect never finished")
	}
}

// Password sign-in, right and wrong.
func TestPasswordSignIn(t *testing.T) {
	f := newFakeJellyfin(t)
	testEnv(t)
	c := NewClient(f.URL(), "", "", "minifin-test")
	if _, err := c.AuthenticateByName("zaki", "wrong"); err == nil || err.Error() != "Wrong user name or password." {
		t.Fatalf("wrong password: %v", err)
	}
	ar, err := c.AuthenticateByName("zaki", "hunter2")
	if err != nil || ar.AccessToken != f.token {
		t.Fatalf("sign in: %+v %v", ar, err)
	}
}

// A token the server stopped taking is reported as signed out, not as a
// network error.
func TestSignedOut(t *testing.T) {
	f := newFakeJellyfin(t)
	testEnv(t)
	c := NewClient(f.URL(), "revoked", "user-1", "minifin-test")
	if _, err := c.Views(); err != errSignedOut {
		t.Fatalf("got %v", err)
	}
}

// Discovery replies are parsed, and junk is ignored.
func TestParseDiscovery(t *testing.T) {
	f, ok := parseDiscovery([]byte(`{"Address":"http://192.168.0.10:8096/","Id":"abc","Name":"homeserver","EndpointAddress":null}`))
	if !ok || f.Address != "http://192.168.0.10:8096" || f.Name != "homeserver" || f.ID != "abc" || f.Where != "Wi-Fi" {
		t.Fatalf("got %+v %v", f, ok)
	}
	for _, bad := range []string{``, `who is JellyfinServer?`, `{"Address":"","Id":"x"}`, `{"Address":"ftp://x","Id":"x"}`, `{"Address":"http://x"}`} {
		if _, ok := parseDiscovery([]byte(bad)); ok {
			t.Errorf("took %q", bad)
		}
	}
	if f, _ := parseDiscovery([]byte(`{"Address":"http://10.0.0.2:8096","Id":"y"}`)); f.Name != "http://10.0.0.2:8096" {
		t.Errorf("no name: %+v", f)
	}
}

// The broadcast gets answers from a server on the LAN (here, a fake one
// on loopback), each server once.
func TestDiscoverLAN(t *testing.T) {
	srv, err := net.ListenUDP("udp4", &net.UDPAddr{IP: net.IPv4(127, 0, 0, 1)})
	if err != nil {
		t.Fatal(err)
	}
	defer srv.Close()
	go func() {
		buf := make([]byte, 100)
		for {
			n, from, err := srv.ReadFromUDP(buf)
			if err != nil {
				return
			}
			if string(buf[:n]) == "who is JellyfinServer?" {
				srv.WriteToUDP([]byte(`{"Address":"http://192.168.0.10:8096","Id":"srv1","Name":"homeserver","EndpointAddress":null}`), from)
				srv.WriteToUDP([]byte(`not json`), from)
			}
		}
	}()
	old := discoveryAddr
	discoveryAddr = srv.LocalAddr().(*net.UDPAddr)
	defer func() { discoveryAddr = old }()
	ctx, cancel := context.WithTimeout(context.Background(), 1500*time.Millisecond)
	defer cancel()
	var got []Found
	if err := discoverLAN(ctx, func(f Found) { got = append(got, f) }); err != nil {
		t.Fatal(err)
	}
	if len(got) != 1 || got[0].Name != "homeserver" {
		t.Fatalf("found %+v", got)
	}
}

// Tailnet discovery tries Jellyfin's ports on each online device and
// reports the server once.
func TestDiscoverTailnet(t *testing.T) {
	f := newFakeJellyfin(t)
	dir := testEnv(t)
	peers := `{"Peer":{"a":{"HostName":"homeserver","DNSName":"homeserver.tail1234.ts.net.","Online":true},
		"b":{"HostName":"nas","DNSName":"nas.tail1234.ts.net.","Online":false},
		"c":{"HostName":"phone","DNSName":"phone.tail1234.ts.net.","Online":true}}}`
	os.WriteFile(filepath.Join(dir, "peers.json"), []byte(peers), 0644)
	t.Setenv("MINIDECK_PEERS_JSON", filepath.Join(dir, "peers.json"))
	ps, err := tailnetPeers()
	if err != nil || len(ps) != 3 {
		t.Fatalf("peers %+v %v", ps, err)
	}
	c := tailnetCandidates(ps[0])
	want := []string{"http://homeserver:8096", "https://homeserver.tail1234.ts.net", "https://homeserver.tail1234.ts.net:8920", "https://homeserver.tail1234.ts.net:8084"}
	if strings.Join(c, " ") != strings.Join(want, " ") {
		t.Fatalf("candidates %v", c)
	}
	// Only homeserver's 8084 answers (as with tailscale serve); every other
	// address fails fast.
	var mu sync.Mutex
	var tried []string
	newClient := func(base string) *Client {
		mu.Lock()
		tried = append(tried, base)
		mu.Unlock()
		if base == "https://homeserver.tail1234.ts.net:8084" || base == "http://homeserver:8096" {
			return NewClient(f.URL(), "", "", "minifin-test")
		}
		return NewClient("http://127.0.0.1:1", "", "", "minifin-test")
	}
	var found []Found
	discoverTailnet(context.Background(), newClient, func(x Found) {
		mu.Lock()
		found = append(found, x)
		mu.Unlock()
	})
	if len(found) != 1 || found[0].Name != "Jellyfin on homeserver" || found[0].Where != "tailnet" {
		t.Fatalf("found %+v", found)
	}
	for _, b := range tried {
		if strings.Contains(b, "nas") {
			t.Errorf("tried an offline device: %s", b)
		}
	}
}

// Home fills its rails from Continue Watching, Next Up, Recently Added
// and the libraries (music left out, Downloads added).
func TestHomeRails(t *testing.T) {
	f := newFakeJellyfin(t)
	a, _ := testApp(t, f)
	h := newHome(a)
	a.push(h)
	a.pump(t, func() bool {
		for _, r := range h.rails {
			if r.loading {
				return false
			}
		}
		return true
	})
	names := func(r *rail) []string {
		var out []string
		for _, c := range r.cards {
			out = append(out, c.name)
		}
		return out
	}
	if got := names(h.rails[railResume]); strings.Join(got, "|") != "The Night Harbor|Static Gardens S2:E2" {
		t.Errorf("continue watching %v", got)
	}
	if h.rails[railResume].cards[0].progress < 0.3 {
		t.Errorf("no progress on a resume card")
	}
	if got := names(h.rails[railNextUp]); len(got) != 1 || got[0] != "Static Gardens S2:E3" {
		t.Errorf("next up %v", got)
	}
	if got := names(h.rails[railLatest]); len(got) != 5 {
		t.Errorf("latest %v", got)
	}
	if got := names(h.rails[railLibraries]); strings.Join(got, "|") != "Movies|Shows|Downloads" {
		t.Errorf("libraries %v", got)
	}
	// moving around and opening a library
	press(a, ui.Down)
	press(a, ui.Down)
	press(a, ui.Down)
	press(a, ui.A)
	lib, ok := a.top().(*libraryPage)
	if !ok {
		t.Fatalf("opened %T", a.top())
	}
	a.pump(t, func() bool { return lib.total >= 0 })
	if lib.total != 5 || lib.q.Types != "Movie" || !lib.q.Recursive {
		t.Fatalf("library %v %+v", lib, lib.q)
	}
	press(a, ui.B)
	if a.top() != h {
		t.Fatal("B didn't go back")
	}
}

// A library pages through the server, keeps only the items near the
// selection, and jumps by letter with NameLessThan.
func TestLibraryPagingAndLetters(t *testing.T) {
	f := newFakeJellyfin(t)
	f.setLibrary(bigLibrary(2000))
	a, _ := testApp(t, f)
	lib := newLibrary(a, Item{ID: "lib-movies", Name: "Movies", CollectionType: "movies"})
	a.push(lib)
	a.pump(t, func() bool { return lib.total == 2000 && len(lib.loading) == 0 })
	if len(lib.items) > (libAhead+2)*libPage {
		t.Fatalf("fetched %d items up front", len(lib.items))
	}
	// R2 to the next letter: the server counts the items before it
	start := lib.letterAt(0)
	press(a, ui.R2)
	a.pump(t, func() bool { return !lib.jumping && len(lib.loading) == 0 })
	if got := lib.letterAt(lib.sel); got == start || got == "" {
		t.Fatalf("R2 from %s went to %q (sel %d)", start, got, lib.sel)
	}
	if prev := lib.letterAt(lib.sel - 1); prev == lib.letterAt(lib.sel) {
		t.Fatalf("R2 didn't land on the first item of %s", lib.letterAt(lib.sel))
	}
	cur := lib.letterAt(lib.sel)
	press(a, ui.R1)
	press(a, ui.L2) // back to the start of the letter
	a.pump(t, func() bool { return !lib.jumping && len(lib.loading) == 0 })
	if lib.letterAt(lib.sel) != cur || lib.letterAt(lib.sel-1) == cur {
		t.Fatalf("L2 went to %d (%s)", lib.sel, lib.letterAt(lib.sel))
	}
	// far into it
	for i := 0; i < 80; i++ {
		press(a, ui.R1)
	}
	a.pump(t, func() bool { _, ok := lib.items[lib.sel]; return ok && len(lib.loading) == 0 })
	if len(lib.items) > libKeep+libPage*(libAhead+2) {
		t.Fatalf("kept %d items", len(lib.items))
	}
	// sort and filter start over
	press(a, ui.Y)
	if lib.sel != 0 || lib.total != -1 {
		t.Fatalf("sort didn't reset: %v", lib)
	}
	a.pump(t, func() bool { return lib.total >= 0 })
	press(a, ui.X)
	a.pump(t, func() bool { return lib.total >= 0 })
	if !lib.q.Unplayed {
		t.Fatal("X didn't filter")
	}
}

// bigLibrary is n movies with names across the alphabet.
func bigLibrary(n int) []Item {
	words := []string{"Arrow", "Beacon", "Cinder", "Delta", "Echo", "Fable", "Granite", "Harbor", "Iris", "Juniper",
		"Kestrel", "Lumen", "Meridian", "Nimbus", "Orbit", "Prism", "Quartz", "Raven", "Signal", "Tundra",
		"Umbra", "Vertex", "Willow", "Xenon", "Yonder", "Zephyr", "7 Days"}
	items := make([]Item, n)
	for i := range items {
		w := words[i%len(words)]
		name := w + " " + thousands(i)
		items[i] = Item{ID: "big" + thousands(i), Name: name, SortName: strings.ToLower(name), Type: "Movie",
			ProductionYear: 1960 + i%60, RunTimeTicks: int64(80+i%60) * 60 * ticksPerSecond,
			ImageTags: map[string]string{"Primary": "t" + thousands(i)}, UserData: &UserData{Played: i%7 == 0}}
	}
	return items
}

// Series > seasons > episodes, and the item page's facts and tracks.
func TestShowPages(t *testing.T) {
	f := newFakeJellyfin(t)
	a, _ := testApp(t, f)
	a.push(newHome(a))
	a.open(f.byID["sh1"])
	d := a.top().(*detailPage)
	a.pump(t, func() bool { return !d.loading })
	rows := d.rows(a)
	if len(rows) != 3 || !strings.HasPrefix(rows[0].Label, "Next: S2:E3") || rows[2].Label != "Season 2" {
		t.Fatalf("show rows %+v", rows)
	}
	d.list.Sel = 2
	press(a, ui.A)
	s := a.top().(*seasonPage)
	a.pump(t, func() bool { return !s.loading })
	if len(s.items) != 3 || s.list.Sel != 1 { // opens at the first one not watched
		t.Fatalf("season: %d items, sel %d", len(s.items), s.list.Sel)
	}
	if got := s.list.Items[1].Label; got != "2. The Long Wave" {
		t.Fatalf("episode label %q", got)
	}
	if got := episodeSub(a, s.items[1]); got != "44 min · 29 min left" {
		t.Fatalf("episode sub %q", got)
	}
	// a movie's page: resume, tracks, download size
	m := newDetail(a, f.byID["m1"])
	a.push(m)
	a.pump(t, func() bool { return !m.loading })
	var labels []string
	for _, r := range m.rows(a) {
		labels = append(labels, r.Label+"="+r.Detail)
	}
	got := strings.Join(labels, "|")
	want := "Resume from 45:12=|Play from the start=|Audio=French - AAC Stereo|Subtitles=Off|Download=About 1.3 GB"
	if got != want {
		t.Fatalf("movie rows\n got %s\nwant %s", got, want)
	}
	m.change(a, actSubs, true)
	m.change(a, actSubs, true)
	if _, sub := m.tracks(a); sub != 4 {
		t.Fatalf("subtitle %d", sub)
	}
	if !strings.Contains(m.subName(a, m.subChoices(a)), "burned in") {
		t.Fatal("a PGS track isn't marked as burned in")
	}
	if metaLine(f.byID["m1"]) != "2019 · 2 h 12 min · PG-13 · 7.8/10" {
		t.Fatalf("meta %q", metaLine(f.byID["m1"]))
	}
	if metaLine(f.byID["sh1"]) != "2018-2021 · 3 seasons · 8.4/10" {
		t.Fatalf("show meta %q", metaLine(f.byID["sh1"]))
	}
}

// Track choice follows the setting and the server's defaults.
func TestPickStreams(t *testing.T) {
	it := sampleMovies()[0]
	def := 3
	it.MediaSources[0].DefaultSubtitleStreamIndex = &def
	if a, s := pickStreams(it, nil, nil, ""); a != 2 || s != 3 {
		t.Errorf("server's choice: %d %d", a, s)
	}
	if _, s := pickStreams(it, nil, nil, "off"); s != -1 {
		t.Errorf("off: %d", s)
	}
	it.MediaSources[0].DefaultSubtitleStreamIndex = nil
	if _, s := pickStreams(it, nil, nil, "on"); s != 3 {
		t.Errorf("on: %d", s)
	}
	one, none := 1, -1
	if a, s := pickStreams(it, &one, &none, "on"); a != 1 || s != -1 {
		t.Errorf("chosen: %d %d", a, s)
	}
}

// Search sends the words and lists what comes back.
func TestSearch(t *testing.T) {
	f := newFakeJellyfin(t)
	a, _ := testApp(t, f)
	p := &searchPage{query: "orbit", th: 44}
	p.search(a)
	a.push(p)
	a.pump(t, func() bool { return !p.loading })
	if len(p.items) != 1 || p.items[0].Name != "Low Orbit" || searchSub(p.items[0]) != "Movie · 2011" {
		t.Fatalf("results %+v", p.items)
	}
}

// Settings step through their values and are saved.
func TestSettings(t *testing.T) {
	f := newFakeJellyfin(t)
	a, _ := testApp(t, f)
	p := newSettingsPage()
	a.push(p)
	p.draw(a)
	find := func(label string) int {
		for i, it := range p.rows(a) {
			if it.Label == label {
				return i
			}
		}
		t.Fatalf("no %s row", label)
		return 0
	}
	p.list.Sel = find("Streaming quality")
	press(a, ui.Right)
	if a.cfg.kbps() != 2000 {
		t.Fatalf("quality %d", a.cfg.kbps())
	}
	press(a, ui.Right)
	if a.cfg.kbps() != 600 {
		t.Fatalf("quality wraps to %d", a.cfg.kbps())
	}
	p.list.Sel = find("Subtitles")
	press(a, ui.A)
	if a.cfg.Subtitles != "off" {
		t.Fatalf("subtitles %q", a.cfg.Subtitles)
	}
	p.list.Sel = find("Video player")
	press(a, ui.A)
	if a.pl.force != "ffplay" {
		t.Fatal("player setting not applied")
	}
	saved := loadSettings(a.dir)
	if saved.VideoKbps != 600 || saved.Subtitles != "off" || saved.Player != "ffplay" {
		t.Fatalf("saved %+v", saved)
	}
	b, _ := os.ReadFile(filepath.Join(a.dir, "settings.json"))
	if strings.Contains(string(b), f.token) {
		t.Fatal("the token is in settings.json")
	}
	if items := p.rows(a); items[len(items)-1].Label != "Appearance" {
		t.Fatal("Settings don't end with Appearance")
	}
}

// The token is sealed in secrets.enc, not readable on the card.
func TestTokenSealed(t *testing.T) {
	f := newFakeJellyfin(t)
	a, _ := testApp(t, f)
	ok := a.saveLogin(f.URL(), PublicInfo{ServerName: "Jellyfin on homeserver", ID: "srv-0001"}, AuthResult{User: User{Name: "zaki", ID: "user-1"}, AccessToken: "fresh-token-77"})
	if !ok {
		t.Fatal("not saved")
	}
	b, _ := os.ReadFile(filepath.Join(a.dir, "secrets.enc"))
	if bytes.Contains(b, []byte("fresh-token-77")) || !bytes.HasPrefix(b, []byte("Salted__")) {
		t.Fatal("secrets.enc isn't sealed")
	}
	m, err := secrets.Load(filepath.Join(a.dir, "secrets.enc"))
	if err != nil || m["token"] != "fresh-token-77" {
		t.Fatalf("reads back %v %v", m, err)
	}
	var cfg map[string]any
	b, _ = os.ReadFile(filepath.Join(a.dir, "settings.json"))
	json.Unmarshal(b, &cfg)
	if cfg["user_name"] != "zaki" || cfg["device_id"] == "" {
		t.Fatalf("settings %v", cfg)
	}
}

// Y on an episode and X on a video's page mark it watched (or not) on the
// server, clearing the resume point; offline nothing is sent.
func TestMarkWatched(t *testing.T) {
	f := newFakeJellyfin(t)
	a, _ := testApp(t, f)
	a.push(newHome(a))
	s := newSeason(a, f.byID["se2"])
	a.push(s)
	a.pump(t, func() bool { return !s.loading })
	sent := func(id string) map[string]any {
		f.mu.Lock()
		defer f.mu.Unlock()
		return f.userData[id]
	}
	s.list.Sel = 1 // ep2, half watched
	press(a, ui.Y)
	a.pump(t, func() bool { return sent("ep2") != nil })
	if d := sent("ep2"); d["Played"] != true || d["PlaybackPositionTicks"] != float64(0) {
		t.Fatalf("episode marked %v", d)
	}
	s.list.Sel = 0 // ep1, watched: marked unwatched
	press(a, ui.Y)
	a.pump(t, func() bool { return sent("ep1") != nil })
	if d := sent("ep1"); d["Played"] != false {
		t.Fatalf("watched episode marked %v", d)
	}
	m := newDetail(a, f.byID["m3"])
	a.push(m)
	a.pump(t, func() bool { return !m.loading })
	press(a, ui.X)
	a.pump(t, func() bool { return sent("m3") != nil })
	if d := sent("m3"); d["Played"] != true {
		t.Fatalf("movie marked %v", d)
	}
	a.offline = true
	m4 := newDetail(a, f.byID["m4"])
	a.push(m4)
	press(a, ui.X)
	time.Sleep(50 * time.Millisecond)
	if sent("m4") != nil {
		t.Fatal("marked while offline")
	}
}
