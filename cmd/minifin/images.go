package main

import (
	"bytes"
	"container/list"
	"context"
	"fmt"
	"image"
	_ "image/gif"
	_ "image/jpeg"
	"image/png" // the server's PNGs, and download posters
	"os"
	"sync"
	"time"

	"github.com/zakijss/minideck/cache"
	"github.com/zakijss/minideck/ui"
)

// imgKey is one picture at one size on screen.
type imgKey struct {
	id, kind, tag string // the item, Primary/Thumb/Backdrop, the server's tag
	w, h          int
}

// images fetches, decodes and keeps pictures for the screens. Drawing
// never waits: get returns what's in memory (nil if not yet) and asks for
// the rest, which workers fetch (from the card's cache or the server),
// decode and scale to the exact size drawn, off the screen's goroutine.
// The newest requests go first, so scrolling fast through a big library
// fetches what is on screen now, not what scrolled past.
type images struct {
	fetch func(ctx context.Context, k imgKey) ([]byte, error)
	disk  *cache.Disk
	poke  func()
	max   int // bytes of decoded pictures kept in memory

	mu      sync.Mutex
	cond    *sync.Cond
	mem     map[imgKey]*list.Element
	lru     *list.List // front: most recently drawn
	bytes   int
	queue   []imgKey // newest last
	queued  map[imgKey]bool
	failed  map[imgKey]time.Time
	files   map[string]image.Image // pictures from the card (downloads), by path
	stopped bool
	working int // requests being fetched now
	workers sync.WaitGroup
}

type imgEntry struct {
	k   imgKey
	img image.Image
}

// max is 6 MB of decoded pictures: about 130 posters, ten screens of the
// library grid. The rest are on the card, quick to read again.

// maxQueue: requests beyond this many waiting are dropped, oldest first
// (they scrolled off the screen long ago; they are asked for again if they
// come back).
const maxQueue = 48

func newImages(fetch func(ctx context.Context, k imgKey) ([]byte, error), disk *cache.Disk, poke func()) *images {
	im := &images{fetch: fetch, disk: disk, poke: poke, max: 6 << 20,
		mem: map[imgKey]*list.Element{}, lru: list.New(), queued: map[imgKey]bool{},
		failed: map[imgKey]time.Time{}, files: map[string]image.Image{}}
	im.cond = sync.NewCond(&im.mu)
	for range 3 {
		im.workers.Add(1)
		go im.work()
	}
	return im
}

// get returns the picture for k, or nil while it loads (or when there is
// none).
func (im *images) get(k imgKey) image.Image {
	if k.id == "" || k.w <= 0 || k.h <= 0 {
		return nil
	}
	im.mu.Lock()
	defer im.mu.Unlock()
	if e, ok := im.mem[k]; ok {
		im.lru.MoveToFront(e)
		return e.Value.(*imgEntry).img
	}
	if t, ok := im.failed[k]; ok && time.Since(t) < 30*time.Second {
		return nil
	}
	if !im.queued[k] {
		im.queued[k] = true
		im.queue = append(im.queue, k)
		if len(im.queue) > maxQueue {
			drop := len(im.queue) - maxQueue
			for _, old := range im.queue[:drop] {
				delete(im.queued, old)
			}
			im.queue = append(im.queue[:0], im.queue[drop:]...)
		}
		im.cond.Signal()
	}
	return nil
}

// idle reports whether nothing is waiting or being fetched.
func (im *images) idle() bool {
	im.mu.Lock()
	defer im.mu.Unlock()
	return len(im.queue) == 0 && im.working == 0
}

// file returns a picture kept on the card (a download's poster), decoded
// once.
func (im *images) file(path string) image.Image {
	im.mu.Lock()
	img, ok := im.files[path]
	im.mu.Unlock()
	if ok {
		return img
	}
	b, err := os.ReadFile(path)
	if err == nil {
		img, _ = png.Decode(bytes.NewReader(b))
	}
	im.mu.Lock()
	if len(im.files) > 64 {
		clear(im.files)
	}
	im.files[path] = img
	im.mu.Unlock()
	return img
}

// stop ends the workers, waiting for the ones busy (so nothing writes to
// the cache after MiniFin closes).
func (im *images) stop() {
	im.mu.Lock()
	im.stopped = true
	im.cond.Broadcast()
	im.mu.Unlock()
	im.workers.Wait()
}

func (im *images) work() {
	defer im.workers.Done()
	for {
		im.mu.Lock()
		for len(im.queue) == 0 && !im.stopped {
			im.cond.Wait()
		}
		if im.stopped {
			im.mu.Unlock()
			return
		}
		k := im.queue[len(im.queue)-1]
		im.queue = im.queue[:len(im.queue)-1]
		im.working++
		im.mu.Unlock()

		img, err := im.load(k)
		im.mu.Lock()
		im.working--
		delete(im.queued, k)
		if err != nil || img == nil {
			im.failed[k] = time.Now()
			if len(im.failed) > 500 {
				clear(im.failed)
			}
		} else {
			im.put(k, img)
		}
		im.mu.Unlock()
		if err == nil {
			im.poke()
		}
	}
}

// put keeps img, dropping the least recently drawn beyond max. The caller
// holds mu.
func (im *images) put(k imgKey, img image.Image) {
	if e, ok := im.mem[k]; ok {
		im.lru.MoveToFront(e)
		return
	}
	im.mem[k] = im.lru.PushFront(&imgEntry{k, img})
	im.bytes += k.w * k.h * 4
	for im.bytes > im.max && im.lru.Len() > 1 {
		e := im.lru.Back()
		old := e.Value.(*imgEntry)
		im.lru.Remove(e)
		delete(im.mem, old.k)
		im.bytes -= old.k.w * old.k.h * 4
	}
}

