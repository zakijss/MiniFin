package main

import (
	"fmt"
	"image"
	"path/filepath"
	"strings"

	"github.com/zakijss/minideck/ui"
)

// Pages that are a list: a season's episodes, search results, downloads.

// listThumbSize is the side of the thumbnails ui.List draws in a list of n
// rows with pictures, h pixels high (its rows are 66 px, stretched by up
// to 10 to fill h, less 22): pictures are fetched at exactly that size.
func listThumbSize(h, n int) int {
	const base = 66
	rows := max(1, (h+base/3)/base)
	rh := h / rows
	if rh > base+10 || n < rows {
		rh = base
	}
	return rh - 22
}

// listArea is where a list page's list goes.
func listArea(top int) image.Rectangle { return image.Rect(20, top, ui.W-26, contentBottom) }

// itemRows makes list rows of items, with square pictures of size th.
func itemRows(a *app, items []Item, th *int, sub func(Item) string) []ui.Item {
	rows := make([]ui.Item, len(items))
	for i, it := range items {
		it := it
		rows[i] = ui.Item{Label: it.Name, Sub: sub(it), Value: it, Image: func() image.Image {
			k := wideKey(it, *th, *th)
			if it.Type != "Episode" {
				k = posterKey(it, *th, *th)
			}
			return a.img.get(k)
		}}
	}
	return rows
}

// ---- a season ----

type seasonPage struct {
	season  Item
	items   []Item
	list    ui.List
	loading bool
	err     string
	th      int
	gen     int
}

func newSeason(a *app, season Item) *seasonPage {
	p := &seasonPage{season: season, th: 44}
	p.refresh(a)
	return p
}

func (p *seasonPage) refresh(a *app) {
	p.loading = true
	p.gen++
	gen := p.gen
	sid := p.season.SeriesID
	if sid == "" {
		sid = p.season.ParentThumbItemID
	}
	a.later(func() func() {
		items, err := a.c.Episodes(sid, p.season.ID)
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
			p.items, p.err = items, ""
			sel := p.list.Sel
			p.list.Items = itemRows(a, items, &p.th, func(it Item) string { return episodeSub(a, it) })
			for i := range p.list.Items {
				p.list.Items[i].Label = episodeLabel(items[i])
			}
			if sel == 0 { // open at the first episode not watched yet
				for i, it := range items {
					if !it.Played() {
						sel = i
						break
					}
				}
			}
			p.list.Sel = min(sel, max(0, len(items)-1))
		}
	})
}

// episodeLabel is "3. Name".
func episodeLabel(it Item) string {
	if it.IndexNumber > 0 {
		return fmt.Sprintf("%d. %s", it.IndexNumber, it.Name)
	}
	return it.Name
}

// episodeSub is an episode's second line: its length and where the user
// is, and whether it is on the card.
func episodeSub(a *app, it Item) string {
	parts := []string{}
	if rt := runtimeText(it.Runtime()); rt != "" {
		parts = append(parts, rt)
	}
	if st := stateLine(it); st != "" {
		parts = append(parts, st)
	}
	if dl, ok := a.dl.Get(it.ID); ok {
		if dl.State == dlDone {
			parts = append(parts, "On the card")
		} else if dl.State == dlRunning || dl.State == dlQueued {
			parts = append(parts, "Downloading")
		}
	}
	return strings.Join(parts, " · ")
}

func (p *seasonPage) busy() bool { return p.loading && len(p.items) == 0 }

func (p *seasonPage) draw(a *app) {
	s := a.scr
	s.SetTicker("")
	sub := p.season.SeriesName
	if n := len(p.items); n > 0 {
		sub = strings.TrimPrefix(sub+" · "+plural(n, "episode"), " · ")
	}
	hints := []ui.Hint{{Button: "A", Label: "Open"}, {Button: "START", Label: "Play"}, {Button: "X", Label: "Download"}}
	if sel := p.list.Selected(); sel != nil && !a.offline {
		// a toggle; the full "Mark watched" doesn't fit next to Download
		hints = append(hints, ui.Hint{Button: "Y", Label: "Watched"})
	}
	top := s.StaticFrame(p.season.Name, sub, append(hints, ui.Hint{Button: "B", Label: "Back"})...)
	area := listArea(top)
	switch {
	case p.loading && len(p.items) == 0:
		drawLoading(s, area, "Loading")
	case p.err != "" && len(p.items) == 0:
		drawMessage(s, area, p.err, ui.Muted)
	case len(p.items) == 0:
		drawMessage(s, area, "No episodes here yet. Episodes added in Jellyfin show up here.", ui.Muted)
	default:
		for i, it := range p.items { // downloads move on while the page shows
			p.list.Items[i].Sub = episodeSub(a, it)
		}
		p.th = listThumbSize(area.Dy(), len(p.items))
		p.list.Draw(s, area)
	}
}

