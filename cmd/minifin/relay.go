package main

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"errors"
	"io"
	"log"
	"math"
	"net"
	"net/http"
	"strconv"
	"strings"
	"sync"
	"time"
)

// relay serves video to the player on 127.0.0.1, over plain HTTP. For a
// stream it fetches the server's transcode itself (HTTPS with the bundled
// CA roots, the tailnet proxy, the Authorization header), so the player
// never needs any of those; for a download it reads the parts on the card.
// Each video gets a random path that is good until the next one, so the
// player's URL has no server address or credentials in it.
type relay struct {
	addr   string
	ln     net.Listener
	client *http.Client
	auth   func() string // the Authorization header for the server
	stall  time.Duration // how long the server may send nothing

	mu      sync.Mutex
	streams map[string]*relayStream // path id -> a stream from the server
	locals  map[string]*relayLocal  // path id -> a download
}

// relayStream is a video streamed from the server. With ?t= the player
// asks for it from another point: seek gives the server's URL for a new
// transcode from there (and stops the old one).
type relayStream struct {
	url  string
	seek func(t float64) (string, error)

	mu    sync.Mutex // one seek at a time
	at    float64
	atURL string
}

// urlAt is the server's URL for the stream from t seconds. Asking for the
// same t again (the player reopening it) reuses the transcode.
func (st *relayStream) urlAt(t float64) (string, error) {
	st.mu.Lock()
	defer st.mu.Unlock()
	if st.atURL != "" && math.Abs(st.at-t) < 0.001 {
		return st.atURL, nil
	}
	u, err := st.seek(t)
	if err != nil {
		return "", err
	}
	st.at, st.atURL = t, u
	return u, nil
}

// relayLocal is a download played from the card; reopen opens it again
// from the keyframe at or before t (?t=).
type relayLocal struct {
	ls     *localStream
	reopen func(t float64) (*localStream, float64, error)
}

// relayStall: after this long without a byte from the server, the request
// is given up and the player sees the stream end.
const relayStall = 30 * time.Second

// startHeader tells the player where in the video a stream asked for with
// ?t= really begins: t for a transcode, the keyframe at or before t for a
// download.
const startHeader = "X-MiniFin-Start"

func startRelay(client *http.Client, auth func() string) (*relay, error) {
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		return nil, err
	}
	r := &relay{addr: ln.Addr().String(), ln: ln, client: client, auth: auth, stall: relayStall,
		streams: map[string]*relayStream{}, locals: map[string]*relayLocal{}}
	go http.Serve(ln, http.HandlerFunc(r.serve))
	return r, nil
}

func (r *relay) close() { r.ln.Close() }

func randomID() string {
	b := make([]byte, 8)
	rand.Read(b)
	return hex.EncodeToString(b)
}

// Stream makes upstream (a server URL without credentials) playable at the
// returned local URL, replacing the previous video. seek, if set, makes
// ?t= work: it returns the URL of a new transcode from t.
func (r *relay) Stream(upstream string, seek func(t float64) (string, error)) string {
	id := randomID()
	r.mu.Lock()
	clear(r.streams)
	r.streams[id] = &relayStream{url: upstream, seek: seek}
	r.mu.Unlock()
	return "http://" + r.addr + "/v/" + id + ".ts"
}

// Local makes a download playable at the returned URL; reopen, if set,
// makes ?t= work.
func (r *relay) Local(ls *localStream, reopen func(t float64) (*localStream, float64, error)) string {
	id := randomID()
	r.mu.Lock()
	for k, old := range r.locals {
		old.ls.Close()
		delete(r.locals, k)
	}
	r.locals[id] = &relayLocal{ls: ls, reopen: reopen}
	r.mu.Unlock()
	return "http://" + r.addr + "/d/" + id + ".ts"
}

// Forget drops the current video: its URL stops working.
func (r *relay) Forget() {
	r.mu.Lock()
	clear(r.streams)
	for k, old := range r.locals {
		old.ls.Close()
		delete(r.locals, k)
	}
	r.mu.Unlock()
}

