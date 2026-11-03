package main

// MPEG-TS, as far as downloads need it. A download is kept as one or more
// parts: each is a transcode the server started at some point in the video
// (the first at 0, the next where an interrupted one stopped). While a part
// downloads, tsScanner notes its first timestamp and every keyframe, so an
// interrupted part can be cut at its last keyframe and the next part asked
// for from exactly there, and offline playback can start at a keyframe
// near the resume point. When parts are played one after another, each
// one's timestamps are moved (shiftTS) so the stream runs on without a
// jump back to zero where they meet.

const (
	tsPacket = 188
	tsSync   = 0x47
	ptsWrap  = 1 << 33
	ptsHz    = 90000
)

// keyPoint is a place a download can start playing from: the packet
// offset in its part (the PAT just before a keyframe, so the player finds
// the stream's tables at once) and the keyframe's timestamp.
type keyPoint struct {
	Off int64 `json:"off"`
	PTS int64 `json:"pts"`
}

// tsScanner follows a part as it is written.
type tsScanner struct {
	off      int64 // bytes seen
	partial  []byte
	FirstPTS int64      // the first video timestamp, -1 until seen
	LastPTS  int64      // the latest video timestamp
	lastPAT  int64      // offset of the latest PAT packet, -1 if none
	Keys     []keyPoint // start points, about every keyEvery
	LastKey  keyPoint   // the latest keyframe
	HasKey   bool
	keyEvery int64 // keep a key point at most every this many ticks
}

func newTSScanner() *tsScanner {
	// a start point about every 2 s (a little less, as keyframe times
	// wobble)
	return &tsScanner{FirstPTS: -1, LastPTS: -1, lastPAT: -1, keyEvery: 2*ptsHz - ptsHz/10}
}

// Write feeds the next bytes of the part.
func (t *tsScanner) Write(b []byte) (int, error) {
	n := len(b)
	if len(t.partial) > 0 {
		need := tsPacket - len(t.partial)
		if len(b) < need {
			t.partial = append(t.partial, b...)
			t.off += int64(len(b))
			return n, nil
		}
		t.partial = append(t.partial, b[:need]...)
		t.packet(t.partial, t.off-int64(len(t.partial))+int64(need))
		t.partial = t.partial[:0]
		b = b[need:]
		t.off += int64(need)
	}
	for len(b) >= tsPacket {
		t.packet(b[:tsPacket], t.off)
		b = b[tsPacket:]
		t.off += tsPacket
	}
	if len(b) > 0 {
		t.partial = append(t.partial, b...)
		t.off += int64(len(b))
	}
	return n, nil
}

// packet looks at one packet at offset off.
func (t *tsScanner) packet(p []byte, off int64) {
	if p[0] != tsSync {
		return
	}
	pid := int(p[1]&0x1f)<<8 | int(p[2])
	if pid == 0 {
		t.lastPAT = off
		return
	}
	start := p[1]&0x40 != 0
	afc := p[3] >> 4 & 3
	rai := false
	payload := 4
	if afc&2 != 0 {
		al := int(p[4])
		if al > 0 && 5 < len(p) {
			rai = p[5]&0x40 != 0
		}
		payload = 5 + al
	}
	if !start || afc&1 == 0 || payload+14 > len(p) {
		return
	}
	pes := p[payload:]
	if pes[0] != 0 || pes[1] != 0 || pes[2] != 1 || pes[3]&0xf0 != 0xe0 { // video PES only
		return
	}
	pts, ok := readPTS(pes)
	if !ok {
		return
	}
	if t.FirstPTS < 0 {
		t.FirstPTS = pts
	}
	t.LastPTS = pts
	if !rai {
		return
	}
	at := off
	if t.lastPAT >= 0 && off-t.lastPAT <= 8*tsPacket {
		at = t.lastPAT
	}
	key := keyPoint{Off: at, PTS: pts}
	t.LastKey, t.HasKey = key, true // where an interrupted part is cut
	if n := len(t.Keys); n == 0 || ptsDiff(pts, t.Keys[n-1].PTS) >= t.keyEvery {
		t.Keys = append(t.Keys, key)
	}
}

// readPTS reads a PES header's presentation timestamp.
func readPTS(pes []byte) (int64, bool) {
	if len(pes) < 14 || pes[7]&0x80 == 0 {
		return 0, false
	}
	b := pes[9:14]
	return int64(b[0]>>1&7)<<30 | int64(b[1])<<22 | int64(b[2]>>1)<<15 | int64(b[3])<<7 | int64(b[4]>>1), true
}

// ptsDiff is a-b in ticks, allowing for the 33-bit wrap.
func ptsDiff(a, b int64) int64 {
	d := (a - b) % ptsWrap
	if d < -ptsWrap/2 {
		d += ptsWrap
	} else if d > ptsWrap/2 {
		d -= ptsWrap
	}
	return d
}

// shiftTS adds delta ticks to every timestamp (PCR, PTS, DTS) in the
// whole packets of b, in place.
func shiftTS(b []byte, delta int64) {
	if delta == 0 {
		return
	}
	for i := 0; i+tsPacket <= len(b); i += tsPacket {
		shiftPacket(b[i:i+tsPacket], delta)
	}
}

func shiftPacket(p []byte, delta int64) {
	if p[0] != tsSync {
		return
	}
	afc := p[3] >> 4 & 3
	payload := 4
	if afc&2 != 0 {
		al := int(p[4])
		payload = 5 + al
		if al >= 7 && p[5]&0x10 != 0 { // PCR
			c := p[6:12]
			base := int64(c[0])<<25 | int64(c[1])<<17 | int64(c[2])<<9 | int64(c[3])<<1 | int64(c[4]>>7)
			base = ((base+delta)%ptsWrap + ptsWrap) % ptsWrap
			c[0], c[1], c[2], c[3] = byte(base>>25), byte(base>>17), byte(base>>9), byte(base>>1)
			c[4] = byte(base&1)<<7 | c[4]&0x7f
		}
	}
	if p[1]&0x40 == 0 || afc&1 == 0 || payload+14 > len(p) {
		return
	}
	pes := p[payload:]
	if pes[0] != 0 || pes[1] != 0 || pes[2] != 1 {
		return
	}
	switch pes[3] {
	case 0xbc, 0xbe, 0xbf, 0xf0, 0xf1, 0xff, 0xf2, 0xf8: // no optional header
		return
	}
	flags := pes[7] >> 6
	if flags&2 != 0 {
		writeTS(pes[9:14], delta)
	}
	if flags == 3 && payload+19 <= len(p) {
		writeTS(pes[14:19], delta)
	}
}

// writeTS moves the 5-byte PES timestamp in b by delta, keeping its
// prefix and marker bits.
func writeTS(b []byte, delta int64) {
	v := int64(b[0]>>1&7)<<30 | int64(b[1])<<22 | int64(b[2]>>1)<<15 | int64(b[3])<<7 | int64(b[4]>>1)
	v = ((v+delta)%ptsWrap + ptsWrap) % ptsWrap
	b[0] = b[0]&0xf1 | byte(v>>29)&0x0e
	b[1] = byte(v >> 22)
	b[2] = byte(v>>14)&0xfe | b[2]&1
	b[3] = byte(v >> 7)
	b[4] = byte(v<<1)&0xfe | b[4]&1
}
