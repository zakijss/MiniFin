package main

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log"
	"net/http"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"sync"
	"syscall"
	"time"

	"github.com/zakijss/minideck/atomicfile"
)

// A download is the same transcode a stream plays, kept on the card: a
// folder per video with meta.json, the parts (see ts.go), the poster and
// any text subtitles as SRT. Downloads run one at a time while MiniFin is
// open, and an interrupted one goes on from its last keyframe.

// dlPart is one piece of a download.
type dlPart struct {
	File     string     `json:"file"`
	Start90  int64      `json:"start90"` // where in the video it begins, in 90 kHz ticks
	Size     int64      `json:"size"`    // bytes that play (an interrupted part is cut at a keyframe)
	FirstPTS int64      `json:"first_pts"`
	EndPTS   int64      `json:"end_pts"` // the timestamp at Size
	Keys     []keyPoint `json:"keys"`
	Done     bool       `json:"done"`
}

// end90 is where the part ends in the video, in 90 kHz ticks.
func (p dlPart) end90() int64 { return p.Start90 + max(0, ptsDiff(p.EndPTS, p.FirstPTS)) }

// dlSub is a subtitle track kept as SRT.
type dlSub struct {
	Index int    `json:"index"`
	Name  string `json:"name"`
	File  string `json:"file"`
}

// Download states.
const (
	dlQueued  = "queued"
	dlRunning = "downloading"
	dlPaused  = "paused"
	dlDone    = "done"
	dlFailed  = "failed"
	dlDamaged = "damaged"
)

// Download is one video kept for offline viewing.
type Download struct {
	ID     string   `json:"id"`
	Item   Item     `json:"item"`
	Kbps   int      `json:"kbps"`
	State  string   `json:"state"`
	Reason string   `json:"reason,omitempty"`
	Parts  []dlPart `json:"parts,omitempty"`
	Subs   []dlSub  `json:"subs,omitempty"`
	// SubsChecked: the item's tracks have been read (an episode queued
	// from a season's list comes without them).
	SubsChecked bool      `json:"subs_checked,omitempty"`
	Need        int64     `json:"need"` // the size expected when done
	Added       time.Time `json:"added"`
	Position    float64   `json:"position,omitempty"` // where it was last stopped
	Played      bool      `json:"played,omitempty"`

	dir string
}

// Covered is how many seconds of the video are on the card.
func (d Download) Covered() float64 {
	if len(d.Parts) == 0 {
		return 0
	}
	return float64(d.Parts[len(d.Parts)-1].end90()) / ptsHz
}

// Bytes is how much of the card it uses.
func (d Download) Bytes() int64 {
	var n int64
	for _, p := range d.Parts {
		n += p.Size
	}
	return n
}

// Progress is how far along it is, 0 to 1.
func (d Download) Progress() float64 {
	if d.State == dlDone {
		return 1
	}
	if rt := d.Item.Runtime(); rt > 0 {
		return min(1, d.Covered()/rt)
	}
	return 0
}

// downloads keeps the list and runs the downloads.
type downloads struct {
	c       *Client
	kbps    func() int
	changed func()                 // the list changed: redraw
	free    func(dir string) int64 // free bytes on dir's card (tests replace it)
	retry   func(n int) time.Duration
	poster  func(it Item, path string) // saves the poster beside it (best effort)

	mu      sync.Mutex
	dir     string
	list    map[string]*Download
	active  string
	cancel  context.CancelFunc
	wake    chan struct{}
	stopped bool
	running bool          // Run is going
	ended   chan struct{} // closed when Run returns
}

// reserve is room always left free on the card, so a download never fills
// it for the other apps.
const reserve = 200 << 20

var errNoSpace = screenError("The card is full. Free some space (MiniTop > Storage) or delete a download.")

func cardFree(dir string) int64 {
	var st syscall.Statfs_t
	for d := dir; ; d = filepath.Dir(d) {
		if syscall.Statfs(d, &st) == nil {
			return int64(st.Bavail) * int64(st.Bsize)
		}
		if d == filepath.Dir(d) {
			return -1
		}
	}
}

func openDownloads(dir string, c *Client, kbps func() int) *downloads {
	d := &downloads{c: c, kbps: kbps, changed: func() {}, free: cardFree, dir: dir,
		list: map[string]*Download{}, wake: make(chan struct{}, 1), ended: make(chan struct{}),
		retry: func(n int) time.Duration { return time.Duration(min(60, 5*n*n)) * time.Second }}
	d.load()
	return d
}

