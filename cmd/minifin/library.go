package main

import (
	"fmt"
	"image"
	"strings"
	"unicode"
	"unicode/utf8"

	"github.com/zakijss/minideck/ui"
)

// A library (or a folder, or a collection) is a grid of posters, six
// across and two rows high, with the selected one's name and facts under
// it. Only the pages of items near the selection are fetched (and kept:
// at most libKeep items), so a library of 20,000 opens as fast as one of
// 20 and scrolls without waiting.

const (
	libCols  = 6
	libRows  = 2
	libPage  = 60  // items per request (ten rows)
	libKeep  = 600 // items kept in memory, nearest the selection
	libAhead = 2   // pages fetched ahead of the selection
)

// sorts are the orders Y steps through.
var sorts = []struct {
	name, by string
	desc     bool
}{
	{"Name", "SortName", false},
	{"Date added", "DateCreated,SortName", true},
	{"Year", "ProductionYear,PremiereDate,SortName", true},
	{"Rating", "CommunityRating,SortName", true},
	{"Last watched", "DatePlayed,SortName", true},
}

type libraryPage struct {
	title string
	q     Query
	kind  string // "movies", "shows", "items": for the count line
	sort  int

	total   int // -1 until the first page arrives
	items   map[int]Item
	loading map[int]bool // pages being fetched
	err     string
	sel     int
	first   int  // the first row shown
	gen     int  // bumps when the order or filter changes
	jumping bool // waiting for the server to say where a letter starts
}

// newLibrary opens a library or folder.
func newLibrary(a *app, parent Item) *libraryPage {
	p := &libraryPage{title: parent.Name, q: Query{ParentID: parent.ID}, kind: "items"}
	switch parent.CollectionType {
	case "movies":
		p.q.Types, p.q.Recursive, p.kind = "Movie", true, "movies"
	case "tvshows":
		p.q.Types, p.q.Recursive, p.kind = "Series", true, "shows"
	case "boxsets":
		p.q.Types, p.q.Recursive, p.kind = "BoxSet", true, "collections"
	case "musicvideos":
		p.q.Types, p.q.Recursive, p.kind = "MusicVideo", true, "videos"
	}
	p.reset(a)
	return p
}

// reset forgets the items (the order or filter changed) and loads the
// first page.
func (p *libraryPage) reset(a *app) {
	p.gen++
	p.total, p.items, p.loading, p.err, p.sel, p.first = -1, map[int]Item{}, map[int]bool{}, "", 0, 0
	s := sorts[p.sort]
	p.q.SortBy, p.q.Descending = s.by, s.desc
	p.fetchAround(a)
}

func (p *libraryPage) busy() bool { return p.total < 0 && p.err == "" }

// fetchAround asks for the pages near the selection that aren't here, and
// lets go of the ones far from it.
func (p *libraryPage) fetchAround(a *app) {
	cur := p.sel / libPage
	for pg := max(0, cur-1); pg <= cur+libAhead; pg++ {
		start := pg * libPage
		if p.total >= 0 && start >= p.total {
			break
		}
		if _, ok := p.items[start]; ok || p.loading[pg] {
			continue
		}
		p.fetchPage(a, pg)
	}
}

// refresh reads the pages on hand again after a video played, so watched
// marks and progress bars are current; the grid stays where it is.
func (p *libraryPage) refresh(a *app) {
	if p.total < 0 || a.offline {
		return
	}
	for pg := range p.pages() {
		if !p.loading[pg] {
			p.fetchPage(a, pg)
		}
	}
}

// pages are the pages with items in memory.
func (p *libraryPage) pages() map[int]bool {
	out := map[int]bool{}
	for i := range p.items {
		out[i/libPage] = true
	}
	return out
}

// fetchPage asks for page pg (in the background).
func (p *libraryPage) fetchPage(a *app, pg int) {
	p.loading[pg] = true
	gen, q := p.gen, p.q
	a.later(func() func() {
		res, err := a.c.Items(q, pg*libPage, libPage)
		return func() {
			if gen != p.gen {
				return
			}
			delete(p.loading, pg)
			if err != nil {
				p.err = errText(err)
				a.failed(err)
				if isOffline(err) && p.total < 0 {
					a.goOffline()
				}
				return
			}
			p.err = ""
			p.total = res.TotalRecordCount
			for i, it := range res.Items {
				p.items[pg*libPage+i] = it
			}
			if len(res.Items) == 0 && p.total < 0 {
				p.total = 0
			}
			p.prune()
			p.fetchAround(a) // the next pages, now that the total is known
		}
	})
}

