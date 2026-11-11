package main

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"image"
	"image/png"
	"log"
	"math"
	"os"
	"path/filepath"
	"time"

	"github.com/zakijss/minideck/atomicfile"
	"github.com/zakijss/minideck/ui"
)

// title is how a video is named to the player and in lists: "Series ·
// S1:E3 · Name" for an episode.
func title(it Item) string {
	if it.Type == "Episode" && it.SeriesName != "" {
		return it.SeriesName + " · " + episodeCode(it) + " · " + it.Name
	}
	if it.ProductionYear > 0 && it.Type == "Movie" {
		return fmt.Sprintf("%s (%d)", it.Name, it.ProductionYear)
	}
	return it.Name
}

// episodeCode is "S1:E3", or "" when the numbers aren't known.
func episodeCode(it Item) string {
	switch {
	case it.ParentIndexNumber > 0 && it.IndexNumber > 0:
		return fmt.Sprintf("S%d:E%d", it.ParentIndexNumber, it.IndexNumber)
	case it.IndexNumber > 0:
		return fmt.Sprintf("E%d", it.IndexNumber)
	}
	return ""
}

// play plays it from start seconds with the chosen tracks (-1: the
// default audio, no subtitles). When an episode plays to the end and the
// show has another, a countdown offers it, and so on down the season. In
// the player, R2 and L2 go to the next or previous episode, and X and
// SELECT pick other tracks (the video starts again where it was).
func (a *app) play(it Item, start float64, audio, sub int) {
	for {
		prev, next := a.neighbours(it)
		tr := tracksFor(it, audio, sub)
		out, ok := a.playOnce(it, start, audio, sub, playRequest{Next: epTitle(next), Prev: epTitle(prev),
			Audio: tr.audioNames, AudioCur: tr.audioCur, Subs: tr.subNames, SubsCur: tr.subCur})
		if !ok {
			return
		}
		var to *Item
		switch {
		case out.Retrack:
			if out.Audio >= 0 {
				audio = tr.audio[out.Audio]
			}
			if out.Sub >= 0 {
				sub = tr.subs[out.Sub]
			}
			start = out.Pos
			continue
		case out.Want == "next":
			to = next
		case out.Want == "prev":
			to = prev
		case out.Ended && next != nil:
			if !a.countdown(*next) {
				return
			}
			to = next
		}
		if to == nil {
			return
		}
		it = *to
		if !a.offline {
			if full, err := a.c.Item(it.ID); err == nil {
				it = full // its tracks
			}
		}
		start = 0
		if !it.Played() {
			start = it.Resume()
		}
		audio, sub = pickStreams(it, nil, nil, a.cfg.Subtitles)
		a.push(newDetail(a, it)) // B after it goes back where the user was
	}
}

// epTitle is how the player names an episode for R2 / L2.
func epTitle(it *Item) string {
	if it == nil {
		return ""
	}
	if c := episodeCode(*it); c != "" {
		return c + " " + it.Name
	}
	return it.Name
}

// tracks are a video's audio tracks and subtitle choices ("Off" first),
// as names for the player and the stream indexes they stand for.
type tracks struct {
	audioNames, subNames []string
	audio, subs          []int
	audioCur, subCur     int
}

func tracksFor(it Item, audio, sub int) tracks {
	t := tracks{subNames: []string{"Off"}, subs: []int{-1}}
	def := -1
	if len(it.MediaSources) > 0 && it.MediaSources[0].DefaultAudioStreamIndex != nil {
		def = *it.MediaSources[0].DefaultAudioStreamIndex
	}
	for i, s := range streamsOf(it, "Audio") {
		t.audioNames = append(t.audioNames, streamName(s))
		t.audio = append(t.audio, s.Index)
		if s.Index == audio || (audio < 0 && s.Index == def) {
			t.audioCur = i
		}
	}
	for _, s := range streamsOf(it, "Subtitle") {
		t.subNames = append(t.subNames, streamName(s))
		t.subs = append(t.subs, s.Index)
		if s.Index == sub {
			t.subCur = len(t.subs) - 1
		}
	}
	return t
}

