package process

import (
	"encoding/binary"
	"io"
)

// orientationReader observes metadata without changing the source's bytes or
// read results. All parsing state is fixed-size, even for untrusted chunk sizes.
type orientationReader struct {
	src                io.Reader
	prefix             [12]byte
	prefixN            int
	format             byte
	done               bool
	jpegDone           bool
	orientation        int
	orientationFound   bool
	pendingOrientation int
	state              uint8
	header             [8]byte
	headerN            int
	remaining          uint64
	containerRemaining uint64
	padding            uint64
	marker             byte
	metadata           bool
	meta               orientationMetadata
}

const (
	metadataJPEG byte = iota + 1
	metadataPNG
	metadataWebP
)

func newOrientationReader(src io.Reader) *orientationReader {
	return &orientationReader{src: src, orientation: 1}
}

func (r *orientationReader) Orientation() int     { return r.orientation }
func (r *orientationReader) JPEGHeaderDone() bool { return r.jpegDone }

func (r *orientationReader) Read(p []byte) (int, error) {
	n, err := r.src.Read(p)
	data := p[:n]
	for len(data) != 0 && !r.done {
		if skipped := r.skipPayload(len(data)); skipped != 0 {
			data = data[skipped:]
			continue
		}
		r.consume(data[0])
		data = data[1:]
	}
	if err != nil {
		r.jpegDone = true
		r.done = true
	}
	return n, err
}

// Skip image data and unrelated metadata in whole slices. A PNG IDAT or WebP
// pixel chunk must not incur a parser call for every compressed byte.
func (r *orientationReader) skipPayload(available int) int {
	payload := r.format == metadataJPEG && r.state == 4 ||
		(r.format == metadataPNG || r.format == metadataWebP) && r.state == 1
	if !payload || r.remaining == 0 ||
		(r.metadata && r.pendingOrientation == 0 && !r.meta.tiff.done) {
		return 0
	}
	n := uint64(available)
	if n > r.remaining {
		n = r.remaining
	}
	r.remaining -= n
	if r.format == metadataWebP {
		r.containerRemaining -= n
	}
	if r.remaining == 0 {
		switch r.format {
		case metadataJPEG:
			r.jpegSegmentDone()
		case metadataPNG:
			r.metadataDone()
			r.state, r.remaining = 2, 4
		case metadataWebP:
			r.metadataDone()
			r.state = 0
			if r.padding != 0 {
				r.state = 2
			}
		}
	}
	return int(n)
}

func (r *orientationReader) consume(b byte) {
	if r.done {
		return
	}
	if r.format == 0 {
		r.prefix[r.prefixN] = b
		r.prefixN++
		if r.prefixN == 2 && r.prefix[0] == 0xff && r.prefix[1] == 0xd8 {
			r.format = metadataJPEG
			return
		}
		if r.prefixN == 8 && string(r.prefix[:8]) == "\x89PNG\r\n\x1a\n" {
			r.format = metadataPNG
			return
		}
		if r.prefixN != len(r.prefix) {
			return
		}
		switch {
		case string(r.prefix[:4]) == "RIFF" && string(r.prefix[8:]) == "WEBP":
			r.format = metadataWebP
			length := binary.LittleEndian.Uint32(r.prefix[4:8])
			if length < 4 {
				r.stop()
				return
			}
			r.containerRemaining = uint64(length) - 4
		default:
			r.stop()
		}
		return
	}
	switch r.format {
	case metadataJPEG:
		r.jpegByte(b)
	case metadataPNG:
		r.pngByte(b)
	case metadataWebP:
		r.webpByte(b)
	}
}

func (r *orientationReader) stop() {
	r.done = true
	r.jpegDone = true
}

func (r *orientationReader) metadataByte(b byte) {
	if r.metadata && r.pendingOrientation == 0 {
		r.pendingOrientation = r.meta.feed(b)
	}
}

func (r *orientationReader) metadataDone() {
	if r.metadata && r.pendingOrientation != 0 && !r.orientationFound {
		r.orientation = r.pendingOrientation
		r.orientationFound = true
	}
	r.pendingOrientation = 0
}

func (r *orientationReader) jpegByte(b byte) {
	switch r.state {
	case 0: // Marker introducer.
		if b != 0xff {
			r.stop()
			return
		}
		r.state = 1
	case 1:
		if b == 0xff {
			return
		}
		if b == 0 || b == 0xd8 {
			r.stop()
			return
		}
		if b == 0xd9 {
			r.stop()
			return
		}
		if b == 0x01 || b >= 0xd0 && b <= 0xd7 {
			r.state = 0
			return
		}
		r.marker = b
		r.state = 2
	case 2:
		r.remaining = uint64(b) << 8
		r.state = 3
	case 3:
		r.remaining |= uint64(b)
		if r.remaining < 2 {
			r.stop()
			return
		}
		r.remaining -= 2
		r.metadata = r.marker == 0xe1 && !r.orientationFound
		if r.metadata {
			r.meta.init(r.remaining, metadataJPEG)
		}
		r.state = 4
		if r.remaining == 0 {
			r.jpegSegmentDone()
		}
	case 4:
		r.metadataByte(b)
		r.remaining--
		if r.remaining == 0 {
			r.jpegSegmentDone()
		}
	}
}

func (r *orientationReader) jpegSegmentDone() {
	r.metadataDone()
	if r.marker == 0xda {
		r.stop() // Entropy-coded scan bytes are not marker headers.
		return
	}
	r.state = 0
}

