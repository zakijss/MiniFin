package main

import (
	"fmt"
	"image"
	"strings"

	"github.com/zakijss/minideck/ui"
)

// The item page: the poster on the left; the title, facts, where the user
// is and the overview on the right, over a short list of what to do (play,
// resume, the audio and subtitle tracks, download). For a show the list is
// its next episode and its seasons.

type detailPage struct {
	it      Item
	loading bool
	err     string
	list    ui.List
	audio   *int // the track chosen here; nil: the default
	sub     *int
	seasons []Item
	next    *Item // the show's next episode
	gen     int
}

func newDetail(a *app, it Item) *detailPage {
	p := &detailPage{it: it}
	p.refresh(a)
	return p
}

// refresh reads the item again (after a video played: the resume point).
func (p *detailPage) refresh(a *app) {
	if dl, ok := a.dl.Get(p.it.ID); ok && a.offline {
		p.it = dl.Item
		return
	}
	if a.offline {
		return
	}
	p.loading = true
	p.gen++
	gen, id, series := p.gen, p.it.ID, p.it.Type == "Series"
	a.later(func() func() {
		full, err := a.c.Item(id)
		var seasons []Item
		var next *Item
		if err == nil && series {
			seasons, err = a.c.Seasons(id)
			if items, nerr := a.c.NextUpFor(id); nerr == nil && len(items) > 0 {
				next = &items[0]
			}
		}
		return func() {
			if gen != p.gen {
				return
			}
			p.loading = false
			if err != nil {
				p.err = errText(err)
				a.failed(err)
				return
			}
			p.it, p.seasons, p.next, p.err = full, seasons, next, ""
		}
	})
}

func (p *detailPage) busy() bool { return p.loading && p.it.Overview == "" }

// row values in the action list
type action string

const (
	actPlay     action = "play"
	actRestart  action = "restart"
	actAudio    action = "audio"
	actSubs     action = "subs"
	actDownload action = "download"
	actNext     action = "next"
)

// rows is the action list for the item as it is now.
func (p *detailPage) rows(a *app) []ui.Item {
	if p.it.Type == "Series" {
		var out []ui.Item
		if p.next != nil {
			out = append(out, ui.Item{Label: "Next: " + strings.TrimSpace(episodeCode(*p.next)+" "+p.next.Name), Value: actNext})
		}
		for _, s := range p.seasons {
			out = append(out, ui.Item{Label: s.Name, Detail: seasonState(s), Value: s, Chevron: true})
		}
		if len(out) == 0 && p.loading {
			out = append(out, ui.Item{Label: "Loading the seasons", Value: nil})
		}
		return out
	}
	var out []ui.Item
	if at := p.it.Resume(); at > 0 && !p.it.Played() {
		out = append(out, ui.Item{Label: "Resume from " + fmtClock(at), Value: actPlay},
			ui.Item{Label: "Play from the start", Value: actRestart})
	} else {
		out = append(out, ui.Item{Label: "Play", Detail: runtimeText(p.it.Runtime()), Value: actPlay})
	}
	if as := streamsOf(p.it, "Audio"); len(as) > 1 {
		out = append(out, ui.Item{Label: "Audio", Detail: p.audioName(a, as), Value: actAudio})
	}
	if subs := p.subChoices(a); len(subs) > 1 {
		out = append(out, ui.Item{Label: "Subtitles", Detail: p.subName(a, subs), Value: actSubs})
	}
	out = append(out, ui.Item{Label: "Download", Detail: downloadState(a, p.it), Value: actDownload})
	return out
}

// seasonState is a season's row value: "Watched", "4 to watch".
func seasonState(s Item) string {
	switch {
	case s.Played():
		return "Watched"
	case s.UserData != nil && s.UserData.UnplayedItemCount > 0:
		return fmt.Sprintf("%d to watch", s.UserData.UnplayedItemCount)
	}
	return ""
}

// subChoice is a subtitle track (or Off, index -1).
type subChoice struct {
	index int
	name  string
}

// subChoices are Off and the subtitle tracks: the item's own online, the
// ones kept with a download offline.
func (p *detailPage) subChoices(a *app) []subChoice {
	out := []subChoice{{-1, "Off"}}
	if dl, ok := a.dl.Get(p.it.ID); ok && (a.offline || dl.State == dlDone) {
		for _, s := range dl.Subs {
			out = append(out, subChoice{s.Index, s.Name})
		}
		return out
	}
	for _, s := range streamsOf(p.it, "Subtitle") {
		name := streamName(s)
		if !s.IsTextSubtitleStream && !textSubtitles[strings.ToLower(s.Codec)] {
			name += " (burned in)"
		}
		out = append(out, subChoice{s.Index, name})
	}
	return out
}