func (p *seasonPage) handle(a *app, ev ui.Event) bool {
	sel := p.list.Selected()
	switch ev.Button {
	case ui.A:
		if sel != nil {
			a.push(newDetail(a, sel.Value.(Item)))
		}
		return true
	case ui.Start:
		if sel != nil {
			it := sel.Value.(Item)
			au, su := pickStreams(it, nil, nil, a.cfg.Subtitles)
			start := it.Resume()
			if it.Played() {
				start = 0
			}
			a.play(it, start, au, su)
		}
		return true
	case ui.Y:
		if sel != nil && !a.offline {
			it := sel.Value.(Item)
			a.markPlayed(it, !it.Played())
		}
		return true
	case ui.X:
		if sel != nil && !a.offline {
			it := sel.Value.(Item)
			if dl, ok := a.dl.Get(it.ID); ok && dl.State != dlFailed && dl.State != dlDamaged {
				a.scr.Notice(a.input, "Already downloading", "It's in Downloads, on Home.")
				return true
			}
			a.dl.Remove(it.ID)
			if err := a.dl.Add(it); err != nil {
				a.scr.Notice(a.input, "Can't download it", errText(err))
			}
		}
		return true
	}
	return p.list.Handle(ev)
}

// ---- search ----

type searchPage struct {
	query   string
	items   []Item
	list    ui.List
	loading bool
	err     string
	th      int
	gen     int
}

// newSearch asks for the words, then searches; nil when cancelled.
func newSearch(a *app) page {
	q, ok := a.scr.Prompt(a.input, "Search", "", "A movie, show or episode", false)
	if !ok || strings.TrimSpace(q) == "" {
		return nil
	}
	p := &searchPage{query: q, th: 44}
	p.search(a)
	return p
}

func (p *searchPage) search(a *app) {
	p.loading, p.err = true, ""
	p.gen++
	gen, q := p.gen, p.query
	a.later(func() func() {
		res, err := a.c.Items(Query{Search: q, Types: "Movie,Series,Episode", Recursive: true, SortBy: "SortName"}, 0, 60)
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
			p.items = res.Items
			p.list = ui.List{Items: itemRows(a, res.Items, &p.th, searchSub)}
		}
	})
}

// searchSub says what a result is: "Movie · 1999", "Episode · Show S1:E3".
func searchSub(it Item) string {
	switch it.Type {
	case "Episode":
		return strings.TrimSpace("Episode · " + it.SeriesName + " " + episodeCode(it))
	case "Series":
		if it.ProductionYear > 0 {
			return fmt.Sprintf("Show · %d", it.ProductionYear)
		}
		return "Show"
	case "Movie":
		if it.ProductionYear > 0 {
			return fmt.Sprintf("Movie · %d", it.ProductionYear)
		}
		return "Movie"
	}
	return it.Type
}

func (p *searchPage) busy() bool { return p.loading }

func (p *searchPage) draw(a *app) {
	s := a.scr
	s.SetTicker("")
	sub := `"` + p.query + `"`
	if !p.loading && p.err == "" {
		sub = plural(len(p.items), "result") + " for " + sub
	}
	top := s.StaticFrame("Search", sub, ui.Hint{Button: "A", Label: "Open"}, ui.Hint{Button: "X", Label: "New search"}, ui.Hint{Button: "B", Label: "Back"})
	area := listArea(top)
	switch {
	case p.loading:
		drawLoading(s, area, "Searching")
	case p.err != "":
		drawMessage(s, area, p.err, ui.Muted)
	case len(p.items) == 0:
		drawMessage(s, area, "Nothing matches. X searches again.", ui.Muted)
	default:
		p.th = listThumbSize(area.Dy(), len(p.items))
		p.list.Draw(s, area)
	}
}

func (p *searchPage) handle(a *app, ev ui.Event) bool {
	switch ev.Button {
	case ui.A:
		if sel := p.list.Selected(); sel != nil {
			a.open(sel.Value.(Item))
		}
		return true
	case ui.X:
		if q, ok := a.scr.Prompt(a.input, "Search", p.query, "A movie, show or episode", false); ok && strings.TrimSpace(q) != "" {
			p.query = q
			p.search(a)
		}
		return true
	}
	return p.list.Handle(ev)
}

// ---- downloads ----

// downloadsPage lists the downloads. Offline, it is the whole app: what's
// on the card plays, and Y tries the server again.
type downloadsPage struct {
	offline  bool
	list     ui.List
	checking bool
	note     string
}

func newDownloadsPage(offline bool) *downloadsPage { return &downloadsPage{offline: offline} }

func (p *downloadsPage) busy() bool { return p.checking }

