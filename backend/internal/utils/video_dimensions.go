package utils

import (
	"encoding/binary"
	"io"
	"os"
)

const (
	maxVideoFileSize  = 100 << 20
	maxVideoDimension = 16384
	maxVideoPixels    = uint64(100_000_000)
	maxVideoElements  = 100_000
)

// VideoDimensionsFromFile reads container metadata only; it never decodes
// frames and does not invoke an external media process. Invalid, unsupported,
// or suspicious dimensions are reported as unavailable so uploads can proceed.
func VideoDimensionsFromFile(path string) (width, height int, ok bool) {
	file, err := os.Open(path)
	if err != nil {
		return 0, 0, false
	}
	defer file.Close()

	info, err := file.Stat()
	if err != nil || info.Size() < 8 || info.Size() > maxVideoFileSize {
		return 0, 0, false
	}
	return parseVideoDimensions(file, info.Size())
}

func parseVideoDimensions(r io.ReaderAt, size int64) (width, height int, ok bool) {
	if size < 8 || size > maxVideoFileSize {
		return 0, 0, false
	}

	var signature [8]byte
	if !readAtExact(r, signature[:], 0) {
		return 0, 0, false
	}

	// WebM files start with the EBML header ID. ISO-BMFF video (MP4/MOV)
	// starts with an ftyp box. Container structure is verified while parsing.
	if signature[0] == 0x1a && signature[1] == 0x45 && signature[2] == 0xdf && signature[3] == 0xa3 {
		width, height, ok = parseWebMDimensions(r, size)
	} else if string(signature[4:8]) == "ftyp" {
		width, height, ok = parseMP4Dimensions(r, size)
	} else {
		return 0, 0, false
	}
	if !ok {
		return 0, 0, false
	}
	return validateVideoDimensions(uint64(width), uint64(height))
}

func validateVideoDimensions(width, height uint64) (int, int, bool) {
	if width == 0 || height == 0 || width > maxVideoDimension || height > maxVideoDimension {
		return 0, 0, false
	}
	if width*height > maxVideoPixels {
		return 0, 0, false
	}
	// Reject extreme aspect ratios commonly caused by corrupt or forged
	// track metadata. A 10:1 range still allows panoramas and portrait video.
	if width > height*10 || height > width*10 {
		return 0, 0, false
	}
	return int(width), int(height), true
}

func readAtExact(r io.ReaderAt, dst []byte, offset int64) bool {
	n, err := r.ReadAt(dst, offset)
	return n == len(dst) && (err == nil || err == io.EOF)
}

// ---- ISO Base Media File Format (MP4/MOV) ----

type mp4Box struct {
	kind       string
	payload    int64
	end        int64
	payloadLen int64
}

func readMP4Box(r io.ReaderAt, offset, parentEnd int64, budget *int) (mp4Box, bool) {
	*budget++
	if *budget > maxVideoElements || offset < 0 || parentEnd < offset || parentEnd-offset < 8 {
		return mp4Box{}, false
	}
	var header [16]byte
	if !readAtExact(r, header[:8], offset) {
		return mp4Box{}, false
	}

	size := uint64(binary.BigEndian.Uint32(header[:4]))
	kind := string(header[4:8])
	headerLen := uint64(8)
	switch size {
	case 0:
		size = uint64(parentEnd - offset)
	case 1:
		if parentEnd-offset < 16 || !readAtExact(r, header[8:16], offset+8) {
			return mp4Box{}, false
		}
		size = binary.BigEndian.Uint64(header[8:16])
		headerLen = 16
	}
	if size < headerLen || size > uint64(parentEnd-offset) {
		return mp4Box{}, false
	}

	end := offset + int64(size) // size was bounded by parentEnd-offset
	payload := offset + int64(headerLen)
	return mp4Box{
		kind:       kind,
		payload:    payload,
		end:        end,
		payloadLen: end - payload,
	}, true
}

func parseMP4Dimensions(r io.ReaderAt, fileSize int64) (int, int, bool) {
	offset := int64(0)
	budget := 0
	for offset < fileSize {
		box, ok := readMP4Box(r, offset, fileSize, &budget)
		if !ok {
			return 0, 0, false
		}
		if box.kind == "moov" {
			if width, height, found := parseMP4Movie(r, box.payload, box.end, &budget); found {
				return width, height, true
			}
			return 0, 0, false
		}
		if box.end <= offset {
			return 0, 0, false
		}
		offset = box.end
	}
	return 0, 0, false
}

func parseMP4Movie(r io.ReaderAt, start, end int64, budget *int) (int, int, bool) {
	for offset := start; offset < end; {
		box, ok := readMP4Box(r, offset, end, budget)
		if !ok {
			return 0, 0, false
		}
		if box.kind == "trak" {
			width, height, isVideo, trackOK := parseMP4Track(r, box.payload, box.end, budget)
			if trackOK && isVideo {
				if w, h, valid := validateVideoDimensions(uint64(width), uint64(height)); valid {
					return w, h, true
				}
			}
		}
		if box.end <= offset {
			return 0, 0, false
		}
		offset = box.end
	}
	return 0, 0, false
}