func (r *orientationReader) pngByte(b byte) {
	switch r.state {
	case 0:
		r.header[r.headerN] = b
		r.headerN++
		if r.headerN != 8 {
			return
		}
		r.headerN = 0
		r.remaining = uint64(binary.BigEndian.Uint32(r.header[:4]))
		r.metadata = string(r.header[4:]) == "eXIf" && !r.orientationFound
		if r.metadata {
			r.meta.init(r.remaining, metadataPNG)
		}
		r.state = 1
		if r.remaining == 0 {
			r.state = 2
			r.remaining = 4
		}
	case 1:
		r.metadataByte(b)
		r.remaining--
		if r.remaining == 0 {
			r.metadataDone()
			r.state = 2
			r.remaining = 4 // CRC belongs to the container, not TIFF.
		}
	case 2:
		r.remaining--
		if r.remaining == 0 {
			if string(r.header[4:]) == "IEND" {
				r.stop()
			} else {
				r.state = 0
			}
		}
	}
}

func (r *orientationReader) webpByte(b byte) {
	if r.containerRemaining == 0 {
		r.stop()
		return
	}
	r.containerRemaining--
	switch r.state {
	case 0:
		r.header[r.headerN] = b
		r.headerN++
		if r.headerN != 8 {
			return
		}
		r.headerN = 0
		r.remaining = uint64(binary.LittleEndian.Uint32(r.header[4:]))
		r.padding = r.remaining & 1
		if r.remaining+r.padding > r.containerRemaining {
			r.stop()
			return
		}
		r.metadata = string(r.header[:4]) == "EXIF" && !r.orientationFound
		if r.metadata {
			r.meta.init(r.remaining, metadataWebP)
		}
		r.state = 1
		if r.remaining == 0 {
			r.state = 0
		}
	case 1:
		r.metadataByte(b)
		r.remaining--
		if r.remaining == 0 {
			r.metadataDone()
			if r.padding != 0 {
				r.state = 2
			} else {
				r.state = 0
			}
		}
	case 2:
		r.state = 0
	}
}

// orientationMetadata strips the container-specific optional Exif signature.
type orientationMetadata struct {
	prefix [6]byte
	n      int
	kind   byte
	length uint64
	tiff   orientationTIFF
}

func (m *orientationMetadata) init(length uint64, kind byte) {
	*m = orientationMetadata{length: length, kind: kind}
	if kind == metadataPNG {
		m.tiff.init(length)
	}
}

func (m *orientationMetadata) feed(b byte) int {
	if m.kind == metadataPNG {
		return m.tiff.feed(b)
	}
	if m.n < 6 {
		m.prefix[m.n] = b
		m.n++
		if m.n != 6 {
			return 0
		}
		if string(m.prefix[:]) == "Exif\x00\x00" {
			m.tiff.init(m.length - 6)
		} else if m.kind == metadataWebP {
			m.tiff.init(m.length)
			for _, v := range m.prefix {
				m.tiff.feed(v)
			}
		} else {
			m.tiff.done = true
		}
		return 0
	}
	return m.tiff.feed(b)
}

// orientationTIFF walks only the first IFD in forward order. Offsets and table
// sizes are checked against the enclosing payload, never followed recursively.
type orientationTIFF struct {
	scratch  [12]byte
	n        int
	state    uint8
	little   bool
	length   uint64
	position uint64
	offset   uint64
	entries  uint64
	done     bool
}

func (t *orientationTIFF) init(length uint64) {
	*t = orientationTIFF{length: length, done: length < 8}
}

func (t *orientationTIFF) uint16(p []byte) uint16 {
	if t.little {
		return binary.LittleEndian.Uint16(p)
	}
	return binary.BigEndian.Uint16(p)
}

func (t *orientationTIFF) uint32(p []byte) uint32 {
	if t.little {
		return binary.LittleEndian.Uint32(p)
	}
	return binary.BigEndian.Uint32(p)
}

func (t *orientationTIFF) feed(b byte) int {
	if t.done || t.position >= t.length {
		return 0
	}
	position := t.position
	t.position++
	if t.state == 1 && position < t.offset {
		return 0
	}
	t.scratch[t.n] = b
	t.n++
	switch t.state {
	case 0:
		if t.n != 8 {
			return 0
		}
		t.little = string(t.scratch[:2]) == "II"
		if !t.little && string(t.scratch[:2]) != "MM" || t.uint16(t.scratch[2:4]) != 42 {
			t.done = true
			return 0
		}
		t.offset = uint64(t.uint32(t.scratch[4:8]))
		if t.offset < 8 || t.offset > t.length-2 {
			t.done = true
			return 0
		}
		t.n = 0
		t.state = 1
	case 1:
		if t.n != 2 {
			return 0
		}
		t.entries = uint64(t.uint16(t.scratch[:2]))
		if t.length-t.position < 4 || t.entries > (t.length-t.position-4)/12 {
			t.done = true
			return 0
		}
		t.n = 0
		t.state = 2
		t.done = t.entries == 0
	case 2:
		if t.n != 12 {
			return 0
		}
		t.n = 0
		t.entries--
		t.done = t.entries == 0
		if t.uint16(t.scratch[:2]) == 0x0112 && t.uint16(t.scratch[2:4]) == 3 && t.uint32(t.scratch[4:8]) == 1 {
			value := int(t.uint16(t.scratch[8:10]))
			if value >= 1 && value <= 8 {
				t.done = true
				return value
			}
		}
	}
	return 0
}
