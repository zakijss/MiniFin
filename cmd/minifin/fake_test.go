package main

import (
	"bytes"
	"encoding/json"
	"fmt"
	"image"
	"image/color"
	"image/png"
	"io"
	"math"
	"net/http"
	"net/http/httptest"
	"regexp"
	"sort"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"
)

// fakeJellyfin answers like a Jellyfin 12 server, with the JSON shapes of
// its API (as in Jellyfin's OpenAPI document): enough for MiniFin's calls,
// and it records what it was sent.
type fakeJellyfin struct {
	t     testing.TB
	srv   *httptest.Server
	token string

	mu        sync.Mutex
	items     []Item // a library, sorted by SortName
	byID      map[string]Item
	qcOn      bool
	qcSecret  string
	approved  bool
	polls     int
	requests  []string // method and URL of each request
	auths     []string // the Authorization headers
	reports   []report
	userData  map[string]map[string]any
	playInfo  []map[string]any
	streamCut int64 // cut the first stream after this many bytes (0: never)
	cuts      int
	streams   []url2
	runtime   float64 // the stream's length in seconds
	slowImage time.Duration
	quiet     bool     // don't keep the requests (stress tests)
	sessions  int      // play sessions handed out by PlaybackInfo
	encStops  []string // DELETE /Videos/ActiveEncodings: "playSessionId deviceId"
}

type report struct {
	Path string
	Body map[string]any
}

type url2 struct{ Query map[string]string }

// authRE is the header Jellyfin 12 needs, with a token.
var authRE = regexp.MustCompile(`^MediaBrowser Client="MiniFin", Device="Miyoo Mini Plus", DeviceId="[^"]+", Version="[^"]+"(, Token="([^"]*)")?$`)

func newFakeJellyfin(t testing.TB) *fakeJellyfin {
	f := &fakeJellyfin{t: t, token: "tok-SECRET-123", qcOn: true, byID: map[string]Item{},
		userData: map[string]map[string]any{}, runtime: 120}
	f.srv = httptest.NewServer(http.HandlerFunc(f.serve))
	t.Cleanup(f.srv.Close)
	f.setLibrary(sampleMovies())
	for _, it := range sampleShow() {
		f.byID[it.ID] = it
	}
	return f
}

func (f *fakeJellyfin) setLibrary(items []Item) {
	f.mu.Lock()
	defer f.mu.Unlock()
	sort.Slice(items, func(i, j int) bool { return items[i].SortName < items[j].SortName })
	f.items = items
	for _, it := range items {
		f.byID[it.ID] = it
	}
}

func sampleMovies() []Item {
	two := 2
	return []Item{
		{ID: "m1", Name: "The Night Harbor", SortName: "night harbor", Type: "Movie", ProductionYear: 2019, RunTimeTicks: 7920 * ticksPerSecond,
			OfficialRating: "PG-13", CommunityRating: 7.8, ImageTags: map[string]string{"Primary": "p1", "Thumb": "t1"},
			Overview: "A harbor pilot on her last night shift guides a crippled freighter through fog while the town sleeps, and finds the ship carries something it shouldn't.",
			UserData: &UserData{PlaybackPositionTicks: 2712 * ticksPerSecond, PlayedPercentage: 34.2},
			MediaSources: []MediaSource{{ID: "ms1", Container: "mkv", RunTimeTicks: 7920 * ticksPerSecond, DefaultAudioStreamIndex: &two,
				MediaStreams: []MediaStream{
					{Type: "Video", Index: 0, Codec: "hevc", Width: 1920, Height: 1080},
					{Type: "Audio", Index: 1, Codec: "eac3", Language: "eng", DisplayTitle: "English - EAC3 5.1", Channels: 6},
					{Type: "Audio", Index: 2, Codec: "aac", Language: "fre", DisplayTitle: "French - AAC Stereo", Channels: 2},
					{Type: "Subtitle", Index: 3, Codec: "subrip", Language: "eng", DisplayTitle: "English - SUBRIP", IsTextSubtitleStream: true},
					{Type: "Subtitle", Index: 4, Codec: "PGSSUB", Language: "eng", DisplayTitle: "English SDH - PGS"},
				}}}},
		{ID: "m2", Name: "Antenna", SortName: "antenna", Type: "Movie", ProductionYear: 2004, RunTimeTicks: 5400 * ticksPerSecond,
			ImageTags: map[string]string{"Primary": "p2"}, UserData: &UserData{Played: true}},
		{ID: "m3", Name: "Low Orbit", SortName: "low orbit", Type: "Movie", ProductionYear: 2011, RunTimeTicks: 6300 * ticksPerSecond,
			ImageTags: map[string]string{"Primary": "p3"}, UserData: &UserData{}},
		{ID: "m4", Name: "Paper Satellites", SortName: "paper satellites", Type: "Movie", ProductionYear: 2022, RunTimeTicks: 6900 * ticksPerSecond,
			ImageTags: map[string]string{"Primary": "p4"}, UserData: &UserData{}},
		{ID: "m5", Name: "1984 Signals", SortName: "1984 signals", Type: "Movie", ProductionYear: 1998, RunTimeTicks: 6000 * ticksPerSecond,
			UserData: &UserData{}},
	}
}

