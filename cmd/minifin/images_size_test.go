package main

import (
	"bytes"
	"encoding/binary"
	"hash/crc32"
	"image"
	"image/png"
	"testing"
)

// A header claiming 65536x65537 pixels wraps to a small number in 32-bit
// int (the device's): it must still be refused before decoding.
func TestDecodeRefusesHugePicture(t *testing.T) {
	var buf bytes.Buffer
	png.Encode(&buf, image.NewRGBA(image.Rect(0, 0, 1, 1)))
	b := buf.Bytes()
	ihdr := b[8+8 : 8+8+13] // after the signature and the chunk's length and type
	binary.BigEndian.PutUint32(ihdr[0:], 65536)
	binary.BigEndian.PutUint32(ihdr[4:], 65537)
	binary.BigEndian.PutUint32(b[8+8+13:], crc32.ChecksumIEEE(b[8+4:8+8+13]))
	if _, err := decodeImage(b, 100, 150); err == nil {
		t.Fatal("decoded a 65536x65537 picture")
	}
}