func parseMP4Track(r io.ReaderAt, start, end int64, budget *int) (int, int, bool, bool) {
	var width, height uint32
	var hasHeader, hasMedia bool
	handler := ""

	for offset := start; offset < end; {
		box, ok := readMP4Box(r, offset, end, budget)
		if !ok {
			return 0, 0, false, false
		}
		switch box.kind {
		case "tkhd":
			if hasHeader {
				return 0, 0, false, false
			}
			width, height, ok = parseMP4TrackHeader(r, box)
			if !ok {
				return 0, 0, false, false
			}
			hasHeader = true
		case "mdia":
			if hasMedia {
				return 0, 0, false, false
			}
			handler, ok = parseMP4MediaHandler(r, box.payload, box.end, budget)
			if !ok {
				return 0, 0, false, false
			}
			hasMedia = true
		}
		if box.end <= offset {
			return 0, 0, false, false
		}
		offset = box.end
	}

	if !hasHeader || !hasMedia {
		return 0, 0, false, false
	}
	return int(width), int(height), handler == "vide", true
}

func parseMP4TrackHeader(r io.ReaderAt, box mp4Box) (uint32, uint32, bool) {
	var version [1]byte
	if box.payloadLen < 1 || !readAtExact(r, version[:], box.payload) {
		return 0, 0, false
	}

	var offset int64
	switch version[0] {
	case 0:
		offset = 76
	case 1:
		offset = 88
	default:
		return 0, 0, false
	}
	if box.payloadLen < offset+8 {
		return 0, 0, false
	}

	var dimensions [8]byte
	if !readAtExact(r, dimensions[:], box.payload+offset) {
		return 0, 0, false
	}
	rawWidth := binary.BigEndian.Uint32(dimensions[:4])
	rawHeight := binary.BigEndian.Uint32(dimensions[4:])
	// tkhd stores 16.16 fixed-point display dimensions. Non-integral values
	// are unnecessary for pixel geometry and are treated as untrusted.
	if rawWidth&0xffff != 0 || rawHeight&0xffff != 0 {
		return 0, 0, false
	}
	return rawWidth >> 16, rawHeight >> 16, true
}

func parseMP4MediaHandler(r io.ReaderAt, start, end int64, budget *int) (string, bool) {
	for offset := start; offset < end; {
		box, ok := readMP4Box(r, offset, end, budget)
		if !ok {
			return "", false
		}
		if box.kind == "hdlr" {
			if box.payloadLen < 12 {
				return "", false
			}
			var handler [12]byte
			if !readAtExact(r, handler[:], box.payload) {
				return "", false
			}
			return string(handler[8:12]), true
		}
		if box.end <= offset {
			return "", false
		}
		offset = box.end
	}
	return "", false
}

// ---- WebM / EBML ----

const (
	ebmlHeaderID = uint64(0x1a45dfa3)
	ebmlSegmentID = uint64(0x18538067)
	ebmlTracksID = uint64(0x1654ae6b)
	ebmlTrackEntryID = uint64(0xae)
	ebmlTrackTypeID = uint64(0x83)
	ebmlVideoID = uint64(0xe0)
	ebmlPixelWidthID = uint64(0xb0)
	ebmlPixelHeightID = uint64(0xba)
)

type ebmlElement struct {
	id      uint64
	payload int64
	end     int64
	size    uint64
	unknown bool
}

func readEBMLVINT(r io.ReaderAt, offset, parentEnd int64, isID bool) (uint64, int, bool) {
	if offset < 0 || parentEnd <= offset {
		return 0, 0, false
	}
	var first [1]byte
	if !readAtExact(r, first[:], offset) || first[0] == 0 {
		return 0, 0, false
	}
	mask := byte(0x80)
	length := 1
	for first[0]&mask == 0 {
		mask >>= 1
		length++
	}
	maxLength := 8
	if isID {
		maxLength = 4
	}
	if length > maxLength || int64(length) > parentEnd-offset {
		return 0, 0, false
	}

	value := uint64(first[0])
	if !isID {
		value &= uint64(mask - 1)
	}
	for i := 1; i < length; i++ {
		var next [1]byte
		if !readAtExact(r, next[:], offset+int64(i)) {
			return 0, 0, false
		}
		value = (value << 8) | uint64(next[0])
	}
	return value, length, true
}

