// MiniFin plays movies and shows from a Jellyfin server on a Miyoo Mini
// Plus: it finds the server on the Wi-Fi or the tailnet, signs in with
// Quick Connect, browses the libraries and plays through a local relay in
// minifin-player (or Onion's ffplay). Videos can be downloaded to the card
// and watched offline. Music stays in MiniTape.
package main

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"errors"
	"log"
	"os"
	"os/signal"
	"path/filepath"
	"strings"
	"sync"
	"syscall"
	"time"

	"github.com/zakijss/minideck/appdir"
	"github.com/zakijss/minideck/atomicfile"
	"github.com/zakijss/minideck/cache"
	"github.com/zakijss/minideck/secrets"
	"github.com/zakijss/minideck/ui"
	"github.com/zakijss/minideck/version"
)

// Settings is settings.json. The token is not here: it is in secrets.enc,
// sealed with the device key.
type Settings struct {
	Server     string `json:"server,omitempty"` // the server's address
	ServerName string `json:"server_name,omitempty"`
	ServerID   string `json:"server_id,omitempty"`
	UserID     string `json:"user_id,omitempty"`
	UserName   string `json:"user_name,omitempty"`
	// DeviceID names this handheld to the server, the same every time, so
	// it shows as one device and a new sign-in replaces the old session.
	DeviceID string `json:"device_id,omitempty"`
	// VideoKbps is the streaming quality: the video's bit rate in kbit/s
	// (600, 1200 or 2000; 0 means 1200).
	VideoKbps int `json:"video_kbps,omitempty"`
	// Subtitles is "" (the server's choice, from the user's Jellyfin
	// settings), "off" or "on".
	Subtitles string `json:"subtitles,omitempty"`
	// Downloads is "" (the app's folder) or "videos" (Videos/MiniFin on
	// the card).
	Downloads string `json:"downloads,omitempty"`
	// Player is "" (minifin-player when installed) or "ffplay".
	Player string `json:"player,omitempty"`
}

func (s Settings) kbps() int {
	for _, r := range videoRates {
		if r == s.VideoKbps {
			return r
		}
	}
	return 1200
}

// downloadDir is where new downloads go.
func (s Settings) downloadDir(app string) string {
	if s.Downloads == "videos" {
		return videosDir
	}
	return filepath.Join(app, "downloads")
}

// videosDir is the other choice for downloads: a folder of its own on the
// card, outside the app.
var videosDir = "/mnt/SDCARD/Videos/MiniFin"

func loadSettings(dir string) Settings {
	var cfg Settings
	b, err := os.ReadFile(filepath.Join(dir, "settings.json"))
	if err != nil {
		if !os.IsNotExist(err) {
			log.Printf("settings: %v", err)
		}
		return cfg
	}
	if err := json.Unmarshal(b, &cfg); err != nil {
		log.Printf("settings.json: %v", err)
		return Settings{}
	}
	return cfg
}

func saveSettings(dir string, cfg *Settings) error {
	b, err := json.MarshalIndent(cfg, "", "  ")
	if err != nil {
		return err
	}
	return atomicfile.Write(filepath.Join(dir, "settings.json"), append(b, '\n'), 0644)
}

// newDeviceID makes the id this handheld keeps.
func newDeviceID() string {
	b := make([]byte, 12)
	rand.Read(b)
	return "minifin-" + hex.EncodeToString(b)
}

// page is one screen on the navigation stack.
type page interface {
	draw(a *app)
	// handle takes a button press; false leaves it to the app (B goes
	// back, SELECT opens Appearance).
	handle(a *app, ev ui.Event) bool
	// busy reports whether something on it is loading (the loading
	// marker moves, so the screen is drawn again now and then).
	busy() bool
}

// refresher is a page that reloads after a video played (resume points,
// Continue Watching).
type refresher interface{ refresh(a *app) }