// playOnce plays one video. A downloaded copy plays from the card even
// online: it starts at once and spares the server. ok is false when it
// didn't get as far as the player.
// extra carries the player's choices (episodes, tracks); a download
// offers only the episodes.
func (a *app) playOnce(it Item, start float64, audio, sub int, extra playRequest) (playOutcome, bool) {
	if dl, ok := a.dl.Get(it.ID); ok && dl.State == dlDone {
		return a.playLocal(dl, start, sub, playRequest{Next: extra.Next, Prev: extra.Prev})
	}
	if a.offline {
		a.scr.Notice(a.input, "Not on the card", "This video isn't downloaded, and the server can't be reached.")
		return playOutcome{}, false
	}
	if a.relay == nil {
		a.scr.Notice(a.input, "Can't play", "MiniFin couldn't open its local relay for the player. Restart MiniFin.")
		return playOutcome{}, false
	}
	a.scr.Alert("Starting the video", "Asking "+a.serverLabel()+" to prepare it…")
	external := a.pl.force != "ffplay" && a.pl.ownInstalled()
	kbps := a.cfg.kbps()
	plan, err := a.c.Plan(it, start, audio, sub, kbps, external)
	if err != nil {
		if isOffline(err) {
			a.scr.Notice(a.input, "Can't reach the server", errText(err))
			return playOutcome{}, false
		}
		a.failed(err)
		if a.c.SignedIn() {
			a.scr.Notice(a.input, "Can't play it", errText(err))
		}
		return playOutcome{}, false
	}
	subPath := ""
	if plan.SubURL != "" {
		ctx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
		b, err := a.c.fetch(ctx, plan.SubURL, 8<<20)
		cancel()
		if err == nil {
			subPath = filepath.Join(os.TempDir(), "minifin-subtitles.srt")
			if werr := os.WriteFile(subPath, b, 0644); werr != nil {
				subPath = ""
			}
		} else {
			log.Printf("subtitles: %v", err)
		}
	}
	rep := newReporter(a.c, plan, "Transcode")
	req := extra
	req.URL, req.Start, req.Title, req.Sub, req.Runtime = a.relay.Stream(plan.URL, a.seeker(it, plan, audio, sub, kbps, external, rep)),
		start, title(it), subPath, plan.Runtime
	log.Printf("playing %s from %.0f s at %d kbit/s", it.ID, start, kbps)
	out := a.withPlayer(func(ctx context.Context) playOutcome {
		return a.pl.play(ctx, req, a.input, rep.update)
	})
	a.relay.Forget()
	if subPath != "" {
		os.Remove(subPath)
	}
	failed := out.Err != "" && out.Pos <= start+5
	if !rep.stop(out.Pos, out.Ended, plan.Runtime, failed) {
		a.pend.add(it.ID, endPos(out, plan.Runtime), out.Ended)
	}
	a.afterPlay(it, out)
	return out, true
}

// seeker is how the relay seeks in a stream (?t=): the same plan asked
// for from t, a new transcode, with the old one stopped on the server and
// the reports moved to the new play session.
func (a *app) seeker(it Item, plan streamPlan, audio, sub, kbps int, external bool, rep *reporter) func(t float64) (string, error) {
	cur := plan.PlaySessionID
	return func(t float64) (string, error) {
		np, err := a.c.Plan(it, t, audio, sub, kbps, external)
		if err != nil {
			return "", err
		}
		if err := a.c.StopEncoding(cur); err != nil {
			log.Printf("stopping the old transcode: %v", err)
		}
		cur = np.PlaySessionID
		rep.setSession(cur)
		return np.URL, nil
	}
}

// endPos is where a video stopped: its end, when it played to the end.
func endPos(out playOutcome, runtime float64) float64 {
	if out.Ended && runtime > 0 {
		return runtime
	}
	return out.Pos
}