// load reads every download's meta.json, checking its parts against the
// files on the card.
func (d *downloads) load() {
	ents, _ := os.ReadDir(d.dir)
	for _, e := range ents {
		if !e.IsDir() || strings.HasPrefix(e.Name(), ".") {
			continue
		}
		dl := loadDownload(filepath.Join(d.dir, e.Name()))
		d.list[dl.ID] = dl
	}
}

// loadDownload reads one download's folder. A meta.json that can't be read
// gives a damaged entry that can only be deleted; parts missing or shorter
// than recorded (the card was pulled mid-write) are cut back to their last
// keyframe still there, and the download goes on from there.
func loadDownload(dir string) *Download {
	dl := &Download{ID: filepath.Base(dir), dir: dir}
	b, err := os.ReadFile(filepath.Join(dir, "meta.json"))
	if err == nil {
		err = json.Unmarshal(b, dl)
	}
	dl.dir = dir
	if err != nil || dl.ID == "" {
		return &Download{ID: filepath.Base(dir), dir: dir, State: dlDamaged,
			Item: Item{ID: filepath.Base(dir), Name: filepath.Base(dir)}, Reason: "Its details are damaged. Delete it and download it again."}
	}
	dl.ID = filepath.Base(dir)
	for i := range dl.Parts {
		p := &dl.Parts[i]
		fi, err := os.Stat(filepath.Join(dir, p.File))
		size := int64(0)
		if err == nil {
			size = fi.Size()
		}
		if size >= p.Size {
			continue
		}
		// cut back to the last keyframe the file still has
		keep := p.Keys[:0]
		for _, k := range p.Keys {
			if k.Off < size && k.Off > 0 {
				keep = append(keep, k)
			}
		}
		if len(keep) == 0 {
			dl.Parts = dl.Parts[:i]
		} else {
			last := keep[len(keep)-1]
			p.Keys, p.Size, p.EndPTS, p.Done = keep, last.Off, last.PTS, false
			os.Truncate(filepath.Join(dir, p.File), p.Size)
			dl.Parts = dl.Parts[:i+1]
		}
		if dl.State == dlDone {
			dl.State = dlQueued
		}
		dl.Reason = ""
		log.Printf("download %s: a part was shorter than recorded; it goes on from %.0f s", dl.ID, dl.Covered())
		break
	}
	if dl.State == dlRunning {
		dl.State = dlQueued // stopped when MiniFin closed
	}
	return dl
}

// save writes dl's meta.json. The caller holds mu (or owns dl).
func (dl *Download) save() error {
	b, err := json.MarshalIndent(dl, "", " ")
	if err != nil {
		return err
	}
	return atomicfile.Write(filepath.Join(dl.dir, "meta.json"), b, 0644)
}

// List is the downloads, newest first.
func (d *downloads) List() []Download {
	d.mu.Lock()
	defer d.mu.Unlock()
	out := make([]Download, 0, len(d.list))
	for _, dl := range d.list {
		out = append(out, dl.snapshot())
	}
	sort.Slice(out, func(i, j int) bool {
		if !out[i].Added.Equal(out[j].Added) {
			return out[i].Added.After(out[j].Added)
		}
		return out[i].ID < out[j].ID
	})
	return out
}

// Get returns the download of item id.
func (d *downloads) Get(id string) (Download, bool) {
	d.mu.Lock()
	defer d.mu.Unlock()
	dl, ok := d.list[id]
	if !ok {
		return Download{}, false
	}
	return dl.snapshot(), true
}

// snapshot is a copy of dl that the download in progress won't change
// under the reader. The caller holds mu.
func (dl *Download) snapshot() Download {
	cp := *dl
	cp.Parts = append([]dlPart(nil), dl.Parts...)
	if dl.Item.UserData != nil {
		ud := *dl.Item.UserData
		cp.Item.UserData = &ud
	}
	return cp
}

// SetDir moves where new downloads go. The ones already in the old folder
// stay there, listed and playable, until deleted.
func (d *downloads) SetDir(dir string) {
	os.MkdirAll(dir, 0755)
	ents, _ := os.ReadDir(dir)
	d.mu.Lock()
	defer d.mu.Unlock()
	d.dir = dir
	for _, e := range ents {
		if e.IsDir() && !strings.HasPrefix(e.Name(), ".") {
			dl := loadDownload(filepath.Join(dir, e.Name()))
			if _, ok := d.list[dl.ID]; !ok {
				d.list[dl.ID] = dl
			}
		}
	}
}

