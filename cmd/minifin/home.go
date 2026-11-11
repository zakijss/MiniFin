package main

import (
	"errors"
	"fmt"
	"image"
	"log"
	"strings"
	"sync"

	"github.com/zakijss/minideck/ui"
)

// Home is a stack of rails: Continue Watching, Next Up, Recently Added
// and the libraries (with Downloads at the end), each a row of 16:9 cards.
// Three rails show at once; Up/Down moves between rails, Left/Right along
// one.

// card is one picture on a rail.
type card struct {
	name     string
	sub      string
	key      imgKey
	file     string // a picture on the card instead (downloads)
	icon     string // drawn when there is no picture
	progress float64
	watched  bool
	open     func(a *app)
}

type rail struct {
	title   string
	cards   []card
	sel     int
	first   int // the first card shown
	loading bool
	err     string
	always  bool // shown even when empty (Libraries)
}

type homePage struct {
	rails []*rail // Continue Watching, Next Up, Recently Added, Libraries
	row   int     // the selected rail, among those shown
	top   int     // the first rail shown
	gen   int     // bumps on every reload, so old results are dropped
}

const (
	railResume = iota
	railNextUp
	railLatest
	railLibraries
)

func newHome(a *app) *homePage {
	h := &homePage{rails: []*rail{
		{title: "Continue watching"},
		{title: "Next up"},
		{title: "Recently added"},
		{title: "Libraries", always: true},
	}}
	h.refresh(a)
	return h
}

// refresh loads every rail again, keeping the cards shown until the new
// ones arrive.
func (h *homePage) refresh(a *app) {
	h.gen++
	gen := h.gen
	for _, r := range h.rails {
		r.loading = true
	}
	var mu sync.Mutex
	offline := 0
	got := func(i int, cards []card, err error) func() {
		mu.Lock()
		if isOffline(err) {
			offline++
		}
		allOffline := offline == len(h.rails)
		mu.Unlock()
		return func() {
			if gen != h.gen {
				return
			}
			r := h.rails[i]
			r.loading = false
			r.err = errText(err)
			if err == nil {
				r.cards = cards
				r.sel = min(r.sel, max(0, len(cards)-1))
			}
			if errors.Is(err, errSignedOut) {
				a.signedOut()
				return
			}
			if allOffline && a.top() == h {
				a.goOffline()
			}
		}
	}
	a.later(func() func() {
		items, err := a.c.Resume(16)
		if err == nil {
			go a.pend.flush(a.c) // the server is there: send positions from offline viewing
		}
		return got(railResume, itemCards(items, true), err)
	})
	a.later(func() func() {
		items, err := a.c.NextUp(16)
		return got(railNextUp, itemCards(items, false), err)
	})
	a.later(func() func() {
		items, err := a.c.Latest("", 20)
		return got(railLatest, itemCards(items, false), err)
	})
	a.later(func() func() {
		views, err := a.c.Views()
		var cards []card
		for _, v := range videoViews(views) {
			v := v
			cards = append(cards, card{name: v.Name, key: wideKey(v, cardW, cardH), icon: libraryIcon(v.CollectionType),
				open: func(a *app) { a.push(newLibrary(a, v)) }})
		}
		cards = append(cards, card{name: "Downloads", icon: "disc", open: func(a *app) { a.push(newDownloadsPage(false)) }})
		if err != nil {
			log.Printf("libraries: %v", err)
		}
		return got(railLibraries, cards, err)
	})
}

// videoViews are the libraries with video in them (music is MiniTape's).
func videoViews(views []Item) []Item {
	var out []Item
	for _, v := range views {
		switch v.CollectionType {
		case "music", "books", "photos", "livetv", "playlists", "audiobooks":
			continue
		}
		out = append(out, v)
	}
	return out
}

func libraryIcon(ct string) string {
	switch ct {
	case "movies":
		return "play"
	case "tvshows":
		return "list"
	case "boxsets":
		return "star"
	}
	return "disc"
}

// itemCards makes cards of items; resume ones show how far in they are.
func itemCards(items []Item, resume bool) []card {
	cards := make([]card, 0, len(items))
	for _, it := range items {
		it := it
		c := card{name: cardName(it), sub: cardSub(it), key: wideKey(it, cardW, cardH), watched: it.Played(),
			open: func(a *app) { a.open(it) }}
		if resume || it.Type == "Episode" {
			c.progress = progressOf(it)
		}
		cards = append(cards, c)
	}
	return cards
}

// cardSub is what the rail's caption says after a card's name: the
// episode's name and how much is left, or the year.
func cardSub(it Item) string {
	var parts []string
	left := leftText(it)
	switch {
	case it.Type == "Episode" && it.Name != "":
		parts = append(parts, it.Name)
	case left == "" && it.ProductionYear > 0:
		parts = append(parts, fmt.Sprint(it.ProductionYear))
	}
	if left != "" {
		parts = append(parts, left)
	}
	return strings.Join(parts, " · ")
}

// cardName is a card's caption: the show and episode for an episode.
func cardName(it Item) string {
	if it.Type == "Episode" && it.SeriesName != "" {
		if c := episodeCode(it); c != "" {
			return it.SeriesName + " " + c
		}
		return it.SeriesName
	}
	return it.Name
}

// shown are the rails with something in them (or loading).
func (h *homePage) shown() []*rail {
	var out []*rail
	for _, r := range h.rails {
		if len(r.cards) > 0 || r.always || r.loading && r.title == "Continue watching" {
			out = append(out, r)
		}
	}
	return out
}

func (h *homePage) busy() bool {
	for _, r := range h.rails {
		if r.loading && len(r.cards) == 0 {
			return true
		}
	}
	return false
}