// playLocal plays a download from the card. The position is kept with the
// download, and told to the server now or once it can be reached.
func (a *app) playLocal(dl Download, start float64, sub int, extra playRequest) (playOutcome, bool) {
	ls, at, err := openLocal(dl, start)
	if err != nil {
		a.scr.Notice(a.input, "Can't play the download", errText(err))
		return playOutcome{}, false
	}
	if a.relay == nil {
		ls.Close()
		a.scr.Notice(a.input, "Can't play", "MiniFin couldn't open its local relay for the player. Restart MiniFin.")
		return playOutcome{}, false
	}
	subPath := ""
	for _, s := range dl.Subs {
		if s.Index == sub {
			if p := filepath.Join(dl.dir, s.File); fileExists(p) {
				subPath = p
			}
		}
	}
	runtime := dl.Item.Runtime()
	reopen := func(t float64) (*localStream, float64, error) { return openLocal(dl, t) }
	req := extra
	req.URL, req.Start, req.Title, req.Sub, req.Runtime = a.relay.Local(ls, reopen), at, title(dl.Item), subPath, runtime
	var rep *reporter
	if !a.offline {
		rep = newReporter(a.c, streamPlan{ItemID: dl.ID, MediaSourceID: dl.ID, PlaySessionID: randomID(), Audio: -1, Sub: sub}, "DirectPlay")
	}
	update := func(pos float64, paused bool) {
		if rep != nil {
			rep.update(pos, paused)
		}
	}
	log.Printf("playing %s from the card from %.0f s", dl.ID, at)
	out := a.withPlayer(func(ctx context.Context) playOutcome {
		return a.pl.play(ctx, req, a.input, update)
	})
	a.relay.Forget()
	pos := endPos(out, runtime)
	played := out.Ended
	a.dl.SetPosition(dl.ID, pos, played)
	if rep == nil || !rep.stop(out.Pos, out.Ended, runtime, false) {
		a.pend.add(dl.ID, pos, played)
	}
	a.afterPlay(dl.Item, out)
	return out, true
}

// afterPlay notes how the video went, for its page, and reloads the pages
// that show resume points.
func (a *app) afterPlay(it Item, out playOutcome) {
	a.playedID = it.ID
	a.played = describePlayer(out)
	if out.Err != "" {
		a.played += " " + errText(screenError(out.Err))
		log.Printf("playback of %s: %s (%s)", it.ID, out.Err, out.Player)
	}
	log.Printf("stopped %s at %.0f s (ended %v) with %s", it.ID, out.Pos, out.Ended, out.Player)
	a.refreshPages()
}

// refreshPages reloads every open page that shows resume points or
// watched marks.
func (a *app) refreshPages() {
	for _, p := range a.stack {
		if r, ok := p.(refresher); ok {
			r.refresh(a)
		}
	}
}

// markPlayed marks a video watched or not on the server. Either way its
// resume point goes, as when Jellyfin's own apps do it. Only online: the
// callers leave the button out offline.
func (a *app) markPlayed(it Item, played bool) {
	a.later(func() func() {
		err := a.c.SetUserData(it.ID, 0, played)
		return func() {
			if err != nil {
				if errors.Is(err, errSignedOut) {
					a.failed(err)
					return
				}
				a.scr.Notice(a.input, "Couldn't mark it", errText(err))
				return
			}
			a.dl.SetPosition(it.ID, 0, played) // the copy on the card agrees
			a.refreshPages()
		}
	})
}

// markLabel is the footer label for marking it: the opposite of how it is.
func markLabel(it Item) string {
	if it.Played() {
		return "Mark unwatched"
	}
	return "Mark watched"
}

func fileExists(p string) bool {
	_, err := os.Stat(p)
	return err == nil
}

// savePoster keeps a download's poster on the card, for the offline list.
func (a *app) savePoster(it Item, path string) {
	k := posterKey(it, posterW, posterH)
	if k.id == "" {
		return
	}
	img, err := a.img.load(k)
	if err != nil || img == nil {
		return
	}
	var buf bytes.Buffer
	if png.Encode(&buf, img) == nil {
		atomicfile.Write(path, buf.Bytes(), 0644)
	}
}

// serverLabel names the server for messages.
func (a *app) serverLabel() string {
	if a.cfg.ServerName != "" {
		return a.cfg.ServerName
	}
	return "the server"
}

// nextWait is how long the next episode's countdown runs.
var nextWait = 10 * time.Second

// nextEpisode is the episode after it in its show.
func (a *app) nextEpisode(it Item) *Item {
	_, next := a.neighbours(it)
	return next
}

