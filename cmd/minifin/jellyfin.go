package main

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/zakijss/minideck/tnet"
	"github.com/zakijss/minideck/version"
)

// Client talks to a Jellyfin server (10.9 and later, Jellyfin 12
// included). Every request carries the standard Authorization header
// (MediaBrowser Client=..., Token=...); the token never goes into a URL, so
// it can't end up in a log line or the server's access log.
type Client struct {
	HTTP     *http.Client
	Stream   *http.Client // for video: no overall time limit
	DeviceID string

	mu     sync.RWMutex
	base   string
	token  string
	userID string
}

// Device is how MiniFin names the handheld to Jellyfin (Dashboard >
// Devices, the Quick Connect prompt).
const Device = "Miyoo Mini Plus"

// NewClient returns a client for the server at base, signed in with token
// (empty before sign-in).
func NewClient(base, token, userID, deviceID string) *Client {
	stream := tnet.Client(30 * time.Second)
	stream.Timeout = 0 // a video takes hours; stalls are timed by the reader
	return &Client{HTTP: tnet.Client(20 * time.Second), Stream: stream, DeviceID: deviceID,
		base: strings.TrimRight(base, "/"), token: token, userID: userID}
}

func (c *Client) streamClient() *http.Client { return c.Stream }

// explainNet is a network error as a sentence, without the URL.
func explainNet(err error) error { return tnet.Explain(err) }

// SetLogin switches the server and account (after setup or sign-out).
func (c *Client) SetLogin(base, token, userID string) {
	c.mu.Lock()
	c.base, c.token, c.userID = strings.TrimRight(base, "/"), token, userID
	c.mu.Unlock()
}

// Base is the server's address.
func (c *Client) Base() string {
	c.mu.RLock()
	defer c.mu.RUnlock()
	return c.base
}

// UserID is the signed-in user's id.
func (c *Client) UserID() string {
	c.mu.RLock()
	defer c.mu.RUnlock()
	return c.userID
}

// SignedIn reports whether the client has a token.
func (c *Client) SignedIn() bool {
	c.mu.RLock()
	defer c.mu.RUnlock()
	return c.token != ""
}

// AuthHeader is the value of the Authorization header: Jellyfin 12 accepts
// only this form (no X-Emby-Token, no api_key in the URL).
func (c *Client) AuthHeader() string {
	c.mu.RLock()
	token := c.token
	c.mu.RUnlock()
	return authHeader(c.DeviceID, token)
}

func authHeader(deviceID, token string) string {
	h := fmt.Sprintf(`MediaBrowser Client="MiniFin", Device="%s", DeviceId="%s", Version="%s"`,
		Device, headerValue(deviceID), headerValue(version.Version))
	if token != "" {
		h += `, Token="` + headerValue(token) + `"`
	}
	return h
}

// headerValue keeps a value from breaking out of its quotes.
func headerValue(s string) string {
	return strings.NewReplacer(`"`, "", "\r", "", "\n", "").Replace(s)
}

// screenError is an error written as a sentence for the screen.
type screenError string

func (e screenError) Error() string { return string(e) }

// errSignedOut: the server no longer accepts the token (signed out on the
// server, or the device was removed in Dashboard > Devices).
var errSignedOut = screenError("Jellyfin signed this device out. Sign in again in Settings > Server.")

// httpError is a reply other than 2xx.
type httpError struct {
	Status int
	Body   string
}

func (e *httpError) Error() string {
	if e.Body != "" {
		return fmt.Sprintf("the server said HTTP %d: %s", e.Status, e.Body)
	}
	return fmt.Sprintf("the server said HTTP %d", e.Status)
}

// statusOf is the HTTP status in err, or 0.
func statusOf(err error) int {
	var he *httpError
	if errors.As(err, &he) {
		return he.Status
	}
	return 0
}

// netError marks a request that never got an answer: the server or the
// network is down. The app goes offline on these.
type netError struct{ err error }