// cacheName is k's name in the card's cache.
func (k imgKey) cacheName() string {
	return fmt.Sprintf("img|%s|%s|%s|%dx%d", k.id, k.kind, k.tag, k.w, k.h)
}

// load reads k from the card's cache, or fetches it and keeps the scaled
// copy there, as raw pixels: no decoding to read it again, and nothing to
// compress when it's written (a PNG encoder needs about a megabyte of
// tables each time, which the device's memory feels).
func (im *images) load(k imgKey) (image.Image, error) {
	if im.disk != nil {
		if b, ok := im.disk.Get(k.cacheName()); ok {
			if img := rawImage(b, k.w, k.h); img != nil {
				return img, nil
			}
		}
	}
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	b, err := im.fetch(ctx, k)
	if err != nil {
		return nil, err
	}
	img, err := decodeImage(b, k.w, k.h)
	if err != nil {
		return nil, err
	}
	if im.disk != nil {
		im.disk.Put(k.cacheName(), rawBytes(img))
	}
	return img, nil
}

// rawMagic starts a picture kept as raw pixels.
const rawMagic = "MFRGBA1\n"

// rawBytes is img's pixels with a header.
func rawBytes(img *image.RGBA) []byte {
	w, h := img.Rect.Dx(), img.Rect.Dy()
	out := make([]byte, 0, len(rawMagic)+4+w*h*4)
	out = append(out, rawMagic...)
	out = append(out, byte(w>>8), byte(w), byte(h>>8), byte(h))
	for y := 0; y < h; y++ {
		out = append(out, img.Pix[y*img.Stride:y*img.Stride+w*4]...)
	}
	return out
}

// rawImage reads rawBytes back, checking it is w x h; nil if it isn't.
func rawImage(b []byte, w, h int) *image.RGBA {
	n := len(rawMagic)
	if len(b) != n+4+w*h*4 || string(b[:n]) != rawMagic ||
		int(b[n])<<8|int(b[n+1]) != w || int(b[n+2])<<8|int(b[n+3]) != h {
		return nil
	}
	img := image.NewRGBA(image.Rect(0, 0, w, h))
	copy(img.Pix, b[n+4:])
	return img
}

// maxPixels refuses pictures that would take too much memory to decode
// (a server that ignores the size asked for sends the original).
const maxPixels = 2500 * 2500

// decodeMu: one decode at a time, so three big pictures arriving together
// don't need three times the memory.
var decodeMu sync.Mutex

// decodeImage decodes b and crops and scales it to w x h.
func decodeImage(b []byte, w, h int) (*image.RGBA, error) {
	cfg, _, err := image.DecodeConfig(bytes.NewReader(b))
	if err != nil {
		return nil, err
	}
	if cfg.Width <= 0 || cfg.Height <= 0 || int64(cfg.Width)*int64(cfg.Height) > maxPixels { // int64: int is 32 bits on the device
		return nil, fmt.Errorf("picture too large (%dx%d)", cfg.Width, cfg.Height)
	}
	decodeMu.Lock()
	defer decodeMu.Unlock()
	img, _, err := image.Decode(bytes.NewReader(b))
	if err != nil {
		return nil, err
	}
	return ui.ScaleCrop(img, w, h), nil
}

// Picture sizes on screen.
const (
	posterW, posterH = 88, 132  // library grid
	cardW, cardH     = 136, 76  // home rails (16:9)
	bigW, bigH       = 160, 240 // item page poster
	stillW, stillH   = 200, 112 // item page, episodes (16:9)
)

// posterKey is an item's poster (its own, or its show's for an episode).
func posterKey(it Item, w, h int) imgKey {
	if tag := it.ImageTags["Primary"]; tag != "" && it.Type != "Episode" {
		return imgKey{it.ID, "Primary", tag, w, h}
	}
	if it.SeriesID != "" && it.SeriesPrimaryImageTag != "" {
		return imgKey{it.SeriesID, "Primary", it.SeriesPrimaryImageTag, w, h}
	}
	if tag := it.ImageTags["Primary"]; tag != "" {
		return imgKey{it.ID, "Primary", tag, w, h}
	}
	return imgKey{}
}

// wideKey is a 16:9 picture of an item: an episode's still, a movie's
// thumb or backdrop, or its show's.
func wideKey(it Item, w, h int) imgKey {
	switch {
	case it.Type == "Episode" && it.ImageTags["Primary"] != "":
		return imgKey{it.ID, "Primary", it.ImageTags["Primary"], w, h}
	case it.ImageTags["Thumb"] != "":
		return imgKey{it.ID, "Thumb", it.ImageTags["Thumb"], w, h}
	case len(it.BackdropImageTags) > 0:
		return imgKey{it.ID, "Backdrop", it.BackdropImageTags[0], w, h}
	case it.ParentThumbItemID != "" && it.ParentThumbImageTag != "":
		return imgKey{it.ParentThumbItemID, "Thumb", it.ParentThumbImageTag, w, h}
	case it.ParentBackdropItemID != "" && len(it.ParentBackdropImageTags) > 0:
		return imgKey{it.ParentBackdropItemID, "Backdrop", it.ParentBackdropImageTags[0], w, h}
	case it.ImageTags["Primary"] != "":
		return imgKey{it.ID, "Primary", it.ImageTags["Primary"], w, h}
	}
	return imgKey{}
}
