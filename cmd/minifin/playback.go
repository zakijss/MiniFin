package main

import (
	"bufio"
	"context"
	"fmt"
	"io"
	"log"
	"math"
	"net/http"
	"os"
	"os/exec"
	"strconv"
	"strings"
	"sync"
	"syscall"
	"time"

	"github.com/zakijss/minideck/procs"
	"github.com/zakijss/minideck/ui"
)

// Video plays in a separate program that owns the screen and the buttons
// while it runs: minifin-player (MiniFin's own, in the app folder) or, when
// that isn't there or fails to start, Onion's ffplay.
//
// minifin-player is run as
//
//	minifin-player --url http://127.0.0.1:PORT/v/ID.ts [--start S] [--title T] [--sub FILE.srt] [--duration D]
//	    [--next TITLE] [--prev TITLE] [--audio A|B --audio-current I] [--subs Off|A --subs-current I]
//
// The URL is always MiniFin's relay. The stream at that URL already begins
// S seconds into the video (the server or the card started it there):
// --start tells the player where that is, for the times it shows and
// reports, and the subtitle file's times are counted from the start of
// the video. To seek, the player opens the same URL with ?t=SECONDS (video
// time, decimals allowed): the relay starts a new transcode there (or
// reads the download from the keyframe at or before it) and says where the
// stream really begins in the X-MiniFin-Start header. It prints, one per
// line on stdout:
//
//	dur SECONDS     the video's length (--duration D, the server's runtime,
//	                when the stream doesn't know it: a live transcode)
//	decoder hw|sw   the hardware decoder or libavcodec
//	pos SECONDS     where it is in the video, about once a second
//	paused 1|0      paused or playing again
//	end             played to the end
//	quit SECONDS    the user stopped it there
//	audio I         before quit: play again with audio track I of --audio
//	subtitle I      before quit: and with subtitle choice I of --subs
//	next, prev      before quit: play the --next or --prev episode
//	error MESSAGE   it couldn't play, or the stream stopped early ("stream
//	                ended early at X of Y s"): not an end
//
// and exits 0 after end or quit, non-zero on a failure.
//
// It dlopens Onion's libpadsp itself, so it gets no LD_PRELOAD (ffplay
// does). MiniFin stops either player with SIGTERM, and with SIGKILL only
// if it is still running 5 s later: killing a player outright mid-video
// can wedge the device's MI driver stack and freeze the screen until a
// reboot.

// playRequest is one video to play.
type playRequest struct {
	URL     string
	Start   float64 // where the stream begins in the video
	Title   string
	Sub     string  // an SRT file, or ""
	Runtime float64 // the video's length as the server knows it (0: unknown)

	Next, Prev  string   // episode titles for R2 / L2, "" for none
	Audio, Subs []string // track names for X / SELECT (fewer than 2: no choice)
	AudioCur    int
	SubsCur     int
}

// playerState is what the player has said so far.
type playerState struct {
	Dur     float64
	Pos     float64 // in the video
	Paused  bool
	Ended   bool
	Quit    bool
	Err     string
	Seen    bool   // a position has come in
	Decoder string // "hw" or "sw", when the player said
	Want    string // "next" or "prev": the user asked for that episode
	Audio   int    // a new choice in playRequest.Audio, or -1
	Sub     int    // a new choice in playRequest.Subs, or -1

	relative bool // the player counts from the stream's start, not the video's
	decided  bool
	start    float64
}

// line applies one line of minifin-player's output. It reports whether
// the line was understood.
func (st *playerState) line(s string) bool {
	word, arg, _ := strings.Cut(strings.TrimSpace(s), " ")
	arg = strings.TrimSpace(arg)
	num := func() (float64, bool) {
		v, err := strconv.ParseFloat(arg, 64)
		return v, err == nil && v >= 0 && v < 1e7
	}
	switch word {
	case "dur":
		if v, ok := num(); ok {
			st.Dur = v
			return true
		}
	case "pos":
		if v, ok := num(); ok {
			st.setPos(v)
			return true
		}
	case "paused":
		st.Paused = arg == "1"
		return arg == "1" || arg == "0"
	case "end":
		st.Ended = true
		return true
	case "decoder":
		if arg == "hw" || arg == "sw" {
			st.Decoder = arg
			log.Printf("player: %s decoding", map[string]string{"hw": "hardware", "sw": "software"}[arg])
			return true
		}
	case "next", "prev":
		st.Want = word
		return true
	case "audio", "subtitle":
		v, err := strconv.Atoi(arg)
		if err != nil || v < 0 {
			return false
		}
		if word == "audio" {
			st.Audio = v
		} else {
			st.Sub = v
		}
		return true
	case "quit":
		st.Quit = true
		if v, ok := num(); ok {
			st.setPos(v)
		}
		return true
	case "error":
		st.Ended = false // "stream ended early" is an error, not the end
		st.Err = arg
		if st.Err == "" {
			st.Err = "the player failed"
		}
		return true
	}
	return false
}

