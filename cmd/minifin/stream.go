package main

import (
	"context"
	"fmt"
	"net/url"
	"strconv"
	"strings"
	"time"
)

// The device decodes video in software on two Cortex-A7 cores, so the
// server transcodes everything to what that can play smoothly at the
// screen's size: H.264 (main profile, level 4.0 at most), 640x480 at most,
// 30 frames a second, with AAC stereo, in an MPEG-TS stream. Nothing is
// played directly from the file.
const (
	maxWidth     = 640
	maxHeight    = 480
	maxFramerate = 30
	audioBitrate = 96_000
	h264Level    = 40
)

// videoRates are the choices in Settings > Streaming quality, in kbit/s.
var videoRates = []int{600, 1200, 2000}

// rateText writes a video bit rate: "1.2 Mbit/s".
func rateText(kbps int) string {
	return strings.TrimSuffix(fmt.Sprintf("%.1f", float64(kbps)/1000), ".0") + " Mbit/s"
}

// streamBytes is about how many bytes an hour of video at kbps takes, with
// the audio and the MPEG-TS overhead (about 6 %).
func streamBytes(kbps int, sec float64) int64 {
	bits := float64(kbps*1000+audioBitrate) * 1.06
	return int64(bits / 8 * sec)
}

// subtitleCodecs that are text, which the server can turn into SRT for the
// player to draw; anything else (PGS, VobSub, DVB) is an image and has to
// be burned into the picture by the server.
var textSubtitles = map[string]bool{
	"srt": true, "subrip": true, "ass": true, "ssa": true, "vtt": true, "webvtt": true,
	"mov_text": true, "text": true, "ttml": true, "smi": true, "sami": true,
}

// deviceProfile describes the device to the server, so PlaybackInfo picks
// a transcode it can play. External text subtitles only when the player
// draws them (minifin-player does; ffplay can't), else every subtitle is
// burned in.
func deviceProfile(kbps int, externalSubs bool) map[string]any {
	total := kbps*1000 + audioBitrate
	cond := func(c, prop, val string) map[string]any {
		return map[string]any{"Condition": c, "Property": prop, "Value": val, "IsRequired": true}
	}
	var subs []map[string]any
	for _, f := range []string{"srt", "subrip", "ass", "ssa", "vtt", "webvtt", "mov_text", "text", "ttml", "smi", "sami"} {
		m := "Encode"
		if externalSubs {
			m = "External"
		}
		subs = append(subs, map[string]any{"Format": f, "Method": m})
	}
	for _, f := range []string{"pgssub", "pgs", "dvdsub", "dvbsub", "vobsub", "xsub", "dvb_subtitle", "dvd_subtitle", "hdmv_pgs_subtitle"} {
		subs = append(subs, map[string]any{"Format": f, "Method": "Encode"})
	}
	return map[string]any{
		"Name":                             "MiniFin (Miyoo Mini Plus)",
		"MaxStreamingBitrate":              total,
		"MaxStaticBitrate":                 total,
		"MusicStreamingTranscodingBitrate": audioBitrate,
		"DirectPlayProfiles":               []any{},
		"TranscodingProfiles": []map[string]any{{
			"Container": "ts", "Type": "Video", "VideoCodec": "h264", "AudioCodec": "aac",
			"Protocol": "http", "Context": "Streaming", "MaxAudioChannels": "2",
			"BreakOnNonKeyFrames": false, "CopyTimestamps": false, "EnableMpegtsM2TsMode": false,
			"EstimateContentLength": false, "TranscodeSeekInfo": "Auto",
		}},
		"ContainerProfiles": []any{},
		"CodecProfiles": []map[string]any{
			{"Type": "Video", "Codec": "h264", "Conditions": []map[string]any{
				cond("LessThanEqual", "Width", strconv.Itoa(maxWidth)),
				cond("LessThanEqual", "Height", strconv.Itoa(maxHeight)),
				cond("LessThanEqual", "VideoFramerate", strconv.Itoa(maxFramerate)),
				cond("LessThanEqual", "VideoLevel", strconv.Itoa(h264Level)),
				cond("EqualsAny", "VideoProfile", "main|baseline|constrained baseline"),
				cond("LessThanEqual", "VideoBitDepth", "8"),
				cond("LessThanEqual", "VideoBitrate", strconv.Itoa(kbps*1000)),
				cond("Equals", "IsInterlaced", "false"),
			}},
			{"Type": "VideoAudio", "Codec": "aac", "Conditions": []map[string]any{
				cond("LessThanEqual", "AudioChannels", "2"),
				cond("LessThanEqual", "AudioBitrate", strconv.Itoa(audioBitrate)),
			}},
		},
		"SubtitleProfiles": subs,
	}
}

// PlaybackInfo is the server's plan for playing an item.
type PlaybackInfo struct {
	MediaSources  []MediaSource `json:"MediaSources"`
	PlaySessionID string        `json:"PlaySessionId"`
	ErrorCode     string        `json:"ErrorCode"`
}

