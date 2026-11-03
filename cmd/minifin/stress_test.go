package main

import (
	"context"
	"fmt"
	"math/rand"
	"os"
	"runtime"
	"runtime/pprof"
	"sort"
	"sync/atomic"
	"testing"
	"time"

	"github.com/zakijss/minideck/ui"
)

// timings collects durations and sums them up.
type timings []time.Duration

func (t timings) String() string {
	if len(t) == 0 {
		return "none"
	}
	s := append(timings(nil), t...)
	sort.Slice(s, func(i, j int) bool { return s[i] < s[j] })
	var sum time.Duration
	for _, d := range s {
		sum += d
	}
	return fmt.Sprintf("n=%d mean=%v p50=%v p95=%v max=%v", len(s), sum/time.Duration(len(s)),
		s[len(s)/2], s[len(s)*95/100], s[len(s)-1])
}

func heapMB() float64 {
	runtime.GC()
	var m runtime.MemStats
	runtime.ReadMemStats(&m)
	return float64(m.HeapInuse) / (1 << 20)
}

// A 20,000-item library: opening it, paging through a third of it a
// screen at a time, jumping through every letter and to random places.
// Each press must be handled and drawn without waiting for the network,
// and memory must stay flat however far it goes.
func TestStressLibrary20k(t *testing.T) {
	if testing.Short() {
		t.Skip("stress test")
	}
	f := newFakeJellyfin(t)
	f.setLibrary(bigLibrary(20000))
	f.quiet = true
	a, _ := testApp(t, f)
	before := heapMB()
	lib := newLibrary(a, Item{ID: "lib-movies", Name: "Movies", CollectionType: "movies"})
	a.push(lib)
	start := time.Now()
	a.pump(t, func() bool { return lib.total == 20000 })
	firstPage := time.Since(start)

	var press, wait timings
	step := func(b ui.Button) {
		t0 := time.Now()
		a.press(ui.Event{Button: b, Pressed: true})
		a.top().draw(a) // what the screen does at once, network or not
		press = append(press, time.Since(t0))
		t1 := time.Now()
		a.pump(t, func() bool {
			_, ok := lib.items[lib.sel]
			return ok && !lib.jumping
		})
		wait = append(wait, time.Since(t1))
	}
	for i := 0; i < 550; i++ { // 550 screens of 12: 6,600 items
		step(ui.R1)
	}
	far := lib.sel
	for i := 0; i < 27; i++ {
		step(ui.R2)
	}
	r := rand.New(rand.NewSource(1))
	for i := 0; i < 60; i++ {
		lib.sel = r.Intn(lib.total)
		step(ui.Down)
	}
	for i := 0; i < 200 && !a.img.idle(); i++ {
		time.Sleep(10 * time.Millisecond)
	}
	after := heapMB()
	if p := os.Getenv("MINIFIN_HEAPPROF"); p != "" {
		fh, _ := os.Create(p)
		pprof.WriteHeapProfile(fh)
		fh.Close()
	}
	t.Logf("20,000 items: first page %v; %d presses handled+drawn: %v", firstPage, len(press), press)
	t.Logf("waiting for pages (fake server on loopback): %v", wait)
	t.Logf("heap in use %.1f MB before, %.1f MB after; %d items kept; reached item %d, then every letter",
		before, after, len(lib.items), far)
	if len(lib.items) > libKeep+libPage*(libAhead+2) {
		t.Fatalf("kept %d items", len(lib.items))
	}
	if far < 6000 {
		t.Fatalf("only got to %d", far)
	}
	// decoded posters (6 MB cap), the interface's scaled copies (96 at
	// most, 4.5 MB) and buffers of requests still in flight
	if after-before > 20 {
		t.Fatalf("heap grew %.1f MB", after-before)
	}
}