func sampleShow() []Item {
	return []Item{
		{ID: "sh1", Name: "Static Gardens", Type: "Series", ProductionYear: 2018, EndDate: "2021-05-01T00:00:00Z", ChildCount: 3,
			ImageTags: map[string]string{"Primary": "sp1"}, CommunityRating: 8.4,
			Overview: "Three radio engineers keep a coastal station on the air after the network forgets it exists.",
			UserData: &UserData{UnplayedItemCount: 9}},
		{ID: "se1", Name: "Season 1", Type: "Season", SeriesID: "sh1", SeriesName: "Static Gardens", IndexNumber: 1,
			ImageTags: map[string]string{"Primary": "ss1"}, UserData: &UserData{Played: true}},
		{ID: "se2", Name: "Season 2", Type: "Season", SeriesID: "sh1", SeriesName: "Static Gardens", IndexNumber: 2,
			UserData: &UserData{UnplayedItemCount: 4}},
		{ID: "ep1", Name: "Dead Air", Type: "Episode", SeriesID: "sh1", SeriesName: "Static Gardens", SeasonID: "se2",
			IndexNumber: 1, ParentIndexNumber: 2, RunTimeTicks: 2580 * ticksPerSecond, SeriesPrimaryImageTag: "sp1",
			ImageTags: map[string]string{"Primary": "e1"}, UserData: &UserData{Played: true}},
		{ID: "ep2", Name: "The Long Wave", Type: "Episode", SeriesID: "sh1", SeriesName: "Static Gardens", SeasonID: "se2",
			IndexNumber: 2, ParentIndexNumber: 2, RunTimeTicks: 2640 * ticksPerSecond, SeriesPrimaryImageTag: "sp1",
			ImageTags: map[string]string{"Primary": "e2"}, UserData: &UserData{PlaybackPositionTicks: 900 * ticksPerSecond, PlayedPercentage: 34}},
		{ID: "ep3", Name: "Interference Patterns on the Northern Relay", Type: "Episode", SeriesID: "sh1", SeriesName: "Static Gardens", SeasonID: "se2",
			IndexNumber: 3, ParentIndexNumber: 2, RunTimeTicks: 2700 * ticksPerSecond, SeriesPrimaryImageTag: "sp1",
			ImageTags: map[string]string{"Primary": "e3"}, UserData: &UserData{}},
	}
}

func (f *fakeJellyfin) URL() string { return f.srv.URL }

func writeJSON(w http.ResponseWriter, v any) {
	w.Header().Set("Content-Type", "application/json; charset=utf-8")
	json.NewEncoder(w).Encode(v)
}

