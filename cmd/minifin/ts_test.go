package main

import (
	"bytes"
	"testing"
)

// The scanner finds the first timestamp and a keyframe every 2 s, each
// starting at the PAT before it, whatever size the writes come in.
func TestTSScanner(t *testing.T) {
	data := makeTS(0, 30)
	for _, chunk := range []int{1, 7, 188, 1000, len(data)} {
		sc := newTSScanner()
		for i := 0; i < len(data); i += chunk {
			sc.Write(data[i:min(len(data), i+chunk)])
		}
		if sc.FirstPTS != 126000 || len(sc.Keys) != 15 || !sc.HasKey {
			t.Fatalf("chunk %d: first %d, %d keys", chunk, sc.FirstPTS, len(sc.Keys))
		}
		for i, k := range sc.Keys {
			if k.Off%tsPacket != 0 || data[k.Off+1]&0x1f != 0 || data[k.Off+2] != 0 { // a PAT packet
				t.Fatalf("chunk %d: key %d at %d isn't a PAT", chunk, i, k.Off)
			}
			if want := int64(126000 + i*2*ptsHz); k.PTS != want {
				t.Fatalf("key %d pts %d, want %d", i, k.PTS, want)
			}
		}
		if sc.LastPTS != 126000+59*ptsHz/2 {
			t.Fatalf("last pts %d", sc.LastPTS)
		}
	}
}

// shiftTS moves PTS, DTS and PCR, keeping the marker bits, and wraps at
// 33 bits.
func TestShiftTS(t *testing.T) {
	data := makeTS(0, 4)
	orig := append([]byte(nil), data...)
	shiftTS(data, 900000)
	a, b := videoPTS(orig), videoPTS(data)
	for i := range a {
		if b[i]-a[i] != 900000 {
			t.Fatalf("frame %d: %d -> %d", i, a[i], b[i])
		}
	}
	shiftTS(data, -900000)
	if !bytes.Equal(data, orig) {
		t.Fatal("shifting back doesn't give the original")
	}
	// wrap: near 2^33
	shiftTS(data, ptsWrap-126000-10)
	if v := videoPTS(data)[0]; v != ptsWrap-10 {
		t.Fatalf("wrapped to %d", v)
	}
	shiftTS(data, 20)
	if v := videoPTS(data)[0]; v != 10 {
		t.Fatalf("past the wrap: %d", v)
	}
	if ptsDiff(10, ptsWrap-10) != 20 || ptsDiff(ptsWrap-10, 10) != -20 {
		t.Fatal("ptsDiff across the wrap")
	}
	// the PCR moved too
	sc := func(b []byte) int64 {
		for i := 0; i+tsPacket <= len(b); i += tsPacket {
			p := b[i:]
			if p[3]&0x20 != 0 && p[4] >= 7 && p[5]&0x10 != 0 {
				c := p[6:12]
				return int64(c[0])<<25 | int64(c[1])<<17 | int64(c[2])<<9 | int64(c[3])<<1 | int64(c[4]>>7)
			}
		}
		return -1
	}
	d2 := append([]byte(nil), orig...)
	shiftTS(d2, 1000)
	if sc(d2)-sc(orig) != 1000 {
		t.Fatalf("PCR %d -> %d", sc(orig), sc(d2))
	}
}