// neighbours are the episodes before and after it in its show: from the
// server, or offline the nearest ones downloaded. nil when there is none.
func (a *app) neighbours(it Item) (prev, next *Item) {
	if it.Type != "Episode" || it.SeriesID == "" {
		return nil, nil
	}
	if !a.offline {
		eps, err := a.c.Episodes(it.SeriesID, "")
		if err == nil {
			for i, e := range eps {
				if e.ID == it.ID {
					if i > 0 {
						prev = &eps[i-1]
					}
					if i+1 < len(eps) {
						next = &eps[i+1]
					}
				}
			}
			return prev, next
		}
		if !isOffline(err) {
			return nil, nil
		}
	}
	after := func(x, y Item) bool { // x comes after y
		if x.ParentIndexNumber != y.ParentIndexNumber {
			return x.ParentIndexNumber > y.ParentIndexNumber
		}
		return x.IndexNumber > y.IndexNumber
	}
	for _, dl := range a.dl.List() {
		e := dl.Item
		if dl.State != dlDone || e.SeriesID != it.SeriesID || e.ID == it.ID {
			continue
		}
		if after(e, it) && (next == nil || after(*next, e)) {
			next = &e
		}
		if after(it, e) && (prev == nil || after(e, *prev)) {
			prev = &e
		}
	}
	return prev, next
}

// countdown offers the next episode for nextWait: true to play it (A, or
// the time ran out), false to stop (B).
func (a *app) countdown(next Item) bool {
	end := time.Now().Add(nextWait)
	for {
		left := time.Until(end)
		if left <= 0 {
			return true
		}
		a.drawNext(next, left)
		a.scr.Present()
		// redraw when the shown second changes, or a picture arrives
		tick := left - left.Truncate(time.Second)
		if tick <= 0 {
			tick = time.Second
		}
		select {
		case ev, ok := <-a.input:
			if !ok {
				return false
			}
			if !ev.Pressed {
				continue
			}
			switch ev.Button {
			case ui.A, ui.Start:
				return true
			case ui.B, ui.Menu:
				return false
			}
		case <-time.After(min(tick, left)):
		case <-a.redraw:
		}
	}
}

// drawNext is the countdown screen: the episode's still, its name and
// overview, and the seconds left.
func (a *app) drawNext(next Item, left time.Duration) {
	s := a.scr
	s.SetTicker("")
	top := s.StaticFrame("Up next", next.SeriesName, ui.Hint{Button: "A", Label: "Play now"}, ui.Hint{Button: "B", Label: "Stop"})
	r := image.Rect(24, top+6, 24+stillW, top+6+stillH)
	drawPicture(s, r, a.img.get(wideKey(next, stillW, stillH)), next.Name, false, progressOf(next), false)
	x, w := r.Max.X+20, ui.W-r.Max.X-20-26
	y := top + 6
	if c := episodeCode(next); c != "" {
		s.Text(x, y, c, ui.Caps, ui.Muted)
		y += ui.LineHeight(ui.Caps) + 4
	}
	for _, l := range wrapLines(ui.Heading, next.Name, w, 3) {
		s.Text(x, y, l, ui.Heading, ui.Text)
		y += ui.LineHeight(ui.Heading)
	}
	if rt := runtimeText(next.Runtime()); rt != "" {
		s.Text(x, y+2, rt, ui.Small, ui.Muted)
	}
	y = r.Max.Y + 14
	secs := int(math.Ceil(left.Seconds()))
	bottom := contentBottom - 54
	if ov := next.Overview; ov != "" {
		for _, l := range wrapLines(ui.Small, ov, ui.W-48, max(0, (bottom-y)/ui.LineHeight(ui.Small))) {
			s.Text(24, y, l, ui.Small, ui.Muted)
			y += ui.LineHeight(ui.Small)
		}
	}
	msg := fmt.Sprintf("Playing in %d s", secs)
	if secs == 1 {
		msg = "Playing in 1 s"
	}
	s.TextCenter(ui.W/2, bottom+4, msg, ui.Heading, ui.Text)
	bar := image.Rect(120, bottom+34, ui.W-120, bottom+40)
	s.Bar(bar, 1-left.Seconds()/nextWait.Seconds(), ui.Accent)
}
