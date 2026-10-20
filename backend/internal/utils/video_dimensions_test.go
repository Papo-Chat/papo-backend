package utils

import (
	"bytes"
	"encoding/binary"
	"testing"
)

func testMP4Box(kind string, payload []byte) []byte {
	box := make([]byte, 8+len(payload))
	binary.BigEndian.PutUint32(box[:4], uint32(len(box)))
	copy(box[4:8], kind)
	copy(box[8:], payload)
	return box
}

func testMP4Track(width, height int, handler string) []byte {
	tkhd := make([]byte, 84) // version 0, including 16.16 width and height
	binary.BigEndian.PutUint32(tkhd[76:80], uint32(width)<<16)
	binary.BigEndian.PutUint32(tkhd[80:84], uint32(height)<<16)

	hdlr := make([]byte, 12)
	copy(hdlr[8:12], handler)
	mdia := testMP4Box("mdia", testMP4Box("hdlr", hdlr))
	trakPayload := append(testMP4Box("tkhd", tkhd), mdia...)
	return testMP4Box("trak", trakPayload)
}

func testMP4File(tracks ...[]byte) []byte {
	ftyp := testMP4Box("ftyp", []byte("isom0000isommp42"))
	var movie []byte
	for _, track := range tracks {
		movie = append(movie, track...)
	}
	return append(ftyp, testMP4Box("moov", movie)...)
}

func testEBMLElement(id []byte, payload []byte) []byte {
	if len(payload) >= 127 {
		panic("test EBML element too large for one-byte size")
	}
	elem := append([]byte{}, id...)
	elem = append(elem, 0x80|byte(len(payload)))
	return append(elem, payload...)
}

func testEBMLUnknownElement(id []byte, payload []byte) []byte {
	elem := append([]byte{}, id...)
	elem = append(elem, 0xff) // one-byte unknown size
	return append(elem, payload...)
}

func testEBMLUint(id byte, value uint64) []byte {
	size := 1
	for size < 8 && value>>(uint(size*8)) != 0 {
		size++
	}
	raw := make([]byte, 8)
	binary.BigEndian.PutUint64(raw, value)
	return testEBMLElement([]byte{id}, raw[8-size:])
}

func testWebMTrack(trackType, width, height uint64) []byte {
	track := testEBMLUint(0x83, trackType)
	if trackType == 1 {
		video := append(testEBMLUint(0xb0, width), testEBMLUint(0xba, height)...)
		track = append(track, testEBMLElement([]byte{0xe0}, video)...)
	}
	return testEBMLElement([]byte{0xae}, track)
}

func testWebMFile(tracks ...[]byte) []byte {
	header := testEBMLElement([]byte{0x1a, 0x45, 0xdf, 0xa3}, nil)
	var trackPayload []byte
	for _, track := range tracks {
		trackPayload = append(trackPayload, track...)
	}
	tracksElement := testEBMLElement([]byte{0x16, 0x54, 0xae, 0x6b}, trackPayload)
	segmentPayload := tracksElement
	segment := testEBMLUnknownElement([]byte{0x18, 0x53, 0x80, 0x67}, segmentPayload)
	return append(header, segment...)
}

func parseTestVideo(t *testing.T, data []byte) (int, int, bool) {
	t.Helper()
	return parseVideoDimensions(bytes.NewReader(data), int64(len(data)))
}

func TestParseMP4VideoDimensions(t *testing.T) {
	data := testMP4File(
		testMP4Track(0, 0, "soun"),
		testMP4Track(1920, 1080, "vide"),
	)
	width, height, ok := parseTestVideo(t, data)
	if !ok || width != 1920 || height != 1080 {
		t.Fatalf("got %dx%d ok=%v, want 1920x1080", width, height, ok)
	}
}

func TestParseWebMVideoDimensions(t *testing.T) {
	data := testWebMFile(
		testWebMTrack(2, 0, 0), // audio track must be ignored
		testWebMTrack(1, 1080, 1920),
	)
	width, height, ok := parseTestVideo(t, data)
	if !ok || width != 1080 || height != 1920 {
		t.Fatalf("got %dx%d ok=%v, want 1080x1920", width, height, ok)
	}
}

func TestVideoDimensionsRejectSuspiciousMetadata(t *testing.T) {
	tests := []struct {
		name string
		data []byte
	}{
		{
			name: "dimension above limit",
			data: testMP4File(testMP4Track(20000, 1080, "vide")),
		},
		{
			name: "pixel count above limit",
			data: testMP4File(testMP4Track(16384, 8192, "vide")),
		},
		{
			name: "extreme aspect ratio",
			data: testWebMFile(testWebMTrack(1, 16000, 100)),
		},
		{
			name: "oversized EBML integer",
			data: testWebMFile(testWebMTrack(1, ^uint64(0), 1080)),
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if width, height, ok := parseTestVideo(t, tt.data); ok {
				t.Fatalf("expected suspicious dimensions to be null, got %dx%d", width, height)
			}
		})
	}
}

func TestVideoDimensionsRejectMalformedContainers(t *testing.T) {
	// ftyp declares a box longer than the entire upload.
	badMP4 := []byte{0, 0, 0, 0x40, 'f', 't', 'y', 'p', 'i', 's', 'o', 'm'}
	if width, height, ok := parseTestVideo(t, badMP4); ok {
		t.Fatalf("expected malformed MP4 to be rejected, got %dx%d", width, height)
	}

	// An unknown-sized EBML header is not accepted; only Segment may use one.
	badWebM := []byte{0x1a, 0x45, 0xdf, 0xa3, 0xff, 0x18, 0x53, 0x80, 0x67, 0x80}
	if width, height, ok := parseTestVideo(t, badWebM); ok {
		t.Fatalf("expected malformed WebM to be rejected, got %dx%d", width, height)
	}
}