// tracks are the audio and subtitle indexes to play with.
func (p *detailPage) tracks(a *app) (int, int) {
	return pickStreams(p.it, p.audio, p.sub, a.cfg.Subtitles)
}

func (p *detailPage) audioName(a *app, as []MediaStream) string {
	cur, _ := p.tracks(a)
	for _, s := range as {
		if s.Index == cur {
			return streamName(s)
		}
	}
	return streamName(as[0])
}

func (p *detailPage) subName(a *app, subs []subChoice) string {
	_, cur := p.tracks(a)
	for _, s := range subs {
		if s.index == cur {
			return s.name
		}
	}
	return "Off"
}

// downloadState is the Download row's value.
func downloadState(a *app, it Item) string {
	dl, ok := a.dl.Get(it.ID)
	if !ok {
		if rt := it.Runtime(); rt > 0 {
			return "About " + sizeText(streamBytes(a.cfg.kbps(), rt))
		}
		return ""
	}
	switch dl.State {
	case dlDone:
		return "On the card"
	case dlRunning:
		return fmt.Sprintf("%d %%", int(dl.Progress()*100))
	case dlQueued:
		return "Waiting"
	case dlPaused:
		return "Paused"
	}
	return "Failed"
}

func (p *detailPage) draw(a *app) {
	s := a.scr
	s.SetTicker("")
	kind := map[string]string{"Movie": "Movie", "Episode": "Episode", "Series": "Show"}[p.it.Type]
	if kind == "" {
		kind = "Video"
	}
	top := s.StaticFrame(kind, "", ui.Hint{Button: "A", Label: "Select"}, ui.Hint{Button: "Y", Label: "About"}, ui.Hint{Button: "B", Label: "Back"})
	// the poster
	pr := image.Rect(24, top+6, 24+bigW, top+6+bigH)
	var img image.Image
	if dl, ok := a.dl.Get(p.it.ID); ok && a.offline {
		img = a.img.file(dl.dir + "/poster.png")
	} else {
		img = a.img.get(posterKey(p.it, bigW, bigH))
	}
	drawPicture(s, pr, img, p.it.Name, false, 0, false)

	x, w := pr.Max.X+22, ui.W-pr.Max.X-22-26
	y := top + 2
	if p.it.Type == "Episode" && p.it.SeriesName != "" {
		s.Text(x, y, ui.Fit(ui.Caps, strings.ToUpper(p.it.SeriesName), w), ui.Caps, ui.Muted)
		y += ui.LineHeight(ui.Caps) + 4
	}
	f := ui.Title
	lines := wrapLines(f, p.it.Name, w, 2)
	if len(ui.Wrap(f, p.it.Name, w)) > 2 {
		f = ui.Heading
		lines = wrapLines(f, p.it.Name, w, 2)
	}
	for _, l := range lines {
		s.Text(x, y, l, f, ui.Text)
		y += ui.LineHeight(f)
	}
	y += 2
	if m := metaLine(p.it); m != "" {
		s.Text(x, y, ui.Fit(ui.Small, m, w), ui.Small, ui.Muted)
		y += ui.LineHeight(ui.Small) + 4
	}
	if fr := progressOf(p.it); fr > 0 {
		s.Bar(image.Rect(x, y+5, x+120, y+10), fr, ui.Accent)
		s.Text(x+132, y, ui.Fit(ui.Small, leftText(p.it), w-132), ui.Small, ui.Text)
		y += ui.LineHeight(ui.Small) + 4
	} else if st := stateLine(p.it); st != "" {
		s.Text(x, y, st, ui.Small, ui.Text)
		y += ui.LineHeight(ui.Small) + 4
	}
	if a.playedID == p.it.ID && a.played != "" {
		for _, l := range wrapLines(ui.Small, a.played, w, 2) {
			s.Text(x, y, l, ui.Small, ui.Faint)
			y += ui.LineHeight(ui.Small)
		}
		y += 4
	}
	// the list at the bottom, the overview in what's left
	p.list.Items = p.rows(a)
	n := min(len(p.list.Items), 4)
	if p.it.Type == "Series" {
		n = min(len(p.list.Items), 3)
	}
	lh := 54
	listTop := contentBottom - n*lh
	room := listTop - 10 - y
	if p.err != "" {
		s.Text(x, y, ui.Fit(ui.Small, p.err, w), ui.Small, ui.Bad)
		y += ui.LineHeight(ui.Small) + 2
		room -= ui.LineHeight(ui.Small) + 2
	}
	if ov := p.it.Overview; ov != "" && room >= ui.LineHeight(ui.Small) {
		for _, l := range wrapLines(ui.Small, ov, w, room/ui.LineHeight(ui.Small)) {
			s.Text(x, y, l, ui.Small, ui.Muted)
			y += ui.LineHeight(ui.Small)
		}
	} else if p.loading && room > 20 {
		s.Loading(y+4, "Loading")
	}
	if n > 0 {
		p.list.Draw(s, image.Rect(x-6, listTop, ui.W-26, contentBottom))
	}
}