// streamPlan is what MiniFin plays: the server's transcode URL (without
// any token), the session to report under, and the subtitle to fetch.
type streamPlan struct {
	ItemID        string
	MediaSourceID string
	PlaySessionID string
	URL           string // absolute, on the server, no credentials
	Start         float64
	Audio, Sub    int // stream indexes, -1 for none
	SubURL        string
	Runtime       float64
}

// playbackInfo asks the server how to play item id from start seconds,
// with audio stream audio and subtitle stream sub (-1: the default audio,
// no subtitles).
func (c *Client) playbackInfo(id, msid string, start float64, audio, sub, kbps int, externalSubs bool) (PlaybackInfo, error) {
	body := map[string]any{
		"UserId":               c.UserID(),
		"DeviceProfile":        deviceProfile(kbps, externalSubs),
		"MaxStreamingBitrate":  kbps*1000 + audioBitrate,
		"StartTimeTicks":       toTicks(start),
		"MaxAudioChannels":     2,
		"EnableDirectPlay":     false,
		"EnableDirectStream":   false,
		"EnableTranscoding":    true,
		"AllowVideoStreamCopy": false,
		"AllowAudioStreamCopy": false,
		"AutoOpenLiveStream":   true,
	}
	if msid != "" {
		body["MediaSourceId"] = msid
	}
	if audio >= 0 {
		body["AudioStreamIndex"] = audio
	}
	if sub >= 0 {
		body["SubtitleStreamIndex"] = sub
	} else {
		body["SubtitleStreamIndex"] = -1
	}
	var pi PlaybackInfo
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	err := c.request(ctx, "POST", "/Items/"+url.PathEscape(id)+"/PlaybackInfo", url.Values{"userId": {c.UserID()}}, body, &pi)
	if err == nil && pi.ErrorCode != "" {
		err = screenError("The server can't play this (" + pi.ErrorCode + ").")
	}
	return pi, err
}

// Plan works out the stream for item it: PlaybackInfo with the device's
// profile, then the TranscodingUrl it returns with the device's limits made
// sure of. If the server's answer has no transcode (an old server, or a
// profile it didn't take), MiniFin builds the same URL itself.
func (c *Client) Plan(it Item, start float64, audio, sub, kbps int, externalSubs bool) (streamPlan, error) {
	msid := ""
	if len(it.MediaSources) > 0 {
		msid = it.MediaSources[0].ID
	}
	p := streamPlan{ItemID: it.ID, MediaSourceID: msid, Start: start, Audio: audio, Sub: sub, Runtime: it.Runtime()}
	pi, err := c.playbackInfo(it.ID, msid, start, audio, sub, kbps, externalSubs)
	if err != nil {
		return p, err
	}
	p.PlaySessionID = pi.PlaySessionID
	var src *MediaSource
	for i := range pi.MediaSources {
		if msid == "" || pi.MediaSources[i].ID == msid {
			src = &pi.MediaSources[i]
			break
		}
	}
	if src == nil && len(pi.MediaSources) > 0 {
		src = &pi.MediaSources[0]
	}
	if src != nil {
		p.MediaSourceID = src.ID
		if src.RunTimeTicks > 0 {
			p.Runtime = toSeconds(src.RunTimeTicks)
		}
	}
	raw := ""
	if src != nil && src.TranscodingURL != "" && !strings.EqualFold(src.TranscodingSubProtocol, "hls") {
		raw = src.TranscodingURL
	}
	if raw == "" {
		raw = "/Videos/" + url.PathEscape(it.ID) + "/stream.ts"
	}
	// A text subtitle the server delivers on the side: the player gets it
	// as an SRT file. Any other is burned into the picture.
	if sub >= 0 && externalSubs && src != nil {
		for _, ms := range src.MediaStreams {
			if ms.Type == "Subtitle" && ms.Index == sub && strings.EqualFold(ms.DeliveryMethod, "External") {
				p.SubURL = c.subtitleURL(it.ID, p.MediaSourceID, sub)
			}
		}
	}
	u, err := c.enforce(raw, p, kbps, sub >= 0 && p.SubURL == "")
	if err != nil {
		return p, err
	}
	p.URL = u
	return p, nil
}