func (e *netError) Error() string { return e.err.Error() }
func (e *netError) Unwrap() error { return e.err }

// isOffline reports whether err means the server couldn't be reached.
func isOffline(err error) bool {
	var ne *netError
	return errors.As(err, &ne)
}

// request sends one API call. q is the query; body, if not nil, is sent as
// JSON; out, if not nil, receives the JSON reply.
func (c *Client) request(ctx context.Context, method, path string, q url.Values, body, out any) error {
	u := c.Base() + path
	if len(q) > 0 {
		u += "?" + q.Encode()
	}
	var rd io.Reader
	var raw []byte
	if body != nil {
		b, err := json.Marshal(body)
		if err != nil {
			return err
		}
		raw = b
		rd = bytes.NewReader(b)
	}
	req, err := http.NewRequestWithContext(ctx, method, u, rd)
	if err != nil {
		return err
	}
	if raw != nil {
		req.Header.Set("Content-Type", "application/json")
		req.GetBody = func() (io.ReadCloser, error) { return io.NopCloser(bytes.NewReader(raw)), nil }
	}
	req.Header.Set("Authorization", c.AuthHeader())
	req.Header.Set("Accept", "application/json")
	resp, err := c.HTTP.Do(req)
	if err != nil {
		// tnet.Explain drops the URL from the message.
		return &netError{tnet.Explain(err)}
	}
	defer resp.Body.Close()
	if resp.StatusCode == http.StatusUnauthorized && c.SignedIn() {
		return errSignedOut
	}
	if resp.StatusCode < 200 || resp.StatusCode > 299 {
		msg, _ := io.ReadAll(io.LimitReader(resp.Body, 300))
		return &httpError{resp.StatusCode, strings.TrimSpace(string(msg))}
	}
	if out == nil {
		io.Copy(io.Discard, io.LimitReader(resp.Body, 1<<20))
		return nil
	}
	dec := json.NewDecoder(io.LimitReader(resp.Body, 16<<20))
	if err := dec.Decode(out); err != nil {
		return fmt.Errorf("the server's reply didn't make sense (%v)", err)
	}
	return nil
}

func (c *Client) get(path string, q url.Values, out any) error {
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	return c.request(ctx, "GET", path, q, nil, out)
}

func (c *Client) post(path string, q url.Values, body, out any) error {
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	return c.request(ctx, "POST", path, q, body, out)
}

// ---- types, as the server sends them (only the fields MiniFin uses) ----

// PublicInfo is /System/Info/Public: what a server says before sign-in.
type PublicInfo struct {
	ServerName   string `json:"ServerName"`
	Version      string `json:"Version"`
	ProductName  string `json:"ProductName"`
	ID           string `json:"Id"`
	LocalAddress string `json:"LocalAddress"`
}

// User is the signed-in account.
type User struct {
	Name string `json:"Name"`
	ID   string `json:"Id"`
}

// AuthResult is the reply to a sign-in.
type AuthResult struct {
	User        User   `json:"User"`
	AccessToken string `json:"AccessToken"`
	ServerID    string `json:"ServerId"`
}

// QuickConnect is a Quick Connect request: the code to show and the secret
// to poll with.
type QuickConnect struct {
	Authenticated bool   `json:"Authenticated"`
	Secret        string `json:"Secret"`
	Code          string `json:"Code"`
}

// UserData is the signed-in user's state for an item.
type UserData struct {
	PlaybackPositionTicks int64   `json:"PlaybackPositionTicks"`
	PlayedPercentage      float64 `json:"PlayedPercentage"`
	Played                bool    `json:"Played"`
	UnplayedItemCount     int     `json:"UnplayedItemCount"`
	IsFavorite            bool    `json:"IsFavorite"`
}