type app struct {
	scr   *ui.Screen
	input <-chan ui.Event
	dir   string
	cfg   Settings
	sec   map[string]string
	c     *Client
	img   *images
	dl    *downloads
	relay *relay
	pl    *players
	pend  *pending

	stack   []page
	done    chan func()
	redraw  chan struct{}
	offline bool
	quit    bool
	// the video playing now: playStop ends it, playDone closes after
	playMu   sync.Mutex
	playStop context.CancelFunc
	playDone chan struct{}
	// played is a note about the last video (which player, any error),
	// shown on its page.
	played   string
	playedID string
}

func main() {
	version.Flag("minifin")
	version.Note("minifin")
	scr, err := ui.Open()
	if err != nil {
		log.Fatal(err)
	}
	a := newApp(scr, ui.Input(), appdir.Dir())
	a.onSignals()
	defer func() {
		a.close()
		a.scr.Close()
		syncCard()
	}()
	a.start()
	a.run()
}

// newApp sets the app up: settings, the token, the caches, the relay.
func newApp(scr *ui.Screen, input <-chan ui.Event, dir string) *app {
	a := &app{scr: scr, input: input, dir: dir, done: make(chan func(), 16), redraw: make(chan struct{}, 1)}
	a.cfg = loadSettings(dir)
	if a.cfg.DeviceID == "" {
		a.cfg.DeviceID = newDeviceID()
		if err := saveSettings(dir, &a.cfg); err != nil {
			log.Printf("settings: %v", err)
		}
	}
	sec, err := secrets.LoadOrSetAside(filepath.Join(dir, "secrets.enc"))
	if err != nil {
		log.Printf("secrets: %v", err) // a new device: sign in again
	}
	a.sec = sec
	a.c = NewClient(a.cfg.Server, sec["token"], a.cfg.UserID, a.cfg.DeviceID)
	disk := cache.New(filepath.Join(dir, "cache"))
	disk.Prune(100 << 20) // quick: one directory listing
	a.img = newImages(func(ctx context.Context, k imgKey) ([]byte, error) {
		return a.c.fetch(ctx, a.c.ImageURL(k.id, k.kind, k.tag, k.w, k.h), 4<<20)
	}, disk, a.poke)
	a.dl = openDownloads(a.cfg.downloadDir(dir), a.c, a.cfg.kbps)
	if a.cfg.Downloads == "videos" { // the app folder's downloads still play
		a.dl.SetDir(filepath.Join(dir, "downloads"))
		a.dl.SetDir(videosDir)
	}
	a.dl.changed = a.poke
	a.dl.poster = a.savePoster
	a.relay, err = startRelay(a.c.Stream, a.c.AuthHeader)
	if err != nil {
		log.Printf("relay: %v", err)
	}
	a.pl = newPlayers(dir)
	a.pl.force = a.cfg.Player
	a.pend = &pending{path: filepath.Join(dir, "pending.json")}
	return a
}

func (a *app) close() {
	a.dl.Stop()
	a.img.stop()
	if a.relay != nil {
		a.relay.close()
	}
}

// start opens the first screen: setup when not signed in, else Home.
func (a *app) start() {
	if !a.c.SignedIn() || a.cfg.Server == "" {
		if !a.setup(true) {
			if len(a.dl.List()) > 0 {
				a.offline = true
				a.push(newDownloadsPage(true))
			}
			go a.dl.Run()
			return
		}
	}
	a.push(newHome(a))
	go a.dl.Run()
}

// poke asks for a redraw (a picture arrived, a download moved on).
func (a *app) poke() {
	select {
	case a.redraw <- struct{}{}:
	default:
	}
}

func (a *app) top() page {
	if len(a.stack) == 0 {
		return nil
	}
	return a.stack[len(a.stack)-1]
}

func (a *app) push(p page) { a.stack = append(a.stack, p) }

func (a *app) pop() {
	if len(a.stack) > 0 {
		a.stack = a.stack[:len(a.stack)-1]
	}
}