// setPos takes a position in the video. A player that counts from the
// start of the stream instead (its first position is far below --start)
// is put right, once, for the rest of the run.
func (st *playerState) setPos(v float64) {
	if !st.decided {
		st.decided = true
		st.relative = st.start > 30 && v < st.start/2
		if st.relative {
			log.Printf("player: positions count from the stream's start; adding %.0f s", st.start)
		}
	}
	if st.relative {
		v += st.start
	}
	st.Pos, st.Seen = v, true
}

// ffplayClock follows Onion's ffplay (FFmpeg 2.4) from its stderr: the
// showinfo filter's pts_time (a frame sampled every second) and the -stats
// status line ("  12.34 A-V: ..."). Both count the stream's own
// timestamps, which start at some offset (MPEG-TS starts at 1.4 s): the
// first one seen is taken as the start of the stream.
type ffplayClock struct {
	start  float64
	origin float64
	have   bool
	Pos    float64
	Seen   bool
}

// record reads one record (ffplay ends status lines with \r).
func (c *ffplayClock) record(s string) bool {
	v, ok := ffplayTime(s)
	if !ok {
		return false
	}
	if !c.have || v < c.origin-1 {
		c.origin, c.have = v, true
	}
	c.Pos, c.Seen = c.start+max(0, v-c.origin), true
	return true
}

// ffplayTime finds a time in one of ffplay's records.
func ffplayTime(s string) (float64, bool) {
	if i := strings.Index(s, "pts_time:"); i >= 0 {
		return leadingFloat(s[i+len("pts_time:"):])
	}
	for _, tag := range []string{"A-V:", "M-A:", "M-V:"} {
		if i := strings.Index(s, tag); i > 0 {
			return leadingFloat(strings.TrimSpace(s[:i]))
		}
	}
	return 0, false
}

// leadingFloat parses the number at the start of s.
func leadingFloat(s string) (float64, bool) {
	s = strings.TrimLeft(s, " ")
	end := 0
	for end < len(s) && (s[end] >= '0' && s[end] <= '9' || s[end] == '.' || end == 0 && s[end] == '-') {
		end++
	}
	v, err := strconv.ParseFloat(s[:end], 64)
	if err != nil || v < 0 || v != v || v > 1e7 {
		return 0, false
	}
	return v, true
}

// splitRecords is a bufio.SplitFunc for lines ending in \n or \r.
func splitRecords(data []byte, atEOF bool) (int, []byte, error) {
	for i, b := range data {
		if b == '\n' || b == '\r' {
			return i + 1, data[:i], nil
		}
	}
	if atEOF && len(data) > 0 {
		return len(data), data, nil
	}
	if len(data) > 64<<10 { // a very long record: hand it over in pieces
		return len(data), data, nil
	}
	return 0, nil, nil
}

// players runs the video players.
type players struct {
	own       string // minifin-player
	ffplay    string
	ffplayDir string // ffplay runs from Onion's folder, as Onion runs it
	force     string // "ffplay" uses ffplay even when minifin-player is there
	// startWait: minifin-player failing within this time, before playing
	// anything, counts as "couldn't start", and ffplay takes over.
	startWait time.Duration
	// killWait is how long a player has to leave after SIGTERM before it
	// gets SIGKILL.
	killWait time.Duration
}

func newPlayers(dir string) *players {
	pl := &players{own: dir + "/minifin-player", ffplay: "/mnt/SDCARD/.tmp_update/bin/ffplay",
		ffplayDir: "/mnt/SDCARD/.tmp_update", startWait: 4 * time.Second, killWait: 5 * time.Second}
	if p := os.Getenv("MINIFIN_PLAYER"); p != "" {
		pl.own = p
	}
	if p := os.Getenv("MINIDECK_FFPLAY"); p != "" {
		pl.ffplay, pl.ffplayDir = p, ""
	}
	return pl
}

