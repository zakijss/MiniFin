package main

import (
	"fmt"
	"image"
	"image/color"
	"math"
	"strings"

	"github.com/zakijss/minideck/ui"
	"golang.org/x/image/font"
	"golang.org/x/image/math/fixed"
)

// contentBottom is the lowest y content may use: the footer bar sits
// below it.
const contentBottom = ui.H - 56

// runtimeText writes a length: "2 h 12 min", "42 min".
func runtimeText(sec float64) string {
	m := int(math.Round(sec / 60))
	switch {
	case m <= 0:
		return ""
	case m >= 60:
		if m%60 == 0 {
			return fmt.Sprintf("%d h", m/60)
		}
		return fmt.Sprintf("%d h %d min", m/60, m%60)
	}
	return fmt.Sprintf("%d min", m)
}

// leftText is how much is left of a video started: "38 min left".
func leftText(it Item) string {
	rt, at := it.Runtime(), it.Resume()
	if rt <= 0 || at <= 0 || at >= rt {
		return ""
	}
	return runtimeText(rt-at) + " left"
}

// progressOf is how far into it the user is, 0 when not started.
func progressOf(it Item) float64 {
	if it.UserData == nil || it.Played() {
		return 0
	}
	if it.UserData.PlayedPercentage > 0 {
		return min(1, it.UserData.PlayedPercentage/100)
	}
	if rt := it.Runtime(); rt > 0 {
		return min(1, it.Resume()/rt)
	}
	return 0
}

// metaLine is an item's facts on one line: "2019 · 2 h 12 min · PG-13 ·
// 7.8".
func metaLine(it Item) string {
	var parts []string
	if it.Type == "Episode" {
		if c := episodeCode(it); c != "" {
			parts = append(parts, c)
		}
	} else if it.ProductionYear > 0 {
		y := fmt.Sprint(it.ProductionYear)
		if it.Type == "Series" && len(it.EndDate) >= 4 && it.EndDate[:4] != y {
			y += "-" + it.EndDate[:4]
		}
		parts = append(parts, y)
	}
	if rt := runtimeText(it.Runtime()); rt != "" && it.Type != "Series" {
		parts = append(parts, rt)
	}
	if it.Type == "Series" && it.ChildCount > 0 {
		parts = append(parts, plural(it.ChildCount, "season"))
	}
	if it.OfficialRating != "" {
		parts = append(parts, it.OfficialRating)
	}
	if it.CommunityRating > 0 {
		parts = append(parts, fmt.Sprintf("%.1f/10", it.CommunityRating))
	}
	return strings.Join(parts, " · ")
}

// stateLine says where the user is with it: "Watched", "38 min left".
func stateLine(it Item) string {
	switch {
	case it.Played():
		return "Watched"
	case leftText(it) != "":
		return leftText(it)
	case it.UserData != nil && it.UserData.UnplayedItemCount > 0:
		return plural(it.UserData.UnplayedItemCount, "episode") + " to watch"
	}
	return ""
}

func plural(n int, word string) string {
	if n == 1 {
		return "1 " + word
	}
	return fmt.Sprintf("%s %ss", thousands(n), word)
}

// thousands writes 20000 as 20,000.
func thousands(n int) string {
	s := fmt.Sprint(n)
	for i := len(s) - 3; i > 0; i -= 3 {
		s = s[:i] + "," + s[i:]
	}
	return s
}

// sizeText writes a size: "820 MB", "1.4 GB".
func sizeText(n int64) string {
	switch {
	case n >= 1<<30:
		return strings.TrimSuffix(fmt.Sprintf("%.1f", float64(n)/(1<<30)), ".0") + " GB"
	case n >= 1<<20:
		return fmt.Sprintf("%d MB", n>>20)
	case n > 0:
		return "Under 1 MB"
	}
	return "0 MB"
}

// wrapLines wraps text to w in f, at most max lines, the last one cut
// with an ellipsis if more is left.
func wrapLines(f font.Face, text string, w, max int) []string {
	lines := ui.Wrap(f, strings.Join(strings.Fields(text), " "), w)
	if max > 0 && len(lines) > max {
		last := strings.Join(lines[max-1:], " ")
		lines = append(lines[:max-1], ui.Fit(f, last, w))
		if !strings.HasSuffix(lines[max-1], "…") {
			lines[max-1] = ui.Fit(f, lines[max-1]+"…", w)
		}
	}
	return lines
}