// railHeight is one rail's height: its caption line, the cards and a gap.
func railHeight() int {
	return captionH() + 6 + cardH + 18
}

func captionH() int { return max(ui.LineHeight(ui.Caps), ui.LineHeight(ui.Small)) }

const railCards = 4 // cards on screen per rail

func (h *homePage) draw(a *app) {
	s := a.scr
	s.SetTicker("")
	top := s.StaticFrame("MiniFin", "", ui.Hint{Button: "A", Label: "Open"}, ui.Hint{Button: "X", Label: "Search"},
		ui.Hint{Button: "Y", Label: "Refresh"}, ui.Hint{Button: "START", Label: "Settings"}, ui.Hint{Button: "B", Label: "Exit"})
	rails := h.shown()
	h.row = min(h.row, len(rails)-1)
	rh := railHeight()
	fit := max(1, (contentBottom-top+18)/rh)
	if h.row < h.top {
		h.top = h.row
	}
	if h.row >= h.top+fit {
		h.top = h.row - fit + 1
	}
	h.top = max(0, min(h.top, len(rails)-fit))
	y := top
	for i := h.top; i < len(rails) && i < h.top+fit; i++ {
		h.drawRail(a, rails[i], y, i == h.row)
		y += rh
	}
	if h.top+fit < len(rails) { // more rails below
		s.Icon("chev-down", ui.W-34, contentBottom-18, 20, ui.Muted)
	}
}

// drawRail draws one rail at y: its title, and for the selected rail the
// selected card's name and facts after it, then the cards.
func (h *homePage) drawRail(a *app, r *rail, y int, focused bool) {
	s := a.scr
	capC := ui.Muted
	if focused {
		capC = ui.Text
	}
	cy := y + captionH()/2
	title := strings.ToUpper(r.title)
	s.Text(24, centerText(ui.Caps, cy), title, ui.Caps, capC)
	if focused && r.sel < len(r.cards) {
		c := r.cards[r.sel]
		x := 24 + ui.Measure(ui.Caps, title) + 14
		info := c.name
		if c.sub != "" {
			info += " · " + c.sub
		}
		s.Text(x, centerText(ui.Small, cy), ui.Fit(ui.Small, info, ui.W-24-x), ui.Small, ui.Text)
	}
	top := y + captionH() + 6
	gap := (ui.W - 48 - railCards*cardW) / (railCards - 1)
	switch {
	case len(r.cards) == 0 && r.loading:
		for i := 0; i < railCards; i++ {
			x := 24 + i*(cardW+gap)
			s.PanelTint(image.Rect(x, top, x+cardW, top+cardH), 6, 0.08)
		}
		return
	case len(r.cards) == 0 && r.err != "":
		s.Text(24, top+cardH/2-8, ui.Fit(ui.Small, r.err, ui.W-48), ui.Small, ui.Bad)
		return
	}
	if r.sel < r.first {
		r.first = r.sel
	}
	if r.sel >= r.first+railCards {
		r.first = r.sel - railCards + 1
	}
	for i := r.first; i < len(r.cards) && i < r.first+railCards; i++ {
		c := r.cards[i]
		x := 24 + (i-r.first)*(cardW+gap)
		rect := image.Rect(x, top, x+cardW, top+cardH)
		sel := focused && i == r.sel
		var img image.Image
		switch {
		case c.file != "":
			img = a.img.file(c.file)
		case c.key.id != "":
			img = a.img.get(c.key)
		}
		if img == nil && c.icon != "" {
			if sel {
				s.Focus(rect.Inset(-5), 10)
			} else {
				s.Shadow(rect, 8)
			}
			s.PanelTint(rect, 6, 0.14)
			s.Icon(c.icon, x+cardW/2-16, top+10, 32, ui.Accent)
			name := strings.ToUpper(c.name)
			s.TextCenter(x+cardW/2, top+cardH-22, ui.Fit(ui.Caps, name, cardW-12), ui.Caps, ui.Text)
			continue
		}
		drawPicture(s, rect, img, c.name, sel, c.progress, c.watched)
	}
	if r.first > 0 {
		s.Icon("chev-left", 2, top+cardH/2-10, 20, ui.Muted)
	}
	if r.first+railCards < len(r.cards) {
		s.Icon("chev-right", ui.W-22, top+cardH/2-10, 20, ui.Muted)
	}
}

func (h *homePage) handle(a *app, ev ui.Event) bool {
	rails := h.shown()
	if len(rails) == 0 {
		return false
	}
	h.row = min(h.row, len(rails)-1)
	r := rails[h.row]
	switch ev.Button {
	case ui.Up:
		h.row = max(0, h.row-1)
	case ui.Down:
		h.row = min(len(rails)-1, h.row+1)
	case ui.Left:
		r.sel = max(0, r.sel-1)
	case ui.Right:
		r.sel = min(max(0, len(r.cards)-1), r.sel+1)
	case ui.L1:
		r.sel = max(0, r.sel-railCards)
	case ui.R1:
		r.sel = min(max(0, len(r.cards)-1), r.sel+railCards)
	case ui.A:
		if r.sel < len(r.cards) && r.cards[r.sel].open != nil {
			r.cards[r.sel].open(a)
		}
	case ui.X:
		if p := newSearch(a); p != nil {
			a.push(p)
		}
	case ui.Start:
		a.push(newSettingsPage())
	case ui.Y:
		h.refresh(a)
	default:
		return false
	}
	return true
}

// open opens an item: a video's page, a show's seasons, a folder's
// contents.
func (a *app) open(it Item) {
	switch {
	case it.IsPlayable():
		a.push(newDetail(a, it))
	case it.Type == "Series":
		a.push(newDetail(a, it))
	case it.Type == "Season":
		a.push(newSeason(a, it))
	default:
		a.push(newLibrary(a, it))
	}
}