// later runs fn in the background; what it returns runs on the screen's
// goroutine, the only one that changes pages.
func (a *app) later(fn func() func()) {
	go func() {
		apply := fn()
		if apply != nil {
			a.done <- apply
		}
	}()
}

// run is the event loop: draw, then wait for a button, a background
// result or a picture. Nothing is drawn on a timer except while something
// loads.
func (a *app) run() {
	for !a.quit && len(a.stack) > 0 {
		p := a.top()
		p.draw(a)
		a.scr.Present()
		var tick <-chan time.Time
		if p.busy() {
			tick = time.After(250 * time.Millisecond)
		}
		select {
		case ev, ok := <-a.input:
			if !ok {
				return
			}
			if ev.Pressed {
				a.press(ev)
			}
		case <-a.redraw:
		case apply := <-a.done:
			apply()
		case <-tick:
		}
	}
}

// press handles one button.
func (a *app) press(ev ui.Event) {
	if ev.Button == ui.Menu {
		a.quit = true
		return
	}
	p := a.top()
	if p.handle(a, ev) {
		return
	}
	switch ev.Button {
	case ui.B:
		if len(a.stack) == 1 {
			a.quit = true
			return
		}
		a.pop()
	case ui.Select:
		a.scr.Appearance(a.input)
	}
}

// signedOut handles a token the server no longer takes: sign in again.
func (a *app) signedOut() {
	a.sec["token"] = ""
	a.c.SetLogin(a.cfg.Server, "", a.cfg.UserID)
	a.scr.Notice(a.input, "Signed out", errSignedOut.Error())
	if a.signInAgain() {
		a.stack = []page{newHome(a)}
	} else {
		a.offline = true
		a.stack = []page{newDownloadsPage(true)}
	}
}

// goOffline shows the downloads, for when the server can't be reached.
func (a *app) goOffline() {
	a.offline = true
	a.stack = []page{newDownloadsPage(true)}
}

// back online: Home again.
func (a *app) goOnline() {
	a.offline = false
	a.stack = []page{newHome(a)}
	a.dl.kick()
}

// failed handles a background error: sign-in again, offline, or nothing
// (the page shows it).
func (a *app) failed(err error) {
	if errors.Is(err, errSignedOut) {
		a.signedOut()
	}
}

// ---- around the player ----

// cpuFiles: the CPU's speed limits. While a video plays the minimum is
// raised to the maximum (the stock 1.2 GHz: no overclock), so the decoder
// never waits for the governor to speed up.
const (
	cpuMin = "/sys/devices/system/cpu/cpu0/cpufreq/scaling_min_freq"
	cpuMax = "/sys/devices/system/cpu/cpu0/cpufreq/scaling_max_freq"
)

func cpuBoost() (restore func()) {
	old, err := os.ReadFile(cpuMin)
	top, err2 := os.ReadFile(cpuMax)
	if err != nil || err2 != nil {
		return func() {}
	}
	os.WriteFile(cpuMin, top, 0644)
	return func() { os.WriteFile(cpuMin, old, 0644) }
}

// stayAwake keeps Onion from sleeping while a video plays (or a download
// runs). Only a marker it made itself is removed after.
func stayAwake() (release func()) {
	const f = "/tmp/stay_awake"
	if _, err := os.Stat(f); err == nil {
		return func() {}
	}
	if os.WriteFile(f, nil, 0644) != nil {
		return func() {}
	}
	return func() { os.Remove(f) }
}