func (f *fakeJellyfin) serve(w http.ResponseWriter, r *http.Request) {
	auth := r.Header.Get("Authorization")
	f.mu.Lock()
	if !f.quiet {
		f.requests = append(f.requests, r.Method+" "+r.URL.String())
		f.auths = append(f.auths, auth)
	}
	f.mu.Unlock()
	m := authRE.FindStringSubmatch(auth)
	if m == nil {
		http.Error(w, "Authorization header missing or malformed", http.StatusBadRequest)
		return
	}
	token := m[2]
	path := r.URL.Path
	if lp := strings.ToLower(path); strings.HasPrefix(lp, "/videos/") { // Jellyfin's routes ignore case
		path = "/Videos/" + path[len("/videos/"):]
	}
	q := r.URL.Query()
	var body map[string]any
	if r.Body != nil {
		b, _ := io.ReadAll(r.Body)
		if len(b) > 0 {
			json.Unmarshal(b, &body)
		}
	}
	// no credentials needed
	switch {
	case path == "/System/Info/Public":
		writeJSON(w, map[string]any{"LocalAddress": f.srv.URL, "ServerName": "Jellyfin on homeserver", "Version": "12.4.0",
			"ProductName": "Jellyfin Server", "OperatingSystem": "", "Id": "srv-0001", "StartupWizardCompleted": true})
		return
	case path == "/QuickConnect/Enabled":
		writeJSON(w, f.qcOn)
		return
	case path == "/QuickConnect/Initiate" && r.Method == "POST":
		if !f.qcOn {
			http.Error(w, "Quick connect is disabled", http.StatusUnauthorized)
			return
		}
		f.mu.Lock()
		f.qcSecret, f.approved, f.polls = "qc-secret-9", false, 0
		f.mu.Unlock()
		writeJSON(w, map[string]any{"Authenticated": false, "Secret": "qc-secret-9", "Code": "482915",
			"DeviceId": "x", "DeviceName": "Miyoo Mini Plus", "AppName": "MiniFin", "AppVersion": "dev", "DateAdded": "2026-11-02T10:00:00Z"})
		return
	case path == "/QuickConnect/Connect":
		f.mu.Lock()
		defer f.mu.Unlock()
		if q.Get("secret") != f.qcSecret || f.qcSecret == "" {
			http.Error(w, "not found", http.StatusNotFound)
			return
		}
		f.polls++
		writeJSON(w, map[string]any{"Authenticated": f.approved, "Secret": f.qcSecret, "Code": "482915"})
		return
	case path == "/Users/AuthenticateWithQuickConnect":
		f.mu.Lock()
		ok := body["Secret"] == f.qcSecret && f.approved
		f.mu.Unlock()
		if !ok {
			http.Error(w, "not approved", http.StatusBadRequest)
			return
		}
		writeJSON(w, f.authResult())
		return
	case path == "/Users/AuthenticateByName":
		if body["Username"] != "zaki" || body["Pw"] != "hunter2" {
			http.Error(w, "Error processing request.", http.StatusUnauthorized)
			return
		}
		writeJSON(w, f.authResult())
		return
	}
	if token != f.token {
		w.WriteHeader(http.StatusUnauthorized)
		return
	}
	switch {
	case path == "/UserViews":
		writeJSON(w, map[string]any{"Items": []Item{
			{ID: "lib-movies", Name: "Movies", Type: "CollectionFolder", CollectionType: "movies", ImageTags: map[string]string{"Primary": "lm"}},
			{ID: "lib-shows", Name: "Shows", Type: "CollectionFolder", CollectionType: "tvshows", ImageTags: map[string]string{"Primary": "ls"}},
			{ID: "lib-music", Name: "Music", Type: "CollectionFolder", CollectionType: "music"},
		}, "TotalRecordCount": 3, "StartIndex": 0})
	case path == "/UserItems/Resume":
		writeJSON(w, map[string]any{"Items": []Item{f.byID["m1"], f.byID["ep2"]}, "TotalRecordCount": 2, "StartIndex": 0})
	case path == "/Shows/NextUp":
		writeJSON(w, map[string]any{"Items": []Item{f.byID["ep3"]}, "TotalRecordCount": 1, "StartIndex": 0})
	case path == "/Items/Latest":
		writeJSON(w, []Item{f.byID["m4"], f.byID["sh1"], f.byID["m3"], f.byID["m2"], f.byID["m5"]})
	case path == "/Items":
		f.items1(w, q)
	case strings.HasPrefix(path, "/Items/") && strings.HasSuffix(path, "/PlaybackInfo"):
		f.playbackInfo(w, strings.Split(path, "/")[2], q, body)
	case strings.HasPrefix(path, "/Items/") && strings.Contains(path, "/Images/"):
		if f.slowImage > 0 {
			time.Sleep(f.slowImage)
		}
		w.Header().Set("Content-Type", "image/png")
		w.Write(testPNG(path, atoi(q.Get("fillWidth")), atoi(q.Get("fillHeight"))))
	case strings.HasPrefix(path, "/Items/"):
		it, ok := f.byID[strings.TrimPrefix(path, "/Items/")]
		if !ok {
			http.NotFound(w, r)
			return
		}
		writeJSON(w, it)
	case strings.HasPrefix(path, "/Shows/") && strings.HasSuffix(path, "/Seasons"):
		writeJSON(w, map[string]any{"Items": []Item{f.byID["se1"], f.byID["se2"]}, "TotalRecordCount": 2, "StartIndex": 0})
	case strings.HasPrefix(path, "/Shows/") && strings.HasSuffix(path, "/Episodes"):
		writeJSON(w, map[string]any{"Items": []Item{f.byID["ep1"], f.byID["ep2"], f.byID["ep3"]}, "TotalRecordCount": 3, "StartIndex": 0})
	case path == "/Videos/ActiveEncodings" && r.Method == "DELETE":
		f.mu.Lock()
		f.encStops = append(f.encStops, q.Get("playSessionId")+" "+q.Get("deviceId"))
		f.mu.Unlock()
		w.WriteHeader(http.StatusNoContent)
	case strings.HasPrefix(path, "/Videos/") && strings.HasSuffix(path, "/stream.ts"):
		f.stream(w, r, q)
	case strings.HasPrefix(path, "/Videos/") && strings.HasSuffix(path, "/Stream.srt"):
		w.Header().Set("Content-Type", "application/x-subrip")
		io.WriteString(w, "1\n00:00:01,000 --> 00:00:03,000\nThe fog came in at nine.\n\n")
	case strings.HasPrefix(path, "/Sessions/Playing") || path == "/Sessions/Logout":
		f.mu.Lock()
		f.reports = append(f.reports, report{path, body})
		f.mu.Unlock()
		w.WriteHeader(http.StatusNoContent)
	case strings.HasPrefix(path, "/UserItems/") && strings.HasSuffix(path, "/UserData"):
		f.mu.Lock()
		f.userData[strings.Split(path, "/")[2]] = body
		f.mu.Unlock()
		writeJSON(w, map[string]any{"PlaybackPositionTicks": body["PlaybackPositionTicks"]})
	default:
		http.NotFound(w, r)
	}
}