// MediaStream is one stream of a media source (video, audio, subtitle).
type MediaStream struct {
	Type                 string `json:"Type"` // Video, Audio, Subtitle
	Index                int    `json:"Index"`
	Codec                string `json:"Codec"`
	Language             string `json:"Language"`
	DisplayTitle         string `json:"DisplayTitle"`
	Title                string `json:"Title"`
	IsDefault            bool   `json:"IsDefault"`
	IsForced             bool   `json:"IsForced"`
	IsExternal           bool   `json:"IsExternal"`
	IsTextSubtitleStream bool   `json:"IsTextSubtitleStream"`
	Channels             int    `json:"Channels"`
	Width                int    `json:"Width"`
	Height               int    `json:"Height"`
	DeliveryMethod       string `json:"DeliveryMethod"`
	DeliveryURL          string `json:"DeliveryUrl"`
}

// MediaSource is one version of an item's file.
type MediaSource struct {
	ID                         string        `json:"Id"`
	Container                  string        `json:"Container"`
	RunTimeTicks               int64         `json:"RunTimeTicks"`
	Size                       int64         `json:"Size"`
	Bitrate                    int64         `json:"Bitrate"`
	SupportsTranscoding        bool          `json:"SupportsTranscoding"`
	TranscodingURL             string        `json:"TranscodingUrl"`
	TranscodingSubProtocol     string        `json:"TranscodingSubProtocol"`
	TranscodingContainer       string        `json:"TranscodingContainer"`
	MediaStreams               []MediaStream `json:"MediaStreams"`
	DefaultAudioStreamIndex    *int          `json:"DefaultAudioStreamIndex"`
	DefaultSubtitleStreamIndex *int          `json:"DefaultSubtitleStreamIndex"`
}

// Item is a BaseItemDto: a movie, series, season, episode, folder or
// library.
type Item struct {
	ID                      string            `json:"Id"`
	Name                    string            `json:"Name"`
	SortName                string            `json:"SortName,omitempty"`
	Type                    string            `json:"Type"`
	CollectionType          string            `json:"CollectionType,omitempty"`
	IsFolder                bool              `json:"IsFolder,omitempty"`
	ProductionYear          int               `json:"ProductionYear,omitempty"`
	EndDate                 string            `json:"EndDate,omitempty"`
	RunTimeTicks            int64             `json:"RunTimeTicks,omitempty"`
	OfficialRating          string            `json:"OfficialRating,omitempty"`
	CommunityRating         float64           `json:"CommunityRating,omitempty"`
	Overview                string            `json:"Overview,omitempty"`
	SeriesID                string            `json:"SeriesId,omitempty"`
	SeriesName              string            `json:"SeriesName,omitempty"`
	SeasonID                string            `json:"SeasonId,omitempty"`
	SeasonName              string            `json:"SeasonName,omitempty"`
	IndexNumber             int               `json:"IndexNumber,omitempty"`
	ParentIndexNumber       int               `json:"ParentIndexNumber,omitempty"`
	ChildCount              int               `json:"ChildCount,omitempty"`
	ImageTags               map[string]string `json:"ImageTags,omitempty"`
	BackdropImageTags       []string          `json:"BackdropImageTags,omitempty"`
	ParentThumbItemID       string            `json:"ParentThumbItemId,omitempty"`
	ParentThumbImageTag     string            `json:"ParentThumbImageTag,omitempty"`
	ParentBackdropItemID    string            `json:"ParentBackdropItemId,omitempty"`
	ParentBackdropImageTags []string          `json:"ParentBackdropImageTags,omitempty"`
	SeriesPrimaryImageTag   string            `json:"SeriesPrimaryImageTag,omitempty"`
	UserData                *UserData         `json:"UserData,omitempty"`
	MediaSources            []MediaSource     `json:"MediaSources,omitempty"`
	MediaStreams            []MediaStream     `json:"MediaStreams,omitempty"`
}

// ItemPage is a page of a list: Items from StartIndex, out of
// TotalRecordCount.
type ItemPage struct {
	Items            []Item `json:"Items"`
	TotalRecordCount int    `json:"TotalRecordCount"`
	StartIndex       int    `json:"StartIndex"`
}

