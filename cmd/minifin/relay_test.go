package main

import (
	"bytes"
	"context"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"
	"time"
)

// upstream is a fake server for the relay: a file with Range support, a
// stream that never ends, one that trickles, one that stalls.
func upstream(t *testing.T) (*httptest.Server, []byte, *atomic.Int32) {
	data := bytes.Repeat([]byte("0123456789abcdef"), 64<<10) // 1 MB
	var cancelled atomic.Int32
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Header.Get("Authorization") != "MediaBrowser Token=\"t\"" {
			http.Error(w, "no auth", http.StatusUnauthorized)
			return
		}
		switch r.URL.Path {
		case "/file.ts":
			w.Header().Set("Content-Type", "video/mp2t")
			http.ServeContent(w, r, "", time.Time{}, bytes.NewReader(data))
		case "/endless.ts":
			w.Header().Set("Content-Type", "video/mp2t")
			for {
				if _, err := w.Write(data[:32<<10]); err != nil {
					cancelled.Add(1)
					return
				}
				w.(http.Flusher).Flush()
				select {
				case <-r.Context().Done():
					cancelled.Add(1)
					return
				case <-time.After(5 * time.Millisecond):
				}
			}
		case "/slow.ts":
			for i := 0; i < 5; i++ {
				w.Write([]byte(fmt.Sprintf("chunk%d;", i)))
				w.(http.Flusher).Flush()
				time.Sleep(60 * time.Millisecond)
			}
		case "/stall.ts":
			w.Write([]byte("start;"))
			w.(http.Flusher).Flush()
			select {
			case <-r.Context().Done():
				cancelled.Add(1)
			case <-time.After(10 * time.Second):
			}
		case "/late.ts": // no answer at all for a long time
			select {
			case <-r.Context().Done():
				cancelled.Add(1)
			case <-time.After(10 * time.Second):
			}
		}
	}))
	t.Cleanup(srv.Close)
	return srv, data, &cancelled
}

func testRelay(t *testing.T) *relay {
	r, err := startRelay(http.DefaultClient, func() string { return `MediaBrowser Token="t"` })
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(r.close)
	return r
}

// Range requests go through to the server and come back as 206.
func TestRelayRange(t *testing.T) {
	srv, data, _ := upstream(t)
	r := testRelay(t)
	local := r.Stream(srv.URL+"/file.ts", nil)
	req, _ := http.NewRequest("GET", local, nil)
	req.Header.Set("Range", "bytes=1000-1999")
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	b, _ := io.ReadAll(resp.Body)
	resp.Body.Close()
	if resp.StatusCode != 206 || !bytes.Equal(b, data[1000:2000]) || resp.Header.Get("Content-Range") != fmt.Sprintf("bytes 1000-1999/%d", len(data)) {
		t.Fatalf("HTTP %d, %d bytes, %q", resp.StatusCode, len(b), resp.Header.Get("Content-Range"))
	}
	if resp.Header.Get("Accept-Ranges") != "bytes" {
		t.Error("Accept-Ranges not passed on")
	}
	// the whole file
	resp, _ = http.Get(local)
	b, _ = io.ReadAll(resp.Body)
	resp.Body.Close()
	if resp.StatusCode != 200 || !bytes.Equal(b, data) {
		t.Fatalf("whole file: HTTP %d, %d bytes", resp.StatusCode, len(b))
	}
}

// The player going away cancels the request to the server.
func TestRelayCancel(t *testing.T) {
	srv, _, cancelled := upstream(t)
	r := testRelay(t)
	ctx, cancel := context.WithCancel(context.Background())
	req, _ := http.NewRequestWithContext(ctx, "GET", r.Stream(srv.URL+"/endless.ts", nil), nil)
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	io.ReadFull(resp.Body, make([]byte, 100<<10))
	cancel()
	resp.Body.Close()
	deadline := time.Now().Add(3 * time.Second)
	for cancelled.Load() == 0 {
		if time.Now().After(deadline) {
			t.Fatal("the server's request wasn't cancelled")
		}
		time.Sleep(10 * time.Millisecond)
	}
	// a new video replaces the old URL
	old := r.Stream(srv.URL+"/file.ts", nil)
	r.Stream(srv.URL+"/slow.ts", nil)
	if resp, _ := http.Get(old); resp.StatusCode != 404 {
		t.Fatalf("an old URL still plays: %d", resp.StatusCode)
	}
}

// A slow server's bytes reach the player as they come, not at the end.
func TestRelaySlowServer(t *testing.T) {
	srv, _, _ := upstream(t)
	r := testRelay(t)
	start := time.Now()
	resp, err := http.Get(r.Stream(srv.URL+"/slow.ts", nil))
	if err != nil {
		t.Fatal(err)
	}
	buf := make([]byte, 7)
	io.ReadFull(resp.Body, buf)
	first := time.Since(start)
	rest, _ := io.ReadAll(resp.Body)
	resp.Body.Close()
	if string(buf) != "chunk0;" || !strings.HasSuffix(string(rest), "chunk4;") {
		t.Fatalf("got %q %q", buf, rest)
	}
	if first > 200*time.Millisecond {
		t.Fatalf("the first bytes took %v: the relay buffered the stream", first)
	}
}

// A server that stops sending is given up after the stall time; one that
// never answers too.
func TestRelayStall(t *testing.T) {
	srv, _, cancelled := upstream(t)
	r := testRelay(t)
	r.stall = 300 * time.Millisecond
	for _, path := range []string{"/stall.ts", "/late.ts"} {
		start := time.Now()
		resp, err := http.Get(r.Stream(srv.URL+path, nil))
		if err == nil {
			io.ReadAll(resp.Body)
			resp.Body.Close()
		}
		if d := time.Since(start); d > 3*time.Second {
			t.Fatalf("%s: the relay waited %v", path, d)
		}
	}
	time.Sleep(100 * time.Millisecond)
	if cancelled.Load() < 2 {
		t.Fatalf("stalled requests to the server weren't cancelled (%d)", cancelled.Load())
	}
}