// seekTime reads ?t=: seconds into the video, decimals allowed.
func seekTime(req *http.Request) (float64, bool, error) {
	v := req.URL.Query().Get("t")
	if v == "" {
		return 0, false, nil
	}
	t, err := strconv.ParseFloat(v, 64)
	if err != nil || t < 0 || math.IsNaN(t) || math.IsInf(t, 0) {
		return 0, true, errors.New("bad t")
	}
	return t, true, nil
}

func (r *relay) serve(w http.ResponseWriter, req *http.Request) {
	path := req.URL.Path
	id := strings.TrimSuffix(path[min(len(path), 3):], ".ts")
	r.mu.Lock()
	st := r.streams[id]
	lo := r.locals[id]
	r.mu.Unlock()
	t, seeking, terr := seekTime(req)
	if terr != nil {
		http.Error(w, "t must be seconds", http.StatusBadRequest)
		return
	}
	switch {
	case strings.HasPrefix(path, "/v/") && st != nil:
		up := st.url
		if seeking {
			if st.seek == nil {
				http.Error(w, "this stream can't seek", http.StatusBadRequest)
				return
			}
			u, err := st.urlAt(t)
			if err != nil {
				log.Printf("relay: seek to %.1f s: %v", t, err)
				http.Error(w, "the server couldn't start there", http.StatusBadGateway)
				return
			}
			up = u
			w.Header().Set(startHeader, strconv.FormatFloat(t, 'f', 3, 64))
			log.Printf("relay: seek to %.1f s", t)
		}
		r.proxy(w, req, up)
	case strings.HasPrefix(path, "/d/") && lo != nil:
		ls := lo.ls
		if seeking {
			if lo.reopen == nil {
				http.Error(w, "this download can't seek", http.StatusBadRequest)
				return
			}
			nls, at, err := lo.reopen(t)
			if err != nil {
				http.Error(w, err.Error(), http.StatusInternalServerError)
				return
			}
			defer nls.Close()
			ls = nls
			w.Header().Set(startHeader, strconv.FormatFloat(at, 'f', 3, 64))
		}
		w.Header().Set("Content-Type", "video/mp2t")
		http.ServeContent(w, req, "", time.Time{}, io.NewSectionReader(ls, ls.from, ls.size-ls.from))
	default:
		http.NotFound(w, req)
	}
}

// proxy copies the server's reply for upstream to the player, passing a
// Range request through. It stops when the player goes away (the request
// to the server is cancelled with it) or the server stalls.
func (r *relay) proxy(w http.ResponseWriter, req *http.Request, upstream string) {
	ctx, cancel := context.WithCancel(req.Context())
	defer cancel()
	out, err := http.NewRequestWithContext(ctx, req.Method, upstream, nil)
	if err != nil {
		http.Error(w, "bad stream address", http.StatusInternalServerError)
		return
	}
	out.Header.Set("Authorization", r.auth())
	if rg := req.Header.Get("Range"); rg != "" {
		out.Header.Set("Range", rg)
	}
	// The stall timer covers the wait for the server's first answer too
	// (a transcode can take a few seconds to start).
	stall := time.AfterFunc(r.stall, cancel)
	defer stall.Stop()
	resp, err := r.client.Do(out)
	if err != nil {
		log.Printf("relay: the server didn't answer: %v", explainNet(err))
		http.Error(w, "the server didn't answer", http.StatusBadGateway)
		return
	}
	defer resp.Body.Close()
	for _, h := range []string{"Content-Type", "Content-Length", "Content-Range", "Accept-Ranges"} {
		if v := resp.Header.Get(h); v != "" {
			w.Header().Set(h, v)
		}
	}
	w.WriteHeader(resp.StatusCode)
	if resp.StatusCode >= 300 {
		log.Printf("relay: the server said HTTP %d for the stream", resp.StatusCode)
	}
	fl, _ := w.(http.Flusher)
	buf := make([]byte, 64<<10)
	var n int64
	for {
		stall.Reset(r.stall)
		got, rerr := resp.Body.Read(buf)
		if got > 0 {
			if _, werr := w.Write(buf[:got]); werr != nil {
				return // the player went away
			}
			n += int64(got)
			if fl != nil {
				fl.Flush()
			}
		}
		if rerr != nil {
			if !errors.Is(rerr, io.EOF) && req.Context().Err() == nil {
				log.Printf("relay: the stream stopped after %d bytes: %v", n, explainNet(rerr))
			}
			return
		}
	}
}