func atoi(s string) int { n, _ := strconv.Atoi(s); return n }

func (f *fakeJellyfin) authResult() map[string]any {
	return map[string]any{"User": map[string]any{"Name": "zaki", "Id": "user-1", "ServerId": "srv-0001"},
		"SessionInfo": map[string]any{"Id": "sess"}, "AccessToken": f.token, "ServerId": "srv-0001"}
}

// items1 is GET /Items: a page of the library, in SortName order, with
// NameLessThan and searchTerm.
func (f *fakeJellyfin) items1(w http.ResponseWriter, q map[string][]string) {
	get := func(k string) string {
		for kk, v := range q {
			if strings.EqualFold(kk, k) && len(v) > 0 {
				return v[0]
			}
		}
		return ""
	}
	f.mu.Lock()
	all := f.items
	f.mu.Unlock()
	var list []Item
	for _, it := range all {
		if lt := get("NameLessThan"); lt != "" && !(it.SortName < lt) {
			continue
		}
		if s := get("searchTerm"); s != "" && !strings.Contains(strings.ToLower(it.Name), strings.ToLower(s)) {
			continue
		}
		if get("Filters") == "IsUnplayed" && it.Played() {
			continue
		}
		list = append(list, it)
	}
	start, limit := atoi(get("StartIndex")), atoi(get("Limit"))
	page := []Item{}
	if start < len(list) {
		page = list[start:min(len(list), start+limit)]
	}
	writeJSON(w, map[string]any{"Items": page, "TotalRecordCount": len(list), "StartIndex": start})
}