// Add queues item it for download, at the quality in Settings. It
// refuses when the card hasn't the room for it.
func (d *downloads) Add(it Item) error {
	kbps := d.kbps()
	need := streamBytes(kbps, it.Runtime())
	d.mu.Lock()
	defer d.mu.Unlock()
	if old, ok := d.list[it.ID]; ok && old.State != dlFailed && old.State != dlDamaged {
		return nil
	}
	if free := d.free(d.dir); free >= 0 && free < need+reserve+d.pendingLocked() {
		return screenError(fmt.Sprintf("It needs about %s and the card has %s free. Free some space or pick a lower quality.",
			sizeText(need), sizeText(max(0, free-reserve))))
	}
	dir := filepath.Join(d.dir, safeName(it.ID))
	if err := os.MkdirAll(dir, 0755); err != nil {
		return err
	}
	keep := it
	keep.MediaSources = nil // the streams are re-read when it plays, if online
	dl := &Download{ID: it.ID, Item: keep, Kbps: kbps, State: dlQueued, Need: need, Added: time.Now(), dir: dir}
	dl.Subs = keepSubs(it)
	dl.SubsChecked = len(streamsOf(it, "Video")) > 0 // a full item, its tracks known
	if err := dl.save(); err != nil {
		os.RemoveAll(dir)
		return err
	}
	d.list[it.ID] = dl
	d.kick()
	return nil
}

// pendingLocked is how many bytes the queued downloads still need.
func (d *downloads) pendingLocked() int64 {
	var n int64
	for _, dl := range d.list {
		if dl.State == dlQueued || dl.State == dlRunning {
			n += max(0, dl.Need-dl.Bytes())
		}
	}
	return n
}

// keepSubs are the item's text subtitle tracks, to keep as SRT (at most
// six; image subtitles can't be kept apart from the picture).
func keepSubs(it Item) []dlSub {
	var out []dlSub
	for _, s := range streamsOf(it, "Subtitle") {
		if (s.IsTextSubtitleStream || textSubtitles[strings.ToLower(s.Codec)]) && len(out) < 6 {
			out = append(out, dlSub{Index: s.Index, Name: streamName(s), File: fmt.Sprintf("sub%d.srt", s.Index)})
		}
	}
	return out
}

// safeName keeps an item id usable as a folder name.
func safeName(id string) string {
	return strings.Map(func(r rune) rune {
		if r >= 'a' && r <= 'z' || r >= 'A' && r <= 'Z' || r >= '0' && r <= '9' || r == '-' || r == '_' {
			return r
		}
		return '_'
	}, id)
}

// Remove stops and deletes a download.
func (d *downloads) Remove(id string) error {
	d.mu.Lock()
	dl, ok := d.list[id]
	if ok && d.active == id && d.cancel != nil {
		d.cancel()
	}
	delete(d.list, id)
	d.mu.Unlock()
	if !ok {
		return nil
	}
	return os.RemoveAll(dl.dir)
}

// Toggle pauses a download, or queues a paused or failed one again.
func (d *downloads) Toggle(id string) {
	d.mu.Lock()
	defer d.mu.Unlock()
	dl, ok := d.list[id]
	if !ok {
		return
	}
	switch dl.State {
	case dlQueued, dlRunning:
		dl.State, dl.Reason = dlPaused, "Paused"
		if d.active == id && d.cancel != nil {
			d.cancel()
		}
	case dlPaused, dlFailed:
		dl.State, dl.Reason = dlQueued, ""
		d.kick()
	}
	dl.save()
}

// SetPosition keeps where offline playback stopped.
func (d *downloads) SetPosition(id string, pos float64, played bool) {
	d.mu.Lock()
	defer d.mu.Unlock()
	if dl, ok := d.list[id]; ok {
		dl.Position, dl.Played = pos, played
		if dl.Item.UserData == nil {
			dl.Item.UserData = &UserData{}
		}
		dl.Item.UserData.PlaybackPositionTicks, dl.Item.UserData.Played = toTicks(pos), played
		dl.save()
	}
}

func (d *downloads) kick() {
	select {
	case d.wake <- struct{}{}:
	default:
	}
}