func (p *detailPage) handle(a *app, ev ui.Event) bool {
	it := p.list.Selected()
	switch ev.Button {
	case ui.Y:
		if p.it.Overview != "" {
			a.push(&textPage{title: p.it.Name, text: p.it.Overview})
		}
		return true
	case ui.Left, ui.Right:
		if it != nil && (it.Value == actAudio || it.Value == actSubs) {
			p.change(a, it.Value.(action), ev.Button == ui.Right)
			return true
		}
		return false
	case ui.Start:
		p.playNow(a, false)
		return true
	case ui.A:
		if it == nil {
			return true
		}
		switch v := it.Value.(type) {
		case Item:
			a.push(newSeason(a, v))
		case action:
			switch v {
			case actPlay:
				p.playNow(a, false)
			case actRestart:
				p.playNow(a, true)
			case actAudio, actSubs:
				p.change(a, v, true)
			case actDownload:
				p.download(a)
			case actNext:
				if p.next != nil {
					a.push(newDetail(a, *p.next))
				}
			}
		}
		return true
	}
	return p.list.Handle(ev)
}

// playNow plays the item (from the start when restart is set).
func (p *detailPage) playNow(a *app, restart bool) {
	if !p.it.IsPlayable() {
		if p.next != nil {
			n := *p.next
			au, su := pickStreams(n, nil, nil, a.cfg.Subtitles)
			a.play(n, n.Resume(), au, su)
		}
		return
	}
	start := p.it.Resume()
	if restart || p.it.Played() {
		start = 0
	}
	au, su := p.tracks(a)
	a.play(p.it, start, au, su)
}

// change steps the audio or subtitle track.
func (p *detailPage) change(a *app, what action, forward bool) {
	step := func(n, i int) int {
		if forward {
			return (i + 1) % n
		}
		return (i - 1 + n) % n
	}
	au, su := p.tracks(a)
	if what == actAudio {
		as := streamsOf(p.it, "Audio")
		i := 0
		for j, s := range as {
			if s.Index == au {
				i = j
			}
		}
		v := as[step(len(as), i)].Index
		p.audio = &v
		return
	}
	subs := p.subChoices(a)
	i := 0
	for j, s := range subs {
		if s.index == su {
			i = j
		}
	}
	v := subs[step(len(subs), i)].index
	p.sub = &v
}

// download starts, pauses or deletes the item's download.
func (p *detailPage) download(a *app) {
	dl, ok := a.dl.Get(p.it.ID)
	switch {
	case !ok || dl.State == dlFailed || dl.State == dlDamaged:
		if a.offline {
			a.scr.Notice(a.input, "Offline", "Downloads need the server.")
			return
		}
		if ok {
			a.dl.Remove(dl.ID)
		}
		if err := a.dl.Add(p.it); err != nil {
			a.scr.Notice(a.input, "Can't download it", errText(err))
		}
	case dl.State == dlDone:
		if a.scr.Confirm(a.input, "Delete the download?", sizeText(dl.Bytes())+" on the card. It can be downloaded again.", "Delete") {
			a.dl.Remove(dl.ID)
		}
	default:
		a.dl.Toggle(dl.ID)
	}
}

// textPage shows a long text (an overview), Up and Down scrolling it.
type textPage struct {
	title string
	text  string
	first int
}

func (t *textPage) busy() bool { return false }

func (t *textPage) draw(a *app) {
	s := a.scr
	top := s.StaticFrame(t.title, "", ui.Hint{Button: "UP/DOWN", Label: "Scroll"}, ui.Hint{Button: "B", Label: "Back"})
	lines := ui.Wrap(ui.Body, t.text, ui.W-60)
	lh := ui.LineHeight(ui.Body)
	fit := (contentBottom - top - 8) / lh
	t.first = max(0, min(t.first, len(lines)-fit))
	r := image.Rect(20, top, ui.W-20, contentBottom-4)
	s.Panel(r, 14)
	for i := 0; i < fit && t.first+i < len(lines); i++ {
		s.Text(32, top+8+i*lh, lines[t.first+i], ui.Body, ui.Text)
	}
}

func (t *textPage) handle(a *app, ev ui.Event) bool {
	switch ev.Button {
	case ui.Up:
		t.first = max(0, t.first-3)
	case ui.Down:
		t.first += 3
	default:
		return false
	}
	return true
}