// Image cache churn: thousands of distinct posters asked for in bursts
// (fast scrolling) keep memory at the cap, the queue short, and the newest
// requests first.
func TestStressImageChurn(t *testing.T) {
	if testing.Short() {
		t.Skip("stress test")
	}
	var fetched atomic.Int32
	im := newImages(func(ctx context.Context, k imgKey) ([]byte, error) {
		fetched.Add(1)
		return testPNG(k.id, k.w, k.h), nil
	}, nil, func() {})
	defer im.stop()
	im.max = 4 << 20
	before := heapMB()
	start := time.Now()
	var gets timings
	for screen := 0; screen < 400; screen++ { // 400 screens of 12 posters
		for i := 0; i < 12; i++ {
			k := imgKey{id: fmt.Sprintf("it%d", screen*12+i), kind: "Primary", tag: "t", w: posterW, h: posterH}
			t0 := time.Now()
			im.get(k)
			gets = append(gets, time.Since(t0))
		}
		im.mu.Lock()
		q := len(im.queue)
		im.mu.Unlock()
		if q > maxQueue {
			t.Fatalf("queue %d", q)
		}
	}
	// the last screen arrives first
	deadline := time.Now().Add(10 * time.Second)
	for !im.idle() && time.Now().Before(deadline) {
		time.Sleep(5 * time.Millisecond)
	}
	last := 0
	for i := 0; i < 12; i++ {
		if im.get(imgKey{id: fmt.Sprintf("it%d", 399*12+i), kind: "Primary", tag: "t", w: posterW, h: posterH}) != nil {
			last++
		}
	}
	im.mu.Lock()
	bytes, n := im.bytes, len(im.mem)
	im.mu.Unlock()
	t.Logf("4,800 posters asked for in %v: get() %v; %d fetched (the rest dropped as they scrolled away); %d in memory, %d KB (cap %d KB); heap %.1f -> %.1f MB",
		time.Since(start), gets, fetched.Load(), n, bytes>>10, im.max>>10, before, heapMB())
	if bytes > im.max {
		t.Fatalf("cache %d bytes over the cap", bytes)
	}
	if last != 12 {
		t.Fatalf("only %d of the last screen's posters loaded", last)
	}
	if fetched.Load() > 1200 {
		t.Fatalf("fetched %d: scrolled-away requests weren't dropped", fetched.Load())
	}
}

// Drawing the busiest screens with their pictures in, for the time a
// frame takes (on this computer; the device is several times slower).
func benchPage(b *testing.B, setup func(a *app, f *fakeJellyfin) page) {
	f := newFakeJellyfin(b)
	f.quiet = true
	a, _ := testApp(b, f)
	p := setup(a, f)
	a.stack = []page{p}
	a.pump(b, func() bool { return !p.busy() })
	for i := 0; i < 50 && !a.img.idle(); i++ {
		p.draw(a)
		time.Sleep(10 * time.Millisecond)
	}
	b.ResetTimer()
	for b.Loop() {
		p.draw(a)
		a.scr.Present()
	}
}

func BenchmarkDrawHome(b *testing.B) {
	benchPage(b, func(a *app, f *fakeJellyfin) page { return newHome(a) })
}

func BenchmarkDrawLibrary(b *testing.B) {
	benchPage(b, func(a *app, f *fakeJellyfin) page {
		f.setLibrary(bigLibrary(500))
		return newLibrary(a, Item{ID: "lib", Name: "Movies", CollectionType: "movies"})
	})
}

// BenchmarkLibraryScroll is a press of Right in the grid and the redraw.
func BenchmarkLibraryScroll(b *testing.B) {
	f := newFakeJellyfin(b)
	f.quiet = true
	f.setLibrary(bigLibrary(500))
	a, _ := testApp(b, f)
	lib := newLibrary(a, Item{ID: "lib", Name: "Movies", CollectionType: "movies"})
	a.stack = []page{lib}
	a.pump(b, func() bool { return lib.total > 0 })
	b.ResetTimer()
	for b.Loop() {
		lib.sel = (lib.sel + 1) % 120
		lib.draw(a)
		a.scr.Present()
	}
}