// playbackInfo answers like Jellyfin with a transcode in the device's
// profile: the TranscodingUrl relative, with an api_key in it (older
// servers put one there) that MiniFin must drop.
func (f *fakeJellyfin) playbackInfo(w http.ResponseWriter, id string, q map[string][]string, body map[string]any) {
	f.mu.Lock()
	f.playInfo = append(f.playInfo, body)
	f.sessions++
	psid := fmt.Sprintf("ps-%d", f.sessions)
	f.mu.Unlock()
	it := f.byID[id]
	msid := "ms-" + id
	streams := []MediaStream{{Type: "Video", Index: 0, Codec: "h264"}, {Type: "Audio", Index: 1, Codec: "aac"}}
	if len(it.MediaSources) > 0 {
		msid, streams = it.MediaSources[0].ID, it.MediaSources[0].MediaStreams
	}
	sub, _ := body["SubtitleStreamIndex"].(float64)
	var out []MediaStream
	burn := ""
	for _, s := range streams {
		if s.Type == "Subtitle" {
			s.DeliveryMethod = "Encode"
			if s.IsTextSubtitleStream && profileExternal(body) {
				s.DeliveryMethod = "External"
				s.DeliveryURL = fmt.Sprintf("/Videos/%s/%s/Subtitles/%d/0/Stream.subrip?api_key=%s", id, msid, s.Index, f.token)
			}
			if int(sub) == s.Index && s.DeliveryMethod == "Encode" {
				burn = fmt.Sprintf("&SubtitleStreamIndex=%d&SubtitleMethod=Encode", s.Index)
			}
		}
		out = append(out, s)
	}
	start, _ := body["StartTimeTicks"].(float64)
	tu := fmt.Sprintf("/videos/%s/stream.ts?DeviceId=dev&MediaSourceId=%s&VideoCodec=h264&AudioCodec=aac&AudioStreamIndex=1&VideoBitrate=139616000&AudioBitrate=384000&MaxFramerate=60&api_key=%s&PlaySessionId=%s&TranscodingMaxAudioChannels=6&RequireAvc=false&Tag=abc&SegmentContainer=ts&MinSegments=1&BreakOnNonKeyFrames=False&h264-level=51&h264-profile=high&TranscodeReasons=VideoCodecNotSupported&StartTimeTicks=%d%s",
		id, msid, f.token, psid, int64(start), burn)
	writeJSON(w, map[string]any{"MediaSources": []map[string]any{{
		"Id": msid, "Container": "mkv", "RunTimeTicks": it.RunTimeTicks, "SupportsTranscoding": true,
		"TranscodingUrl": tu, "TranscodingSubProtocol": "http", "TranscodingContainer": "ts",
		"MediaStreams": out, "DefaultAudioStreamIndex": 1,
	}}, "PlaySessionId": psid})
}

// profileExternal reports whether the device profile sent asks for text
// subtitles on the side.
func profileExternal(body map[string]any) bool {
	dp, _ := body["DeviceProfile"].(map[string]any)
	subs, _ := dp["SubtitleProfiles"].([]any)
	for _, s := range subs {
		m, _ := s.(map[string]any)
		if m["Format"] == "srt" {
			return m["Method"] == "External"
		}
	}
	return false
}

// stream is /Videos/{id}/stream.ts: a transcode starting at
// StartTimeTicks, its timestamps from 1.4 s as FFmpeg's MPEG-TS muxer
// writes them. With streamCut set, the first request is cut short.
func (f *fakeJellyfin) stream(w http.ResponseWriter, r *http.Request, q map[string][]string) {
	params := map[string]string{}
	for k, v := range q {
		params[k] = v[0]
	}
	f.mu.Lock()
	f.streams = append(f.streams, url2{params})
	cut := f.streamCut
	if f.cuts > 0 {
		cut = 0
	}
	f.cuts++
	rt := f.runtime
	f.mu.Unlock()
	start := float64(atoi(params["StartTimeTicks"])) / ticksPerSecond
	data := makeTS(start, rt-start)
	w.Header().Set("Content-Type", "video/mp2t")
	if cut > 0 && cut < int64(len(data)) {
		w.Write(data[:cut])
		if hj, ok := w.(http.Hijacker); ok {
			conn, _, _ := hj.Hijack()
			conn.Close()
		}
		return
	}
	w.Write(data)
}