// ticks are Jellyfin's time unit: 100 ns.
const ticksPerSecond = 10_000_000

func toTicks(sec float64) int64  { return int64(sec*ticksPerSecond + 0.5) }
func toSeconds(t int64) float64  { return float64(t) / ticksPerSecond }
func (it Item) Runtime() float64 { return toSeconds(it.RunTimeTicks) }
func (it Item) IsPlayable() bool { return isVideo(it.Type) }
func (it Item) Resume() float64 {
	if it.UserData == nil {
		return 0
	}
	return toSeconds(it.UserData.PlaybackPositionTicks)
}

// Played reports whether the user has watched it.
func (it Item) Played() bool { return it.UserData != nil && it.UserData.Played }

// isVideo reports whether items of type t play (as opposed to open).
func isVideo(t string) bool {
	switch t {
	case "Movie", "Episode", "Video", "MusicVideo", "Trailer":
		return true
	}
	return false
}

// ---- calls ----

// Fields asked for in lists: only what the screens show, so a page of a
// big library stays small. The basic fields (name, year, runtime, rating,
// image tags, user data) always come.
const (
	listFields  = "SortName"
	imageParams = "Primary,Thumb,Backdrop"
)

// listQuery is the common part of every list request.
func (c *Client) listQuery(extra url.Values) url.Values {
	q := url.Values{
		"userId":           {c.UserID()},
		"EnableImageTypes": {imageParams},
		"ImageTypeLimit":   {"1"},
		"EnableUserData":   {"true"},
	}
	for k, v := range extra {
		q[k] = v
	}
	return q
}

// Info is /System/Info/Public at the client's base.
func (c *Client) Info() (PublicInfo, error) {
	var pi PublicInfo
	ctx, cancel := context.WithTimeout(context.Background(), 8*time.Second)
	defer cancel()
	err := c.request(ctx, "GET", "/System/Info/Public", nil, nil, &pi)
	if err == nil && pi.ID == "" && !strings.Contains(strings.ToLower(pi.ProductName), "jellyfin") {
		return pi, screenError("That address answers, but it isn't a Jellyfin server.")
	}
	if statusOf(err) == http.StatusNotFound {
		return pi, screenError("That address answers, but it isn't a Jellyfin server.")
	}
	return pi, err
}

// QuickConnectEnabled reports whether the server allows Quick Connect.
func (c *Client) QuickConnectEnabled() (bool, error) {
	var on bool
	err := c.get("/QuickConnect/Enabled", nil, &on)
	return on, err
}

// QuickConnectInitiate asks for a code (POST in Jellyfin 10.9 and later).
func (c *Client) QuickConnectInitiate() (QuickConnect, error) {
	var qc QuickConnect
	err := c.post("/QuickConnect/Initiate", nil, nil, &qc)
	switch statusOf(err) {
	case http.StatusUnauthorized:
		return qc, screenError("Quick Connect is turned off on this server. Turn it on in Dashboard > General, or sign in with a password.")
	case http.StatusMethodNotAllowed:
		err = c.get("/QuickConnect/Initiate", nil, &qc) // Jellyfin 10.8
	}
	if err == nil && (qc.Secret == "" || qc.Code == "") {
		err = screenError("The server didn't send a Quick Connect code.")
	}
	return qc, err
}

// errCodeExpired: the server forgot the request (it keeps them 10 minutes).
var errCodeExpired = screenError("The code expired.")

// QuickConnectState polls a request: Authenticated turns true once someone
// approves the code.
func (c *Client) QuickConnectState(secret string) (QuickConnect, error) {
	var qc QuickConnect
	err := c.get("/QuickConnect/Connect", url.Values{"secret": {secret}}, &qc)
	if statusOf(err) == http.StatusNotFound {
		err = errCodeExpired
	}
	return qc, err
}