// ownInstalled reports whether minifin-player is there to run.
func (pl *players) ownInstalled() bool {
	fi, err := os.Stat(pl.own)
	return err == nil && !fi.IsDir() && fi.Mode()&0111 != 0
}

// playEnv is the players' environment: Onion's libraries, and for ffplay
// (preload) the sound through Onion's audioserver when it runs (the same
// shim the menu uses; without it the sound device is busy).
// minifin-player loads that shim itself.
func playEnv(preload bool) []string {
	var env []string
	for _, kv := range os.Environ() {
		if !strings.HasPrefix(kv, "SDL_VIDEODRIVER=") && !strings.HasPrefix(kv, "SDL_AUDIODRIVER=") &&
			!strings.HasPrefix(kv, "LD_LIBRARY_PATH=") && !strings.HasPrefix(kv, "LD_PRELOAD=") {
			env = append(env, kv)
		}
	}
	env = append(env, "LD_LIBRARY_PATH=/lib:/config/lib:/mnt/SDCARD/miyoo/lib:/mnt/SDCARD/.tmp_update/lib:/mnt/SDCARD/.tmp_update/lib/parasyte:/customer/lib")
	if preload && procs.Running("audioserver") {
		env = append(env, "LD_PRELOAD=/mnt/SDCARD/miyoo/lib/libpadsp.so")
	}
	return env
}

// playOutcome is how a video ended.
type playOutcome struct {
	Player string // "minifin-player" or "ffplay"
	Why    string // why ffplay played it, if it did
	Pos    float64
	Ended  bool
	Err    string
	// Decoder is "hw" or "sw" when minifin-player said which it used.
	Decoder string
	Want    string // "next" or "prev"
	// Retrack: play it again from Pos with choice Audio of playRequest.Audio
	// and Sub of playRequest.Subs (-1: that one unchanged).
	Retrack    bool
	Audio, Sub int
}

// play plays req with minifin-player, or ffplay. update is called with
// each new position (in the video) and pause state; input is the buttons,
// read (and for minifin-player ignored) while the player owns them.
func (pl *players) play(ctx context.Context, req playRequest, input <-chan ui.Event, update func(pos float64, paused bool)) playOutcome {
	why := ""
	switch {
	case pl.force == "ffplay":
		why = "Settings > Video player is set to ffplay"
	case !pl.ownInstalled():
		why = "minifin-player isn't installed"
	default:
		out, started := pl.runOwn(ctx, req, input, update)
		if started || ctx.Err() != nil {
			return out
		}
		why = "minifin-player didn't start (" + out.Err + ")"
		log.Printf("player: %s; trying ffplay", why)
	}
	out := pl.runFFplay(ctx, req, input, update)
	out.Why = why
	return out
}

// runOwn runs minifin-player. started is false when it failed before
// playing anything, soon after starting.
func (pl *players) runOwn(ctx context.Context, req playRequest, input <-chan ui.Event, update func(float64, bool)) (playOutcome, bool) {
	out := playOutcome{Player: "minifin-player", Pos: req.Start, Audio: -1, Sub: -1}
	args := []string{"--url", req.URL}
	if req.Start > 0 {
		args = append(args, "--start", strconv.FormatFloat(req.Start, 'f', 3, 64))
	}
	if req.Title != "" {
		args = append(args, "--title", req.Title)
	}
	if req.Sub != "" {
		args = append(args, "--sub", req.Sub)
	}
	if req.Runtime > 0 {
		args = append(args, "--duration", strconv.FormatFloat(req.Runtime, 'f', 3, 64))
	}
	if req.Next != "" {
		args = append(args, "--next", req.Next)
	}
	if req.Prev != "" {
		args = append(args, "--prev", req.Prev)
	}
	if len(req.Audio) > 1 {
		args = append(args, "--audio", trackList(req.Audio), "--audio-current", strconv.Itoa(req.AudioCur))
	}
	if len(req.Subs) > 1 {
		args = append(args, "--subs", trackList(req.Subs), "--subs-current", strconv.Itoa(req.SubsCur))
	}
	cmd := pl.command(ctx, pl.own, args...)
	cmd.Env = playEnv(false)
	cmd.Stderr = os.Stderr // into minifin.log
	stdout, err := cmd.StdoutPipe()
	if err != nil {
		out.Err = err.Error()
		return out, false
	}
	began := time.Now()
	if err := cmd.Start(); err != nil {
		out.Err = err.Error()
		return out, false
	}
	stopInput := ignoreInput(input)
	st := &playerState{start: req.Start, Pos: req.Start, Audio: -1, Sub: -1}
	sc := bufio.NewScanner(stdout)
	sc.Buffer(make([]byte, 4096), 64<<10)
	for sc.Scan() {
		before := *st
		if !st.line(sc.Text()) {
			log.Printf("player: %q?", truncate(sc.Text(), 80))
			continue
		}
		if st.Pos != before.Pos || st.Paused != before.Paused {
			update(st.Pos, st.Paused)
		}
	}
	werr := cmd.Wait()
	stopInput()
	out.Pos, out.Ended, out.Err, out.Decoder = st.Pos, st.Ended, st.Err, st.Decoder
	if st.Quit && st.Err == "" {
		out.Want = st.Want
		if st.Audio < len(req.Audio) {
			out.Audio = st.Audio
		}
		if st.Sub < len(req.Subs) {
			out.Sub = st.Sub
		}
		out.Retrack = out.Audio >= 0 || out.Sub >= 0
	}
	if werr != nil && out.Err == "" && ctx.Err() == nil {
		out.Err = "minifin-player stopped: " + werr.Error()
	}
	if werr != nil && !st.Seen && time.Since(began) < pl.startWait && ctx.Err() == nil {
		return out, false
	}
	if werr == nil && !st.Ended && st.Err != "" && st.Seen {
		out.Err = st.Err // exit 0 after an error line: still an error
	}
	return out, true
}