// Stop ends the worker (MiniFin is closing); the running download is
// saved and goes on next time. It waits (a little) for that.
func (d *downloads) Stop() {
	d.mu.Lock()
	d.stopped = true
	running := d.running
	if d.cancel != nil {
		d.cancel()
	}
	d.mu.Unlock()
	d.kick()
	if running {
		select {
		case <-d.ended:
		case <-time.After(3 * time.Second):
		}
	}
}

// next picks the oldest queued download.
func (d *downloads) next() *Download {
	var best *Download
	for _, dl := range d.list {
		if dl.State == dlQueued && (best == nil || dl.Added.Before(best.Added)) {
			best = dl
		}
	}
	return best
}

// Run downloads the queue, one at a time, until Stop.
func (d *downloads) Run() {
	d.mu.Lock()
	if d.running || d.stopped {
		d.mu.Unlock()
		return
	}
	d.running = true
	d.mu.Unlock()
	defer close(d.ended)
	fails := 0
	for {
		d.mu.Lock()
		if d.stopped {
			d.mu.Unlock()
			return
		}
		dl := d.next()
		var ctx context.Context
		if dl != nil {
			ctx, d.cancel = context.WithCancel(context.Background())
			d.active = dl.ID
			dl.State = dlRunning
			dl.save()
		}
		d.mu.Unlock()
		if dl == nil {
			<-d.wake
			continue
		}
		d.changed()
		before := dl.Covered()
		err := d.fetch(ctx, dl)
		d.mu.Lock()
		d.active, d.cancel = "", nil
		stopped := d.stopped
		switch {
		case err == nil:
			dl.State, dl.Reason = dlDone, ""
			fails = 0
		case dl.State == dlPaused || stopped || d.list[dl.ID] != dl:
			// paused, closing or deleted: leave it
		case errors.Is(err, errNoSpace):
			dl.State, dl.Reason = dlPaused, err.Error()
		case errors.Is(err, errSignedOut):
			dl.State, dl.Reason = dlPaused, "Sign in again to go on."
		case isOffline(err) && dl.Covered() <= before+5:
			// the server can't be reached: wait for it, without giving up
			dl.State, dl.Reason = dlQueued, "Waiting for the server"
		default:
			if dl.Covered() > before+5 {
				fails = 0
			}
			fails++
			dl.State, dl.Reason = dlQueued, "Trying again: "+err.Error()
			if fails >= 6 {
				dl.State, dl.Reason = dlFailed, err.Error()
				fails = 0
			}
		}
		if d.list[dl.ID] == dl {
			dl.save()
		}
		wait := time.Duration(0)
		switch {
		case dl.State == dlQueued && dl.Reason == "Waiting for the server":
			wait = d.retry(10) // the longest wait; going online wakes it
		case dl.State == dlQueued && err != nil:
			wait = d.retry(fails)
		}
		d.mu.Unlock()
		d.changed()
		if err != nil {
			log.Printf("download %s: %v", dl.ID, err)
		}
		if wait > 0 {
			select {
			case <-time.After(wait):
			case <-d.wake:
			}
		}
	}
}

// fetch downloads what's missing of dl: the poster and subtitles first,
// then a transcode from where the parts end.
func (d *downloads) fetch(ctx context.Context, dl *Download) error {
	if d.poster != nil {
		if _, err := os.Stat(filepath.Join(dl.dir, "poster.png")); err != nil {
			d.poster(dl.Item, filepath.Join(dl.dir, "poster.png"))
		}
	}
	if !dl.SubsChecked {
		// queued from a list, without its tracks: read them now
		full, err := d.c.Item(dl.ID)
		if err != nil {
			return err
		}
		d.mu.Lock()
		if len(dl.Subs) == 0 {
			dl.Subs = keepSubs(full)
		}
		dl.SubsChecked = true
		d.mu.Unlock()
	}
	d.mu.Lock()
	subs := append([]dlSub(nil), dl.Subs...)
	d.mu.Unlock()
	for _, s := range subs {
		path := filepath.Join(dl.dir, s.File)
		if _, err := os.Stat(path); err == nil {
			continue
		}
		msid := dl.Item.ID
		if len(dl.Item.MediaSources) > 0 {
			msid = dl.Item.MediaSources[0].ID
		}
		b, err := d.c.fetch(ctx, d.c.subtitleURL(dl.Item.ID, msid, s.Index), 4<<20)
		if err != nil {
			if isOffline(err) || errors.Is(err, errSignedOut) {
				return err
			}
			log.Printf("download %s: subtitle %d: %v", dl.ID, s.Index, err)
			continue
		}
		atomicfile.Write(path, b, 0644)
	}
	for {
		d.mu.Lock()
		n := len(dl.Parts)
		done := n > 0 && dl.Parts[n-1].Done
		start90 := int64(0)
		if n > 0 {
			start90 = dl.Parts[n-1].end90()
		}
		d.mu.Unlock()
		rt := dl.Item.Runtime()
		if done || (rt > 0 && float64(start90)/ptsHz >= rt-2) {
			return nil
		}
		if err := d.fetchPart(ctx, dl, start90); err != nil {
			return err
		}
	}
}