// AuthenticateQuickConnect turns an approved request into a token.
func (c *Client) AuthenticateQuickConnect(secret string) (AuthResult, error) {
	var ar AuthResult
	err := c.post("/Users/AuthenticateWithQuickConnect", nil, map[string]string{"Secret": secret}, &ar)
	if err == nil && ar.AccessToken == "" {
		err = screenError("The server didn't send a token.")
	}
	return ar, err
}

// AuthenticateByName signs in with a user name and password.
func (c *Client) AuthenticateByName(user, pw string) (AuthResult, error) {
	var ar AuthResult
	err := c.post("/Users/AuthenticateByName", nil, map[string]string{"Username": user, "Pw": pw}, &ar)
	if s := statusOf(err); s == http.StatusUnauthorized || s == http.StatusBadRequest {
		return ar, screenError("Wrong user name or password.")
	}
	if err == nil && ar.AccessToken == "" {
		err = screenError("The server didn't send a token.")
	}
	return ar, err
}

// Logout ends this device's session on the server.
func (c *Client) Logout() error { return c.post("/Sessions/Logout", nil, nil, nil) }

// Views are the user's libraries.
func (c *Client) Views() ([]Item, error) {
	var p ItemPage
	err := c.get("/UserViews", url.Values{"userId": {c.UserID()}}, &p)
	return p.Items, err
}

// Resume is Continue Watching: videos started and not finished.
func (c *Client) Resume(limit int) ([]Item, error) {
	var p ItemPage
	err := c.get("/UserItems/Resume", c.listQuery(url.Values{"Limit": {strconv.Itoa(limit)},
		"MediaTypes": {"Video"}, "EnableTotalRecordCount": {"false"}}), &p)
	return p.Items, err
}

// NextUp is the next episode of each show being watched.
func (c *Client) NextUp(limit int) ([]Item, error) {
	var p ItemPage
	err := c.get("/Shows/NextUp", c.listQuery(url.Values{"Limit": {strconv.Itoa(limit)},
		"EnableTotalRecordCount": {"false"}, "EnableResumable": {"false"}}), &p)
	return p.Items, err
}

// NextUpFor is the next episode of one show.
func (c *Client) NextUpFor(seriesID string) ([]Item, error) {
	var p ItemPage
	err := c.get("/Shows/NextUp", c.listQuery(url.Values{"seriesId": {seriesID}, "Limit": {"1"},
		"EnableTotalRecordCount": {"false"}}), &p)
	return p.Items, err
}

// Latest is what was added last, newest first (an episode list is grouped
// into its show).
func (c *Client) Latest(parentID string, limit int) ([]Item, error) {
	q := c.listQuery(url.Values{"Limit": {strconv.Itoa(limit)}})
	if parentID != "" {
		q.Set("ParentId", parentID)
	}
	var items []Item // this call returns a bare array
	err := c.get("/Items/Latest", q, &items)
	return items, err
}

// Query is a page of a library or folder.
type Query struct {
	ParentID   string
	Types      string // IncludeItemTypes, e.g. "Movie" or "Series"
	Recursive  bool
	SortBy     string
	Descending bool
	Unplayed   bool
	Search     string
	NameBefore string // NameLessThan: counts the items before a letter
}

func (q Query) values(start, limit int) url.Values {
	v := url.Values{
		"StartIndex": {strconv.Itoa(start)},
		"Limit":      {strconv.Itoa(limit)},
		"Fields":     {listFields},
	}
	if q.ParentID != "" {
		v.Set("ParentId", q.ParentID)
	}
	if q.Types != "" {
		v.Set("IncludeItemTypes", q.Types)
	}
	if q.Recursive {
		v.Set("Recursive", "true")
	}
	sortBy := q.SortBy
	if sortBy == "" {
		sortBy = "SortName"
	}
	v.Set("SortBy", sortBy)
	if q.Descending {
		v.Set("SortOrder", "Descending")
	} else {
		v.Set("SortOrder", "Ascending")
	}
	if q.Unplayed {
		v.Set("Filters", "IsUnplayed")
	}
	if q.Search != "" {
		v.Set("searchTerm", q.Search)
	}
	if q.NameBefore != "" {
		v.Set("NameLessThan", q.NameBefore)
	}
	return v
}