// trackList is names for the player's --audio and --subs: "|" between them.
func trackList(names []string) string {
	clean := make([]string, len(names))
	for i, n := range names {
		clean[i] = strings.ReplaceAll(n, "|", "/")
	}
	return strings.Join(clean, "|")
}

// ffplayFilter flips the picture (the panel is mounted upside down and
// ffplay draws straight to it) and samples a frame's timestamp each second
// for the position (showinfo), as MiyooFin does.
const ffplayFilter = `hflip,vflip,split=2[main][tap];[tap]select=isnan(prev_selected_t)+gte(t-prev_selected_t\,1)+lte(t-prev_selected_t\,-1),showinfo,nullsink;[main]null`

// ffplayArgs is Onion's ffplay's command line for req.
func ffplayArgs(req playRequest) []string {
	return []string{"-fs", "-autoexit", "-stats", "-sn",
		"-probesize", "262144", "-analyzeduration", "1000000",
		"-vf", ffplayFilter, "-i", req.URL}
}

// runFFplay plays req with Onion's ffplay. It knows its keys from the
// buttons: A (space) pauses; B is nothing to ffplay, so MiniFin stops it;
// MENU (Escape) quits it.
func (pl *players) runFFplay(ctx context.Context, req playRequest, input <-chan ui.Event, update func(float64, bool)) playOutcome {
	out := playOutcome{Player: "ffplay", Pos: req.Start}
	ctx, stop := context.WithCancel(ctx)
	defer stop()
	cmd := pl.command(ctx, pl.ffplay, ffplayArgs(req)...)
	cmd.Env = playEnv(true)
	if pl.ffplayDir != "" {
		cmd.Dir = pl.ffplayDir
	}
	stderr, err := cmd.StderrPipe()
	if err != nil {
		out.Err = err.Error()
		return out
	}
	if err := cmd.Start(); err != nil {
		out.Err = "can't start ffplay: " + err.Error()
		return out
	}
	// update is called from the button goroutine and the stderr loop: one
	// at a time
	var upMu sync.Mutex
	report := update
	update = func(pos float64, paused bool) {
		upMu.Lock()
		defer upMu.Unlock()
		report(pos, paused)
	}
	var mu sync.Mutex
	clock := &ffplayClock{start: req.Start}
	paused, stoppedByUs := false, false
	done := make(chan struct{})
	var buttons sync.WaitGroup
	buttons.Add(1)
	go func() { // the buttons, while ffplay has the screen
		defer buttons.Done()
		for {
			select {
			case <-done:
				return
			case ev, ok := <-input:
				if !ok {
					return
				}
				if !ev.Pressed || ev.Repeat {
					continue
				}
				mu.Lock()
				switch ev.Button {
				case ui.A:
					paused = !paused
					pos, p := clock.Pos, paused
					mu.Unlock()
					update(pos, p)
					continue
				case ui.B, ui.Menu:
					stoppedByUs = true
					stop() // SIGTERM, then SIGKILL if it hangs on
				}
				mu.Unlock()
			}
		}
	}()
	logged := 0
	sc := bufio.NewScanner(stderr)
	sc.Buffer(make([]byte, 4096), 128<<10)
	sc.Split(splitRecords)
	for sc.Scan() {
		rec := sc.Text()
		mu.Lock()
		before := clock.Pos
		ok := clock.record(rec)
		pos, p := clock.Pos, paused
		mu.Unlock()
		if ok {
			if pos != before {
				update(pos, p)
			}
			continue
		}
		if t := strings.TrimSpace(rec); t != "" && logged < 40 && !strings.Contains(t, "nan") {
			logged++
			log.Printf("ffplay: %s", truncate(t, 160))
		}
	}
	werr := cmd.Wait()
	close(done)
	buttons.Wait()
	ignoreInput(input)() // the press that quit ffplay isn't MiniFin's
	mu.Lock()
	defer mu.Unlock()
	out.Pos = clock.Pos
	rt := req.Runtime
	nearEnd := rt > 0 && (out.Pos >= rt-60 || out.Pos >= rt*0.95)
	switch {
	case stoppedByUs:
	case werr == nil:
		out.Ended = nearEnd || rt <= 0
	case !clock.Seen:
		out.Err = "ffplay couldn't play it (" + werr.Error() + ")"
	}
	return out
}