// dlStatus is a download's second line.
func dlStatus(dl Download) string {
	switch dl.State {
	case dlDone:
		s := "Ready · " + sizeText(dl.Bytes())
		if dl.Played {
			s += " · Watched"
		} else if dl.Position > 0 {
			s += " · " + runtimeText(dl.Item.Runtime()-dl.Position) + " left"
		}
		return s
	case dlRunning:
		return fmt.Sprintf("Downloading · %d %% of %s", int(dl.Progress()*100), sizeText(dl.Need))
	case dlQueued:
		if dl.Reason != "" {
			return dl.Reason
		}
		return "Waiting to download"
	case dlPaused:
		if dl.Reason == errNoSpace.Error() {
			return fmt.Sprintf("Paused at %d %%: the card is full", int(dl.Progress()*100))
		}
		if dl.Reason != "" && dl.Reason != "Paused" {
			return dl.Reason
		}
		return fmt.Sprintf("Paused at %d %%", int(dl.Progress()*100))
	case dlDamaged:
		return dl.Reason
	}
	return "Failed: " + dl.Reason
}

func (p *downloadsPage) rows(a *app) []ui.Item {
	list := a.dl.List()
	out := make([]ui.Item, len(list))
	for i, dl := range list {
		path := filepath.Join(dl.dir, "poster.png")
		out[i] = ui.Item{Label: title(dl.Item), Sub: dlStatus(dl), Value: dl.ID,
			Image: func() image.Image { return a.img.file(path) }}
		if dl.State == dlFailed || dl.State == dlDamaged {
			out[i].Tint = ui.Bad
		}
	}
	return out
}

func (p *downloadsPage) draw(a *app) {
	s := a.scr
	s.SetTicker("")
	sel := p.list.Sel
	p.list.Items = p.rows(a)
	p.list.Sel = min(sel, max(0, len(p.list.Items)-1))
	var used int64
	for _, dl := range a.dl.List() {
		used += dl.Bytes()
	}
	sub := sizeText(used) + " on the card"
	if free := cardFree(a.cfg.downloadDir(a.dir)); free >= 0 {
		sub += " · " + sizeText(free) + " free"
	}
	title := "Downloads"
	hints := []ui.Hint{{Button: "A", Label: "Play"}, {Button: "Y", Label: "Pause"}, {Button: "X", Label: "Delete"}, {Button: "B", Label: "Back"}}
	if p.offline {
		title = "Offline"
		sub = "Can't reach " + a.serverLabel()
		hints = []ui.Hint{{Button: "A", Label: "Play"}, {Button: "Y", Label: "Retry"}, {Button: "X", Label: "Delete"}, {Button: "START", Label: "Settings"}, {Button: "B", Label: "Exit"}}
	}
	top := s.StaticFrame(title, sub, hints...)
	area := listArea(top)
	switch {
	case p.checking:
		drawLoading(s, area, "Trying "+a.serverLabel())
	case p.note != "":
		drawMessage(s, area, p.note, ui.Muted)
	case len(p.list.Items) == 0 && p.offline:
		drawMessage(s, area, "Nothing is downloaded. Videos you download (on a video's page) play here without the network.", ui.Muted)
	case len(p.list.Items) == 0:
		drawMessage(s, area, "Nothing yet. Download a video from its page, or a whole season with X on its episodes.", ui.Muted)
	default:
		p.list.Draw(s, area)
	}
}

func (p *downloadsPage) handle(a *app, ev ui.Event) bool {
	p.note = ""
	sel := p.list.Selected()
	var dl Download
	ok := false
	if sel != nil {
		dl, ok = a.dl.Get(sel.Value.(string))
	}
	switch ev.Button {
	case ui.A:
		switch {
		case !ok:
		case dl.State == dlDone:
			_, su := pickStreams(dl.Item, nil, nil, a.cfg.Subtitles)
			start := dl.Position
			if dl.Played {
				start = 0
			}
			if su >= 0 && !hasSub(dl, su) {
				su = -1
			}
			a.play(dl.Item, start, -1, su) // the copy on the card
		case a.offline:
			a.scr.Notice(a.input, "Not finished", "It finishes downloading once the server can be reached.")
		default:
			a.push(newDetail(a, dl.Item))
		}
		return true
	case ui.X:
		if ok && a.scr.Confirm(a.input, "Delete the download?", title(dl.Item)+" ("+sizeText(dl.Bytes())+")", "Delete") {
			a.dl.Remove(dl.ID)
		}
		return true
	case ui.Y:
		if p.offline {
			p.retry(a)
		} else if ok {
			a.dl.Toggle(dl.ID)
		}
		return true
	case ui.Start:
		if p.offline {
			a.push(newSettingsPage())
			return true
		}
	}
	return p.list.Handle(ev)
}

func hasSub(dl Download, index int) bool {
	for _, s := range dl.Subs {
		if s.Index == index {
			return true
		}
	}
	return false
}

// retry checks whether the server answers again; if it does, Home.
func (p *downloadsPage) retry(a *app) {
	if !a.c.SignedIn() {
		if a.setup(false) {
			a.goOnline()
		}
		return
	}
	p.checking = true
	a.later(func() func() {
		_, err := a.c.Views()
		return func() {
			p.checking = false
			switch {
			case err == nil:
				a.goOnline()
			case isOffline(err):
				p.note = "Still can't reach " + a.serverLabel() + ": " + errText(err)
			default:
				a.failed(err)
				p.note = errText(err)
			}
		}
	})
}