// fetchPart downloads one part, from start90 to the end of the video or
// until it is interrupted; an interrupted part is cut at its last
// keyframe, so the next one starts exactly there.
func (d *downloads) fetchPart(ctx context.Context, dl *Download, start90 int64) error {
	start := float64(start90) / ptsHz
	left := streamBytes(dl.Kbps, max(0, dl.Item.Runtime()-start))
	if free := d.free(dl.dir); free >= 0 && free < left+reserve/2 {
		return errNoSpace
	}
	it := dl.Item
	if len(it.MediaSources) == 0 { // the media source id and streams, for PlaybackInfo
		if full, err := d.c.Item(it.ID); err == nil {
			it.MediaSources = full.MediaSources
		} else if isOffline(err) || errors.Is(err, errSignedOut) {
			return err
		}
	}
	plan, err := d.c.Plan(it, start, -1, -1, dl.Kbps, true)
	if err != nil {
		return err
	}
	req, err := http.NewRequestWithContext(ctx, "GET", plan.URL, nil)
	if err != nil {
		return err
	}
	req.Header.Set("Authorization", d.c.AuthHeader())
	resp, err := d.c.streamClient().Do(req)
	if err != nil {
		return &netError{explainNet(err)}
	}
	defer resp.Body.Close()
	if resp.StatusCode == http.StatusUnauthorized {
		return errSignedOut
	}
	if resp.StatusCode != http.StatusOK {
		return &httpError{Status: resp.StatusCode}
	}
	name := fmt.Sprintf("part%d.ts", len(dl.Parts))
	path := filepath.Join(dl.dir, name)
	f, err := os.Create(path)
	if err != nil {
		return err
	}
	defer f.Close()
	sc := newTSScanner()
	part := dlPart{File: name, Start90: start90}
	d.mu.Lock()
	dl.Parts = append(dl.Parts, part)
	idx := len(dl.Parts) - 1
	d.mu.Unlock()
	// record writes the part's safe extent to meta.json: the bytes up to
	// its last keyframe, so a crash never leaves meta pointing past what's
	// on the card.
	record := func(done bool, size int64) {
		d.mu.Lock()
		defer d.mu.Unlock()
		p := &dl.Parts[idx]
		p.FirstPTS, p.Keys, p.Done = sc.FirstPTS, append([]keyPoint(nil), sc.Keys...), done
		switch {
		case done:
			p.Size, p.EndPTS = size, sc.LastPTS
		case sc.HasKey:
			p.Size, p.EndPTS = sc.LastKey.Off, sc.LastKey.PTS
		default:
			p.Size, p.EndPTS = 0, sc.FirstPTS
		}
		if d.list[dl.ID] == dl {
			dl.save()
		}
	}
	stall := time.AfterFunc(relayStall, func() { resp.Body.Close() })
	defer stall.Stop()
	buf := make([]byte, 64<<10)
	var written int64
	lastSave, lastFree := time.Now(), int64(0)
	var ferr error
	for {
		stall.Reset(relayStall)
		n, rerr := resp.Body.Read(buf)
		if n > 0 {
			if _, werr := f.Write(buf[:n]); werr != nil {
				ferr = werr
				if errors.Is(werr, syscall.ENOSPC) {
					ferr = errNoSpace
				}
				break
			}
			sc.Write(buf[:n])
			written += int64(n)
		}
		if rerr != nil {
			if !errors.Is(rerr, io.EOF) {
				ferr = &netError{explainNet(rerr)}
				if ctx.Err() != nil {
					ferr = ctx.Err()
				}
			}
			break
		}
		if written-lastFree >= 8<<20 {
			lastFree = written
			if free := d.free(dl.dir); free >= 0 && free < reserve/4 {
				ferr = errNoSpace
				break
			}
		}
		if time.Since(lastSave) > 10*time.Second {
			lastSave = time.Now()
			f.Sync()
			record(false, written)
			d.changed()
		}
	}
	f.Sync()
	if ferr == nil {
		// The server ended the stream. It counts as the whole video when
		// it reaches the end (or the runtime isn't known).
		got := float64(start90+max(0, ptsDiff(sc.LastPTS, sc.FirstPTS))) / ptsHz
		if rt := dl.Item.Runtime(); sc.FirstPTS >= 0 && (rt <= 0 || got >= rt-15) {
			record(true, written)
			return nil
		}
		ferr = &netError{fmt.Errorf("the server stopped the transcode at %.0f s", got)}
	}
	record(false, written)
	d.mu.Lock()
	p := dl.Parts[idx]
	if p.Size == 0 {
		dl.Parts = dl.Parts[:idx] // nothing usable: drop the part
	}
	d.mu.Unlock()
	if p.Size == 0 {
		f.Close()
		os.Remove(path)
	} else {
		f.Truncate(p.Size)
	}
	d.mu.Lock()
	if d.list[dl.ID] == dl {
		dl.save()
	}
	d.mu.Unlock()
	return ferr
}