// command runs a player that ctx stops: SIGTERM when ctx is done, and
// SIGKILL only if it is still there killWait later.
func (pl *players) command(ctx context.Context, path string, args ...string) *exec.Cmd {
	cmd := exec.CommandContext(ctx, path, args...)
	cmd.Cancel = func() error { return cmd.Process.Signal(syscall.SIGTERM) }
	cmd.WaitDelay = pl.killWait
	if cmd.WaitDelay <= 0 {
		cmd.WaitDelay = 5 * time.Second
	}
	return cmd
}

// ignoreInput reads and drops the buttons until the returned stop is
// called (the player has them), and for a moment after, so presses meant
// for the player (MENU to quit it) don't reach MiniFin's screens.
func ignoreInput(input <-chan ui.Event) (stop func()) {
	done := make(chan struct{})
	finished := make(chan struct{})
	go func() {
		defer close(finished)
		var quiet <-chan time.Time
		for {
			select {
			case <-done:
				done = nil
				quiet = time.After(300 * time.Millisecond)
			case <-quiet:
				return
			case _, ok := <-input:
				if !ok {
					return
				}
			}
		}
	}()
	var once sync.Once
	return func() {
		once.Do(func() { close(done) })
		<-finished
	}
}

func truncate(s string, n int) string {
	if len(s) <= n {
		return s
	}
	return s[:n] + "…"
}

// ---- reporting to the server ----

// reporter tells the server what plays, so Continue Watching and the
// dashboard follow: Playing at the start, Progress every 10 seconds and on
// pause and unpause, Stopped with the place at the end. Reports go out in
// order from one goroutine, so a slow server never holds up the player's
// output.
type reporter struct {
	c        *Client
	body     map[string]any // the ids common to every report
	every    time.Duration
	queue    chan func()
	finished chan struct{}

	mu      sync.Mutex
	closed  bool // stop was called: no more reports
	started bool
	last    time.Time
	paused  bool
	pos     float64
	failed  bool // the server couldn't be reached
}

func newReporter(c *Client, p streamPlan, method string) *reporter {
	r := &reporter{c: c, every: 10 * time.Second, queue: make(chan func(), 64), finished: make(chan struct{}),
		body: map[string]any{"ItemId": p.ItemID, "MediaSourceId": p.MediaSourceID, "PlaySessionId": p.PlaySessionID,
			"PlayMethod": method, "CanSeek": false, "IsMuted": false}}
	if p.Audio >= 0 {
		r.body["AudioStreamIndex"] = p.Audio
	}
	if p.Sub >= 0 {
		r.body["SubtitleStreamIndex"] = p.Sub
	}
	go func() {
		defer close(r.finished)
		for f := range r.queue {
			f()
		}
	}()
	return r
}