// centerText is the y to draw text in f at so it sits in the middle of a
// row centred on cy.
func centerText(f font.Face, cy int) int {
	m := f.Metrics()
	return cy - m.Ascent.Ceil() + m.CapHeight.Ceil()/2
}

// drawPicture draws a picture card at r: the picture (or a panel with the
// name while it loads, or when there is none), how far in the user is,
// and a tick for one watched. A focused card sits in the selection's
// frame.
func drawPicture(s *ui.Screen, r image.Rectangle, img image.Image, name string, focused bool, progress float64, watched bool) {
	if focused {
		s.Focus(r.Inset(-5), 10)
	} else {
		s.Shadow(r, 8)
	}
	if img != nil {
		s.RoundPicture(img, r, 6)
	} else {
		s.PanelTint(r, 6, 0.13)
		lh := ui.LineHeight(ui.Small)
		lines := wrapLines(ui.Small, name, r.Dx()-12, max(1, (r.Dy()-12)/lh))
		y := r.Min.Y + (r.Dy()-len(lines)*lh)/2
		for _, l := range lines {
			s.TextCenter((r.Min.X+r.Max.X)/2, y, l, ui.Small, ui.Muted)
			y += lh
		}
	}
	if progress > 0 {
		bar := image.Rect(r.Min.X+6, r.Max.Y-10, r.Max.X-6, r.Max.Y-5)
		s.FillAlpha(bar.Inset(-1), color.Black, 0.55)
		s.Bar(bar, progress, ui.Accent)
	}
	if watched {
		c := image.Point{r.Max.X - 13, r.Min.Y + 13}
		s.Circle(float64(c.X), float64(c.Y), 10, ui.Good)
		s.Icon("check", c.X-9, c.Y-9, 18, ui.BG)
	}
}

// bigText draws text n times the size of f, centred on cx, with hard pixel
// edges (the pixel fonts scale cleanly): the Quick Connect code.
func bigText(s *ui.Screen, cx, y int, text string, f font.Face, n int, c color.RGBA) {
	w := ui.Measure(f, text)
	h := ui.LineHeight(f)
	tmp := image.NewAlpha(image.Rect(0, 0, w+2, h+2))
	d := &font.Drawer{Dst: tmp, Src: image.Opaque, Face: f, Dot: fixed.P(0, f.Metrics().Ascent.Ceil())}
	d.DrawString(text)
	x0 := cx - w*n/2
	b := s.Bounds()
	for ty := 0; ty < tmp.Rect.Dy(); ty++ {
		for tx := 0; tx < tmp.Rect.Dx(); tx++ {
			a := tmp.AlphaAt(tx, ty).A
			if a == 0 {
				continue
			}
			for dy := 0; dy < n; dy++ {
				for dx := 0; dx < n; dx++ {
					px, py := x0+tx*n+dx, y+ty*n+dy
					if image.Pt(px, py).In(b) {
						s.SetRGBA(px, py, blend(s.RGBAAt(px, py), c, a))
					}
				}
			}
		}
	}
}

func blend(dst, src color.RGBA, a uint8) color.RGBA {
	if a == 255 {
		return src
	}
	m := func(d, s uint8) uint8 { return uint8((int(d)*(255-int(a)) + int(s)*int(a)) / 255) }
	return color.RGBA{m(dst.R, src.R), m(dst.G, src.G), m(dst.B, src.B), 255}
}

// drawLoading and drawError fill an area with a loading marker or a
// message.
func drawLoading(s *ui.Screen, r image.Rectangle, text string) {
	s.Loading((r.Min.Y+r.Max.Y)/2-12, text)
}

func drawMessage(s *ui.Screen, r image.Rectangle, text string, c color.Color) {
	lines := wrapLines(ui.Body, text, r.Dx()-40, 6)
	lh := ui.LineHeight(ui.Body)
	y := (r.Min.Y+r.Max.Y)/2 - len(lines)*lh/2
	for _, l := range lines {
		s.TextCenter((r.Min.X+r.Max.X)/2, y, l, ui.Body, c)
		y += lh
	}
}