// localStream is a download played from the card: its parts one after
// another, each part's timestamps moved to follow on from the one before.
// It reads whole packets so the timestamps can be rewritten as they go.
type localStream struct {
	parts []localPart
	size  int64
	from  int64 // the offset playback starts at
}

type localPart struct {
	f     *os.File
	base  int64 // offset of the part in the stream
	size  int64
	delta int64 // ticks added to its timestamps
}

// openLocal opens dl for playing from about start seconds: it starts at
// the last keyframe before start, and reports that keyframe's time.
func openLocal(dl Download, start float64) (*localStream, float64, error) {
	if len(dl.Parts) == 0 {
		return nil, 0, errors.New("nothing of it is on the card yet")
	}
	ls := &localStream{}
	first := dl.Parts[0]
	at, from := 0.0, int64(0)
	for _, p := range dl.Parts {
		if p.Size <= 0 {
			continue
		}
		f, err := os.Open(filepath.Join(dl.dir, p.File))
		if err != nil {
			ls.Close()
			return nil, 0, fmt.Errorf("a part of the download is missing (%v). Delete it and download it again", err)
		}
		delta := (p.Start90 - first.Start90) + first.FirstPTS - p.FirstPTS
		ls.parts = append(ls.parts, localPart{f: f, base: ls.size, size: p.Size, delta: delta})
		for _, k := range p.Keys {
			t := float64(p.Start90+ptsDiff(k.PTS, p.FirstPTS)) / ptsHz
			if t <= start+0.5 && k.Off < p.Size {
				at, from = t, ls.size+k.Off
			}
		}
		ls.size += p.Size
	}
	if len(ls.parts) == 0 {
		return nil, 0, errors.New("nothing of it is on the card yet")
	}
	ls.from = from
	return ls, at, nil
}

// ReadAt reads the stream at off, rewriting timestamps packet by packet.
func (ls *localStream) ReadAt(p []byte, off int64) (int, error) {
	n := 0
	for n < len(p) {
		if off >= ls.size {
			return n, io.EOF
		}
		i := sort.Search(len(ls.parts), func(i int) bool { return ls.parts[i].base+ls.parts[i].size > off })
		lp := ls.parts[i]
		in := off - lp.base
		aligned := in - in%tsPacket
		want := min(int64(len(p)-n)+(in-aligned), lp.size-aligned)
		want = (want + tsPacket - 1) / tsPacket * tsPacket
		want = min(want, lp.size-aligned)
		buf := make([]byte, want)
		got, err := lp.f.ReadAt(buf, aligned)
		if got <= 0 {
			if err == nil {
				err = io.ErrUnexpectedEOF
			}
			return n, err
		}
		buf = buf[:got]
		shiftTS(buf, lp.delta)
		c := copy(p[n:], buf[in-aligned:])
		n += c
		off += int64(c)
		if c == 0 {
			return n, io.ErrUnexpectedEOF
		}
	}
	return n, nil
}

// Close closes the part files.
func (ls *localStream) Close() {
	for _, p := range ls.parts {
		p.f.Close()
	}
}