func (r *reporter) send(path string, pos float64, extra map[string]any) {
	b := map[string]any{}
	for k, v := range r.body {
		b[k] = v
	}
	for k, v := range extra {
		b[k] = v
	}
	b["PositionTicks"] = toTicks(pos)
	select {
	case r.queue <- func() {
		if err := r.c.post(path, nil, b, nil); err != nil {
			r.mu.Lock()
			r.failed = r.failed || isOffline(err)
			r.mu.Unlock()
			log.Printf("report %s: %v", path, err)
		}
	}:
	default: // the server is far behind: drop this one, the next will do
	}
}

// update is called with every new position or pause state.
func (r *reporter) update(pos float64, paused bool) {
	r.mu.Lock()
	defer r.mu.Unlock()
	if r.closed {
		return
	}
	prev := r.pos
	r.pos = pos
	switch {
	case r.started && math.Abs(pos-prev) > 5:
		// a seek: the server hears the new place at once
		r.last = time.Now()
		r.send("/Sessions/Playing/Progress", pos, map[string]any{"IsPaused": paused, "EventName": "TimeUpdate"})
	case !r.started:
		r.started, r.last, r.paused = true, time.Now(), paused
		r.send("/Sessions/Playing", pos, map[string]any{"IsPaused": paused, "PlaybackStartTimeTicks": time.Now().Unix() * ticksPerSecond})
	case paused != r.paused:
		r.paused, r.last = paused, time.Now()
		ev := "Unpause"
		if paused {
			ev = "Pause"
		}
		r.send("/Sessions/Playing/Progress", pos, map[string]any{"IsPaused": paused, "EventName": ev})
	case time.Since(r.last) >= r.every:
		r.last = time.Now()
		r.send("/Sessions/Playing/Progress", pos, map[string]any{"IsPaused": paused, "EventName": "TimeUpdate"})
	}
}

// setSession switches the reports to a new play session (a seek started
// a new transcode).
func (r *reporter) setSession(id string) {
	r.mu.Lock()
	r.body["PlaySessionId"] = id
	r.mu.Unlock()
}

// stop sends Stopped (at the end of the video when it ended) and waits a
// little for the reports to go out. It reports whether the server got
// them.
func (r *reporter) stop(pos float64, ended bool, runtime float64, failed bool) bool {
	if ended && runtime > 0 {
		pos = runtime
	}
	r.mu.Lock()
	if r.closed {
		r.mu.Unlock()
		return !r.failed
	}
	if !r.started {
		r.started = true
		r.send("/Sessions/Playing", pos, map[string]any{"IsPaused": false})
	}
	r.send("/Sessions/Playing/Stopped", pos, map[string]any{"Failed": failed})
	r.closed = true
	r.mu.Unlock()
	close(r.queue)
	select {
	case <-r.finished:
	case <-time.After(6 * time.Second):
		log.Printf("report: the server is slow; not waiting for it")
		return false
	}
	r.mu.Lock()
	defer r.mu.Unlock()
	return !r.failed
}

// describePlayer says which player played, for the item page.
func describePlayer(o playOutcome) string {
	if o.Player == "ffplay" {
		if o.Why != "" {
			return "Played with ffplay: " + o.Why + "."
		}
		return "Played with ffplay."
	}
	switch o.Decoder {
	case "hw":
		return "Played with minifin-player, hardware decoding."
	case "sw":
		return "Played with minifin-player, software decoding."
	}
	return "Played with minifin-player."
}

// pauseMiniTape pauses MiniTape's music, if it plays, so the video's
// sound has the speaker (best effort: MiniTape's player answers on
// 127.0.0.1).
func pauseMiniTape() {
	c := &http.Client{Timeout: 700 * time.Millisecond}
	resp, err := c.Get("http://127.0.0.1:45211/state")
	if err != nil {
		return
	}
	b, _ := io.ReadAll(io.LimitReader(resp.Body, 64<<10))
	resp.Body.Close()
	s := string(b)
	if strings.Contains(s, `"Playing":true`) && strings.Contains(s, `"Paused":false`) {
		if r, err := c.Post("http://127.0.0.1:45211/toggle", "application/json", nil); err == nil {
			r.Body.Close()
			log.Printf("paused MiniTape's music for the video")
		}
	}
}

// fmtClock writes seconds as 1:02:03 or 2:03.
func fmtClock(sec float64) string {
	s := int(sec + 0.5)
	if s >= 3600 {
		return fmt.Sprintf("%d:%02d:%02d", s/3600, s%3600/60, s%60)
	}
	return fmt.Sprintf("%d:%02d", s/60, s%60)
}