// enforce makes the transcode URL ask for exactly what the device plays,
// whatever the server put in it, and drops any credentials from it (the
// relay sends the Authorization header instead).
//
// burn asks for subtitle stream p.Sub to be drawn into the picture (an
// image subtitle, or any subtitle when the player can't draw them).
func (c *Client) enforce(raw string, p streamPlan, kbps int, burn bool) (string, error) {
	u, err := url.Parse(raw)
	if err != nil {
		return "", fmt.Errorf("the server's stream address doesn't parse")
	}
	base, err := url.Parse(c.Base() + "/")
	if err != nil {
		return "", err
	}
	if !u.IsAbs() {
		// TranscodingUrl is relative to the server root; keep a base path
		// (a server behind https://host/jellyfin).
		u = base.ResolveReference(&url.URL{Path: strings.TrimPrefix(u.Path, "/"), RawQuery: u.RawQuery})
	}
	q := u.Query()
	for k := range q {
		switch strings.ToLower(k) {
		case "api_key", "apikey", "x-emby-token", "token":
			q.Del(k)
		}
	}
	set := func(k, v string) {
		for old := range q { // the server may send these in another case
			if strings.EqualFold(old, k) {
				q.Del(old)
			}
		}
		q.Set(k, v)
	}
	set("DeviceId", c.DeviceID)
	set("MediaSourceId", p.MediaSourceID)
	if p.PlaySessionID != "" {
		set("PlaySessionId", p.PlaySessionID)
	}
	set("Container", "ts")
	set("VideoCodec", "h264")
	set("AudioCodec", "aac")
	set("MaxWidth", strconv.Itoa(maxWidth))
	set("MaxHeight", strconv.Itoa(maxHeight))
	set("MaxFramerate", strconv.Itoa(maxFramerate))
	set("VideoBitRate", strconv.Itoa(kbps*1000))
	set("AudioBitRate", strconv.Itoa(audioBitrate))
	set("AudioChannels", "2")
	set("MaxAudioChannels", "2")
	set("TranscodingMaxAudioChannels", "2")
	set("MaxVideoBitDepth", "8")
	set("h264-profile", "main,baseline,constrainedbaseline")
	set("h264-level", strconv.Itoa(h264Level))
	set("RequireAvc", "false")
	set("EnableAutoStreamCopy", "false")
	set("AllowVideoStreamCopy", "false")
	set("AllowAudioStreamCopy", "false")
	set("Static", "false")
	set("StartTimeTicks", strconv.FormatInt(toTicks(p.Start), 10))
	if p.Audio >= 0 {
		set("AudioStreamIndex", strconv.Itoa(p.Audio))
	}
	switch {
	case burn:
		set("SubtitleStreamIndex", strconv.Itoa(p.Sub))
		set("SubtitleMethod", "Encode")
	case p.Sub < 0:
		set("SubtitleStreamIndex", "-1")
		q.Del("SubtitleMethod")
	default: // drawn by the player from the SRT file: not in the picture
		q.Del("SubtitleMethod")
		set("SubtitleStreamIndex", "-1")
	}
	u.RawQuery = q.Encode()
	return u.String(), nil
}

// subtitleURL is where the server sends subtitle stream index as SRT, its
// times counted from the start of the video.
func (c *Client) subtitleURL(id, msid string, index int) string {
	return fmt.Sprintf("%s/Videos/%s/%s/Subtitles/%d/0/Stream.srt", c.Base(), url.PathEscape(id), url.PathEscape(msid), index)
}

// pickStreams works out the audio and subtitle tracks to play: the user's
// choice when there is one, else the server's defaults (which follow the
// user's language settings in Jellyfin) and the subtitle setting.
func pickStreams(it Item, audio, sub *int, subMode string) (int, int) {
	var src MediaSource
	if len(it.MediaSources) > 0 {
		src = it.MediaSources[0]
	}
	streams := src.MediaStreams
	if len(streams) == 0 {
		streams = it.MediaStreams
	}
	a := -1
	if audio != nil {
		a = *audio
	} else if src.DefaultAudioStreamIndex != nil {
		a = *src.DefaultAudioStreamIndex
	}
	s := -1
	switch {
	case sub != nil:
		s = *sub
	case subMode == "off":
	case subMode == "on":
		for _, ms := range streams {
			if ms.Type == "Subtitle" && (s < 0 || ms.IsDefault) {
				s = ms.Index
			}
		}
	default: // the server's choice
		if src.DefaultSubtitleStreamIndex != nil {
			s = *src.DefaultSubtitleStreamIndex
		}
	}
	return a, s
}

// streamsOf lists an item's streams of one type.
func streamsOf(it Item, typ string) []MediaStream {
	streams := it.MediaStreams
	if len(it.MediaSources) > 0 && len(it.MediaSources[0].MediaStreams) > 0 {
		streams = it.MediaSources[0].MediaStreams
	}
	var out []MediaStream
	for _, s := range streams {
		if s.Type == typ {
			out = append(out, s)
		}
	}
	return out
}

// streamName is a short name for a track: "English · AAC 5.1".
func streamName(s MediaStream) string {
	name := s.DisplayTitle
	if name == "" {
		name = s.Title
	}
	if name == "" {
		name = s.Language
	}
	if name == "" {
		name = strings.ToUpper(s.Codec)
	}
	if name == "" {
		name = fmt.Sprintf("Track %d", s.Index)
	}
	return name
}