// prune keeps the libKeep items nearest the selection.
func (p *libraryPage) prune() {
	if len(p.items) <= libKeep {
		return
	}
	for i := range p.items {
		if i < p.sel-libKeep/2 || i > p.sel+libKeep/2 {
			delete(p.items, i)
		}
	}
}

func (p *libraryPage) countLine() string {
	if p.total < 0 {
		return "Loading"
	}
	n := plural(p.total, strings.TrimSuffix(p.kind, "s"))
	if p.kind == "shows" {
		n = plural(p.total, "show")
	}
	line := n + " · " + sorts[p.sort].name
	if p.q.Unplayed {
		line += " · Unwatched"
	}
	return line
}

func (p *libraryPage) draw(a *app) {
	s := a.scr
	s.SetTicker("")
	top := s.StaticFrame(p.title, p.countLine(), ui.Hint{Button: "A", Label: "Open"}, ui.Hint{Button: "Y", Label: "Sort"},
		ui.Hint{Button: "X", Label: "Unwatched"}, ui.Hint{Button: "L2/R2", Label: "A-Z"}, ui.Hint{Button: "B", Label: "Back"})
	area := image.Rect(20, top, ui.W-20, contentBottom)
	switch {
	case p.total < 0 && p.err != "":
		drawMessage(s, area, p.err, ui.Muted)
		return
	case p.total < 0:
		drawLoading(s, area, "Loading")
		return
	case p.total == 0:
		msg := "Nothing here yet. Videos added in Jellyfin show up here."
		if p.q.Unplayed {
			msg = "You've watched everything here. X shows it all again."
		}
		drawMessage(s, area, msg, ui.Muted)
		return
	}
	p.sel = max(0, min(p.sel, p.total-1))
	row := p.sel / libCols
	if row < p.first {
		p.first = row
	}
	if row >= p.first+libRows {
		p.first = row - libRows + 1
	}
	gap := (ui.W - 52 - libCols*posterW) / (libCols - 1)
	y0 := top + 6
	for r := 0; r < libRows; r++ {
		for c := 0; c < libCols; c++ {
			i := (p.first+r)*libCols + c
			if i >= p.total {
				break
			}
			x := 26 + c*(posterW+gap)
			y := y0 + r*(posterH+14)
			rect := image.Rect(x, y, x+posterW, y+posterH)
			it, ok := p.items[i]
			if !ok {
				s.PanelTint(rect, 6, 0.08)
				if i == p.sel {
					s.Focus(rect.Inset(-5), 10)
					s.PanelTint(rect, 6, 0.08)
				}
				continue
			}
			drawPicture(s, rect, a.img.get(posterKey(it, posterW, posterH)), it.Name, i == p.sel,
				progressOf(it), it.Played() || it.UserData != nil && it.Type == "Series" && it.UserData.Played)
		}
	}
	// the scroll position, at the right
	rows := (p.total + libCols - 1) / libCols
	if rows > libRows {
		track := image.Rect(ui.W-14, y0, ui.W-10, y0+libRows*(posterH+14)-14)
		s.FillAlpha(track, ui.Text, 0.12)
		h := max(16, track.Dy()*libRows/rows)
		ty := track.Min.Y + (track.Dy()-h)*p.first/max(1, rows-libRows)
		s.Fill(image.Rect(track.Min.X, ty, track.Max.X, ty+h), ui.Accent)
	}
	// the selected item
	y := y0 + libRows*(posterH+14) + 2
	w := ui.W - 52
	letter := ""
	if p.sort == 0 && !p.q.Descending {
		letter = p.letterAt(p.sel)
	}
	if letter != "" {
		box := image.Rect(ui.W-26-34, y, ui.W-26, y+34)
		s.Panel(box, 8)
		s.TextCenter(box.Min.X+17, centerText(ui.Heading, box.Min.Y+17), letter, ui.Heading, ui.Accent)
		w -= 46
	}
	if it, ok := p.items[p.sel]; ok {
		s.Text(26, y, ui.Fit(ui.Heading, it.Name, w), ui.Heading, ui.Text)
		s.Text(26, y+ui.LineHeight(ui.Heading)+2, ui.Fit(ui.Small, stripLine(it, w), w), ui.Small, ui.Muted)
	} else if p.err != "" {
		s.Text(26, y, ui.Fit(ui.Small, p.err, w), ui.Small, ui.Bad)
	} else {
		s.Text(26, y, "Loading", ui.Heading, ui.Faint)
	}
}