// withPlayer hands the screen to a player: MiniFin's screen closes (its
// moving background stops drawing), the player runs, and the screen opens
// again with whatever the player left in the framebuffer cleared. While it
// runs, stopPlayer (MiniFin being stopped) ends it the gentle way: run's
// context is cancelled, which sends the player SIGTERM.
func (a *app) withPlayer(run func(ctx context.Context) playOutcome) playOutcome {
	pauseMiniTape()
	restoreCPU := cpuBoost()
	release := stayAwake()
	a.scr.Close()
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan struct{})
	a.playMu.Lock()
	a.playStop, a.playDone = cancel, done
	a.playMu.Unlock()
	out := run(ctx)
	a.playMu.Lock()
	a.playStop, a.playDone = nil, nil
	a.playMu.Unlock()
	cancel()
	close(done)
	release()
	restoreCPU()
	var err error
	for i := 0; i < 10; i++ {
		var s *ui.Screen
		if s, err = ui.Open(); err == nil {
			a.scr = s
			break
		}
		time.Sleep(200 * time.Millisecond)
	}
	if err != nil {
		log.Fatalf("the screen didn't come back after the player: %v", err)
	}
	return out
}

// stopPlayer ends a video playing now, if any, and waits for the player
// to leave (SIGTERM, then SIGKILL after 5 s: see players.command). It
// reports whether one was playing.
func (a *app) stopPlayer() bool {
	a.playMu.Lock()
	stop, done := a.playStop, a.playDone
	a.playMu.Unlock()
	if stop == nil {
		return false
	}
	stop()
	select {
	case <-done:
	case <-time.After(8 * time.Second):
		log.Printf("the player is still running after SIGKILL")
	}
	return true
}

// onSignals stops MiniFin cleanly when it is told to (SIGTERM from
// launch.sh, MiniTop or a power-off): the player first, the gentle way,
// then the downloads (saved where they are).
func (a *app) onSignals() {
	ch := make(chan os.Signal, 1)
	signal.Notify(ch, syscall.SIGTERM, syscall.SIGINT, syscall.SIGHUP)
	go func() {
		sig := <-ch
		log.Printf("%v: closing", sig)
		if a.stopPlayer() {
			log.Printf("the player stopped")
		}
		a.dl.Stop()
		syncCard()
		os.Exit(0)
	}()
}

// errors as sentences for pages that show them.
func errText(err error) string {
	if err == nil {
		return ""
	}
	s := err.Error()
	if s == "" {
		return "Something went wrong."
	}
	s = strings.ToUpper(s[:1]) + s[1:]
	if !strings.HasSuffix(s, ".") && !strings.HasSuffix(s, "?") && !strings.HasSuffix(s, ")") {
		s += "."
	}
	return s
}

// pending keeps positions of videos watched offline until the server can
// be told (pending.json).
type pending struct {
	path string
	mu   sync.Mutex // add (the screen) and flush (in the background)
}

type pendingEntry struct {
	Pos    float64 `json:"pos"`
	Played bool    `json:"played"`
}

func (p *pending) load() map[string]pendingEntry {
	m := map[string]pendingEntry{}
	if b, err := os.ReadFile(p.path); err == nil {
		json.Unmarshal(b, &m)
	}
	return m
}

func (p *pending) add(id string, pos float64, played bool) {
	p.mu.Lock()
	defer p.mu.Unlock()
	m := p.load()
	m[id] = pendingEntry{pos, played}
	b, _ := json.Marshal(m)
	atomicfile.Write(p.path, b, 0644)
}

// flush tells the server, and forgets what it took.
func (p *pending) flush(c *Client) {
	p.mu.Lock()
	defer p.mu.Unlock()
	m := p.load()
	if len(m) == 0 {
		return
	}
	for id, e := range m {
		if err := c.SetUserData(id, e.Pos, e.Played); err != nil {
			if isOffline(err) {
				return
			}
			log.Printf("pending position for %s: %v", id, err)
		}
		delete(m, id)
	}
	b, _ := json.Marshal(m)
	atomicfile.Write(p.path, b, 0644)
	log.Printf("sent the positions of videos watched offline")
}

// sync makes sure the card has written what MiniFin saved (FAT on an SD
// card; Onion may power off soon after).
func syncCard() { syscall.Sync() }