func readEBMLElement(r io.ReaderAt, offset, parentEnd int64, budget *int) (ebmlElement, bool) {
	*budget++
	if *budget > maxVideoElements || offset < 0 || parentEnd < offset {
		return ebmlElement{}, false
	}
	id, idLen, ok := readEBMLVINT(r, offset, parentEnd, true)
	if !ok {
		return ebmlElement{}, false
	}
	sizeOffset := offset + int64(idLen)
	size, sizeLen, ok := readEBMLVINT(r, sizeOffset, parentEnd, false)
	if !ok {
		return ebmlElement{}, false
	}
	payload := sizeOffset + int64(sizeLen)
	if payload > parentEnd {
		return ebmlElement{}, false
	}

	unknownSize := size == (uint64(1)<<(7*sizeLen))-1
	if unknownSize {
		return ebmlElement{
			id: id, payload: payload, end: parentEnd, unknown: true,
		}, true
	}
	if size > uint64(parentEnd-payload) {
		return ebmlElement{}, false
	}
	return ebmlElement{
		id: id, payload: payload, end: payload + int64(size), size: size,
	}, true
}

func parseWebMDimensions(r io.ReaderAt, fileSize int64) (int, int, bool) {
	budget := 0
	for offset := int64(0); offset < fileSize; {
		elem, ok := readEBMLElement(r, offset, fileSize, &budget)
		if !ok {
			return 0, 0, false
		}
		if elem.unknown && elem.id != ebmlSegmentID {
			return 0, 0, false
		}
		if elem.id == ebmlSegmentID {
			return parseWebMSegment(r, elem.payload, elem.end, &budget)
		}
		if elem.end <= offset {
			return 0, 0, false
		}
		offset = elem.end
	}
	return 0, 0, false
}

func parseWebMSegment(r io.ReaderAt, start, end int64, budget *int) (int, int, bool) {
	for offset := start; offset < end; {
		elem, ok := readEBMLElement(r, offset, end, budget)
		if !ok || elem.unknown {
			return 0, 0, false
		}
		if elem.id == ebmlTracksID {
			return parseWebMTracks(r, elem.payload, elem.end, budget)
		}
		if elem.end <= offset {
			return 0, 0, false
		}
		offset = elem.end
	}
	return 0, 0, false
}

func parseWebMTracks(r io.ReaderAt, start, end int64, budget *int) (int, int, bool) {
	for offset := start; offset < end; {
		elem, ok := readEBMLElement(r, offset, end, budget)
		if !ok || elem.unknown {
			return 0, 0, false
		}
		if elem.id == ebmlTrackEntryID {
			trackType, width, height, trackOK := parseWebMTrack(r, elem.payload, elem.end, budget)
			if trackOK && trackType == 1 {
				if w, h, valid := validateVideoDimensions(width, height); valid {
					return w, h, true
				}
			}
		}
		if elem.end <= offset {
			return 0, 0, false
		}
		offset = elem.end
	}
	return 0, 0, false
}

func parseWebMTrack(r io.ReaderAt, start, end int64, budget *int) (uint64, uint64, uint64, bool) {
	var trackType, width, height uint64
	var hasType, hasVideo bool

	for offset := start; offset < end; {
		elem, ok := readEBMLElement(r, offset, end, budget)
		if !ok || elem.unknown {
			return 0, 0, 0, false
		}
		switch elem.id {
		case ebmlTrackTypeID:
			if hasType {
				return 0, 0, 0, false
			}
			trackType, ok = readEBMLUint(r, elem)
			if !ok {
				return 0, 0, 0, false
			}
			hasType = true
		case ebmlVideoID:
			if hasVideo {
				return 0, 0, 0, false
			}
			width, height, ok = parseWebMVideo(r, elem.payload, elem.end, budget)
			if !ok {
				return 0, 0, 0, false
			}
			hasVideo = true
		}
		if elem.end <= offset {
			return 0, 0, 0, false
		}
		offset = elem.end
	}
	return trackType, width, height, hasType && hasVideo
}

func parseWebMVideo(r io.ReaderAt, start, end int64, budget *int) (uint64, uint64, bool) {
	var width, height uint64
	var hasWidth, hasHeight bool

	for offset := start; offset < end; {
		elem, ok := readEBMLElement(r, offset, end, budget)
		if !ok || elem.unknown {
			return 0, 0, false
		}
		switch elem.id {
		case ebmlPixelWidthID:
			if hasWidth {
				return 0, 0, false
			}
			width, ok = readEBMLUint(r, elem)
			if !ok {
				return 0, 0, false
			}
			hasWidth = true
		case ebmlPixelHeightID:
			if hasHeight {
				return 0, 0, false
			}
			height, ok = readEBMLUint(r, elem)
			if !ok {
				return 0, 0, false
			}
			hasHeight = true
		}
		if elem.end <= offset {
			return 0, 0, false
		}
		offset = elem.end
	}
	return width, height, hasWidth && hasHeight
}

func readEBMLUint(r io.ReaderAt, elem ebmlElement) (uint64, bool) {
	if elem.size == 0 || elem.size > 8 {
		return 0, false
	}
	var data [8]byte
	value := data[:elem.size]
	if !readAtExact(r, value, elem.payload) {
		return 0, false
	}
	var out uint64
	for _, b := range value {
		out = (out << 8) | uint64(b)
	}
	return out, true
}