// testPNG is a small picture, its colour from the path.
func testPNG(path string, w, h int) []byte {
	if w <= 0 {
		w = 100
	}
	if h <= 0 {
		h = 150
	}
	img := image.NewRGBA(image.Rect(0, 0, w, h))
	sum := 0
	for _, c := range path {
		sum = sum*31 + int(c)
	}
	base := color.RGBA{uint8(60 + sum%150), uint8(40 + sum/7%150), uint8(90 + sum/13%140), 255}
	// a plain poster: a gradient to dark and a soft disc, placed by the path
	cx, cy, r := w*(25+sum%50)/100, h*(20+sum/3%30)/100, w*(18+sum/5%14)/100
	for y := 0; y < h; y++ {
		k := 1 - 0.8*float64(y)/float64(h)
		for x := 0; x < w; x++ {
			c := color.RGBA{uint8(float64(base.R) * k), uint8(float64(base.G) * k), uint8(float64(base.B) * k), 255}
			if dx, dy := x-cx, y-cy; dx*dx+dy*dy < r*r {
				c = color.RGBA{uint8(155 + int(c.R)/3), uint8(140 + int(c.G)/3), uint8(120 + int(c.B)/3), 255}
			}
			img.SetRGBA(x, y, c)
		}
	}
	var buf bytes.Buffer
	png.Encode(&buf, img)
	return buf.Bytes()
}

// ---- synthetic MPEG-TS ----

// makeTS makes dur seconds of a stream that begins sec seconds into the
// video: two video frames a second (a keyframe every 2 s, each after a
// PAT), and an audio frame a second. Timestamps start at 1.4 s.
func makeTS(sec, dur float64) []byte {
	var out []byte
	pkt := func(pid int, start bool, af []byte, payload []byte) {
		p := make([]byte, tsPacket)
		p[0] = tsSync
		p[1] = byte(pid >> 8 & 0x1f)
		if start {
			p[1] |= 0x40
		}
		p[2] = byte(pid)
		i := 4
		if af != nil {
			p[3] = 0x30
			p[4] = byte(len(af))
			copy(p[5:], af)
			i = 5 + len(af)
		} else {
			p[3] = 0x10
		}
		copy(p[i:], payload)
		for j := i + len(payload); j < tsPacket; j++ {
			p[j] = 0xff
		}
		out = append(out, p...)
	}
	pes := func(sid byte, pts int64) []byte {
		h := []byte{0, 0, 1, sid, 0, 0, 0x80, 0x80, 5, 0, 0, 0, 0, 0}
		putTS(h[9:14], 0x21, pts)
		return append(h, bytes.Repeat([]byte{0xab}, 20)...)
	}
	frames := int(dur * 2)
	for n := 0; n < frames; n++ {
		t := float64(n) / 2
		pts := int64(math.Round((1.4 + t) * ptsHz))
		key := n%4 == 0
		if key {
			pkt(0, true, nil, []byte{0, 0, 0xb0, 0x0d, 0, 1, 0xc1, 0, 0, 0, 1, 0xf0, 0, 0x2a, 0xb1, 0x04, 0xb2})
			pkt(0x1000, true, nil, []byte{0, 2, 0xb0, 0x17})
		}
		var af []byte
		if key {
			pcr := pts - 9000
			af = []byte{0x50, byte(pcr >> 25), byte(pcr >> 17), byte(pcr >> 9), byte(pcr >> 1), byte(pcr<<7) | 0x7e, 0}
		}
		pkt(0x100, true, af, pes(0xe0, pts))
		for k := 0; k < 3; k++ {
			pkt(0x100, false, nil, bytes.Repeat([]byte{byte(n)}, 100))
		}
		if n%2 == 0 {
			pkt(0x101, true, nil, pes(0xc0, pts+100))
		}
	}
	_ = sec
	return out
}

// putTS writes a PES timestamp with prefix bits pre (0x21 PTS only).
func putTS(b []byte, pre byte, v int64) {
	b[0] = pre&0xf1 | byte(v>>29)&0x0e
	b[1] = byte(v >> 22)
	b[2] = byte(v>>14) | 1
	b[3] = byte(v >> 7)
	b[4] = byte(v<<1) | 1
}

// videoPTS lists the video timestamps in a stream, in order.
func videoPTS(b []byte) []int64 {
	var out []int64
	for i := 0; i+tsPacket <= len(b); i += tsPacket {
		p := b[i : i+tsPacket]
		if p[0] != tsSync || p[1]&0x40 == 0 {
			continue
		}
		off := 4
		if p[3]&0x20 != 0 {
			off = 5 + int(p[4])
		}
		pes := p[off:]
		if len(pes) >= 14 && pes[0] == 0 && pes[1] == 0 && pes[2] == 1 && pes[3] == 0xe0 {
			if v, ok := readPTS(pes); ok {
				out = append(out, v)
			}
		}
	}
	return out
}