// Items returns limit items of q from start, and how many there are.
func (c *Client) Items(q Query, start, limit int) (ItemPage, error) {
	var p ItemPage
	err := c.get("/Items", c.listQuery(q.values(start, limit)), &p)
	return p, err
}

// CountBefore is how many items of q sort before name: the index the
// list jumps to for a letter.
func (c *Client) CountBefore(q Query, name string) (int, error) {
	q.NameBefore = name
	var p ItemPage
	v := q.values(0, 0)
	v.Set("Fields", "")
	err := c.get("/Items", c.listQuery(v), &p)
	return p.TotalRecordCount, err
}

// Item returns one item with everything about it (its media sources and
// streams included).
func (c *Client) Item(id string) (Item, error) {
	var it Item
	err := c.get("/Items/"+url.PathEscape(id), url.Values{"userId": {c.UserID()}}, &it)
	return it, err
}

// Seasons are a show's seasons.
func (c *Client) Seasons(seriesID string) ([]Item, error) {
	var p ItemPage
	err := c.get("/Shows/"+url.PathEscape(seriesID)+"/Seasons", c.listQuery(nil), &p)
	return p.Items, err
}

// Episodes are a season's episodes, or with no season the whole show's,
// in order.
func (c *Client) Episodes(seriesID, seasonID string) ([]Item, error) {
	var p ItemPage
	q := url.Values{}
	if seasonID != "" {
		q.Set("seasonId", seasonID)
	}
	err := c.get("/Shows/"+url.PathEscape(seriesID)+"/Episodes", c.listQuery(q), &p)
	return p.Items, err
}

// StopEncoding asks the server to stop the transcode of a play session
// (after a seek started a new one), so it doesn't run two at once. Best
// effort: a server without the call ends it when the connection closes.
func (c *Client) StopEncoding(playSessionID string) error {
	if playSessionID == "" {
		return nil
	}
	ctx, cancel := context.WithTimeout(context.Background(), 8*time.Second)
	defer cancel()
	err := c.request(ctx, "DELETE", "/Videos/ActiveEncodings", url.Values{"deviceId": {c.DeviceID}, "playSessionId": {playSessionID}}, nil, nil)
	if statusOf(err) == http.StatusNotFound {
		return nil
	}
	return err
}

// SetUserData records a position (and whether it was watched) for an item
// played offline.
func (c *Client) SetUserData(id string, pos float64, played bool) error {
	return c.post("/UserItems/"+url.PathEscape(id)+"/UserData", url.Values{"userId": {c.UserID()}},
		map[string]any{"PlaybackPositionTicks": toTicks(pos), "Played": played}, nil)
}

// ImageURL is the address of an item's image, scaled by the server to fill
// w x h (and cropped to it on the device). tag makes it cacheable.
func (c *Client) ImageURL(id, kind, tag string, w, h int) string {
	q := url.Values{"fillWidth": {strconv.Itoa(w)}, "fillHeight": {strconv.Itoa(h)}, "quality": {"80"}}
	if tag != "" {
		q.Set("tag", tag)
	}
	return c.Base() + "/Items/" + url.PathEscape(id) + "/Images/" + kind + "?" + q.Encode()
}

// fetch GETs a URL on the server with the login header (images,
// subtitles). It returns at most max bytes.
func (c *Client) fetch(ctx context.Context, u string, max int64) ([]byte, error) {
	req, err := http.NewRequestWithContext(ctx, "GET", u, nil)
	if err != nil {
		return nil, err
	}
	req.Header.Set("Authorization", c.AuthHeader())
	resp, err := c.HTTP.Do(req)
	if err != nil {
		return nil, &netError{tnet.Explain(err)}
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return nil, &httpError{Status: resp.StatusCode}
	}
	return io.ReadAll(io.LimitReader(resp.Body, max))
}