// stripLine is the facts under the grid, the ratings left out when they
// don't fit with where the user is.
func stripLine(it Item, w int) string {
	join := func(it Item) string {
		info := metaLine(it)
		if st := stateLine(it); st != "" {
			info = strings.TrimPrefix(info+" · "+st, " · ")
		}
		return info
	}
	line := join(it)
	if ui.Measure(ui.Small, line) > w {
		it.OfficialRating, it.CommunityRating = "", 0
		line = join(it)
	}
	return line
}

// letterAt is the letter the item at i sorts under, "#" for digits and
// signs, "" when it isn't loaded.
func (p *libraryPage) letterAt(i int) string {
	it, ok := p.items[i]
	if !ok {
		return ""
	}
	return sortLetter(it)
}

func sortLetter(it Item) string {
	name := it.SortName
	if name == "" {
		name = it.Name
	}
	r, _ := utf8.DecodeRuneInString(name)
	if r = unicode.ToUpper(r); r >= 'A' && r <= 'Z' {
		return string(r)
	}
	return "#"
}

const letters = "#ABCDEFGHIJKLMNOPQRSTUVWXYZ"

func (p *libraryPage) handle(a *app, ev ui.Event) bool {
	if p.total <= 0 {
		switch ev.Button {
		case ui.Y, ui.X:
		default:
			return false
		}
	}
	last := max(0, p.total-1)
	switch ev.Button {
	case ui.Left:
		p.sel = max(0, p.sel-1)
	case ui.Right:
		p.sel = min(last, p.sel+1)
	case ui.Up:
		if p.sel >= libCols {
			p.sel -= libCols
		}
	case ui.Down:
		if p.sel+libCols <= last {
			p.sel += libCols
		} else if p.sel/libCols < last/libCols {
			p.sel = last // the last row is short
		}
	case ui.L1:
		p.sel = max(0, p.sel-libCols*libRows)
	case ui.R1:
		p.sel = min(last, p.sel+libCols*libRows)
	case ui.L2, ui.R2:
		p.jump(a, ev.Button == ui.R2)
	case ui.Y:
		p.sort = (p.sort + 1) % len(sorts)
		p.reset(a)
		return true
	case ui.X:
		p.q.Unplayed = !p.q.Unplayed
		p.reset(a)
		return true
	case ui.A:
		if it, ok := p.items[p.sel]; ok {
			a.open(it)
		}
		return true
	default:
		return false
	}
	p.fetchAround(a)
	return true
}

// jump moves to the next (or previous) letter. Sorted by name, the server
// counts the items before the letter (NameLessThan), which is where it
// starts; in any other order L2 and R2 go to the start and the end.
func (p *libraryPage) jump(a *app, forward bool) {
	if p.sort != 0 || p.q.Descending {
		if forward {
			p.sel = max(0, p.total-1)
		} else {
			p.sel = 0
		}
		return
	}
	if p.jumping {
		return
	}
	cur := p.letterAt(p.sel)
	if cur == "" {
		return
	}
	i := strings.Index(letters, cur)
	target := ""
	switch {
	case forward && i+1 < len(letters):
		target = letters[i+1 : i+2]
	case forward:
		p.sel = max(0, p.total-1)
		return
	default:
		// back to the start of this letter, or of the one before when
		// already there
		prev := ""
		if p.sel > 0 {
			prev = p.letterAt(p.sel - 1)
		}
		switch {
		case p.sel > 0 && (prev == "" || prev == cur):
			target = cur
		case i == 0:
			p.sel = 0
			return
		default:
			target = letters[i-1 : i]
		}
	}
	if target == "#" {
		p.sel = 0
		return
	}
	p.jumping = true
	gen, q := p.gen, p.q
	a.later(func() func() {
		n, err := a.c.CountBefore(q, strings.ToLower(target))
		return func() {
			p.jumping = false
			if gen != p.gen || err != nil {
				if err != nil {
					p.err = errText(err)
				}
				return
			}
			if n >= p.total { // no item under that letter: stay at the end
				n = p.total - 1
			}
			if !forward && n == p.sel && target != "A" {
				// already at the start of that letter: one more back
				p.sel = max(0, n-1)
			} else {
				p.sel = max(0, n)
			}
			p.fetchAround(a)
		}
	})
}

// String names the page for logs and tests.
func (p *libraryPage) String() string {
	return fmt.Sprintf("library %q: %d of %d", p.title, len(p.items), p.total)
}
