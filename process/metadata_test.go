package process

import (
	"bytes"
	"encoding/binary"
	"errors"
	"fmt"
	"io"
	"testing"
)

func metadataTestTIFF(order binary.ByteOrder, values ...uint16) []byte {
	p := make([]byte, 8+2+12*len(values)+4)
	if order == binary.LittleEndian {
		copy(p, "II")
	} else {
		copy(p, "MM")
	}
	order.PutUint16(p[2:], 42)
	order.PutUint32(p[4:], 8)
	order.PutUint16(p[8:], uint16(len(values)))
	for i, value := range values {
		entry := p[10+12*i:]
		order.PutUint16(entry, 0x0112)
		order.PutUint16(entry[2:], 3)
		order.PutUint32(entry[4:], 1)
		order.PutUint16(entry[8:], value)
	}
	return p
}

func metadataTestJPEGSegment(marker byte, payload []byte) []byte {
	p := []byte{0xff, marker, 0, 0}
	binary.BigEndian.PutUint16(p[2:], uint16(len(payload)+2))
	return append(p, payload...)
}

func metadataTestJPEG(payloads ...[]byte) []byte {
	p := []byte{0xff, 0xd8}
	// EXIF need not precede the frame header used by DecodeConfig.
	p = append(p, metadataTestJPEGSegment(0xc0, []byte{8, 0, 1, 0, 1, 1, 1, 0x11, 0})...)
	for _, payload := range payloads {
		p = append(p, metadataTestJPEGSegment(0xe1, append([]byte("Exif\x00\x00"), payload...))...)
	}
	p = append(p, metadataTestJPEGSegment(0xda, []byte{1, 1, 0, 0, 63, 0})...)
	return append(p, 0x11, 0xff, 0x00, 0x22, 0xff, 0xd9)
}

func metadataTestPNGChunk(name string, payload []byte) []byte {
	p := make([]byte, 8)
	binary.BigEndian.PutUint32(p, uint32(len(payload)))
	copy(p[4:], name)
	p = append(p, payload...)
	return append(p, 0, 0, 0, 0) // CRCs are the image decoder's responsibility.
}

func metadataTestPNG(payloads ...[]byte) []byte {
	p := []byte("\x89PNG\r\n\x1a\n")
	p = append(p, metadataTestPNGChunk("IDAT", []byte{1, 2, 3})...)
	for _, payload := range payloads {
		p = append(p, metadataTestPNGChunk("eXIf", payload)...)
	}
	return append(p, metadataTestPNGChunk("IEND", nil)...)
}

func metadataTestWebPChunk(name string, payload []byte) []byte {
	p := make([]byte, 8)
	copy(p, name)
	binary.LittleEndian.PutUint32(p[4:], uint32(len(payload)))
	p = append(p, payload...)
	if len(payload)&1 != 0 {
		p = append(p, 0)
	}
	return p
}

func metadataTestWebP(payloads ...[]byte) []byte {
	p := []byte("RIFF\x00\x00\x00\x00WEBP")
	p = append(p, metadataTestWebPChunk("JUNK", []byte{1, 2, 3})...)
	for _, payload := range payloads {
		p = append(p, metadataTestWebPChunk("EXIF", payload)...)
	}
	binary.LittleEndian.PutUint32(p[4:], uint32(len(p)-8))
	return p
}

type metadataTestChunkReader struct {
	data     []byte
	chunk    int
	terminal error
}

func (r *metadataTestChunkReader) Read(p []byte) (int, error) {
	if len(p) == 0 {
		return 0, nil
	}
	if len(r.data) == 0 {
		return 0, r.terminal
	}
	if len(p) > r.chunk {
		p = p[:r.chunk]
	}
	n := copy(p, r.data)
	r.data = r.data[n:]
	if len(r.data) == 0 {
		return n, r.terminal
	}
	return n, nil
}

func metadataTestRead(t *testing.T, input []byte, chunk, want int) {
	t.Helper()
	r := newOrientationReader(&metadataTestChunkReader{data: input, chunk: chunk, terminal: io.EOF})
	got, err := io.ReadAll(r)
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(got, input) {
		t.Fatal("metadata reader changed source bytes")
	}
	if r.Orientation() != want {
		t.Fatalf("orientation = %d, want %d", r.Orientation(), want)
	}
	if !r.JPEGHeaderDone() {
		t.Fatal("JPEG header not done after EOF")
	}
}

func TestOrientationMetadataContainers(t *testing.T) {
	for _, order := range []binary.ByteOrder{binary.LittleEndian, binary.BigEndian} {
		for value := uint16(1); value <= 8; value++ {
			payload := metadataTestTIFF(order, value)
			cases := map[string][]byte{
				"JPEG":          metadataTestJPEG(payload),
				"PNG":           metadataTestPNG(payload),
				"WebP":          metadataTestWebP(payload),
				"WebP-prefixed": metadataTestWebP(append([]byte("Exif\x00\x00"), payload...)),
			}
			for name, input := range cases {
				for _, chunk := range []int{1, 3, 17, 4096} {
					t.Run(fmt.Sprintf("%T/%d/%s/%d", order, value, name, chunk), func(t *testing.T) {
						metadataTestRead(t, input, chunk, int(value))
					})
				}
			}
		}
	}
}

func TestOrientationMetadataFirstValid(t *testing.T) {
	for _, wrap := range []func(...[]byte) []byte{metadataTestJPEG, metadataTestPNG, metadataTestWebP} {
		for _, value := range []uint16{1, 6} {
			first := metadataTestTIFF(binary.LittleEndian, 0, 9, value, 8)
			second := metadataTestTIFF(binary.BigEndian, 3)
			metadataTestRead(t, wrap(first, second), 1, int(value))
		}
		// A malformed earlier payload does not hide a later valid one.
		metadataTestRead(t, wrap([]byte("not TIFF"), metadataTestTIFF(binary.BigEndian, 7)), 2, 7)
	}
}

func TestOrientationMetadataIFDEntries(t *testing.T) {
	for _, order := range []binary.ByteOrder{binary.LittleEndian, binary.BigEndian} {
		p := metadataTestTIFF(order, 8, 8, 8, 5)
		order.PutUint16(p[10:], 0x0100) // Unrelated tag.
		order.PutUint16(p[24:], 4)      // Wrong field type.
		order.PutUint32(p[38:], 2)      // Wrong value count.
		metadataTestRead(t, metadataTestPNG(p), 1, 5)
		// A forward first-IFD offset may skip arbitrary unrelated bytes.
		p = metadataTestTIFF(order, 4)
		p = append(append(append([]byte(nil), p[:8]...), make([]byte, 257)...), p[8:]...)
		order.PutUint32(p[4:], 265)
		metadataTestRead(t, metadataTestPNG(p), 3, 4)
	}
}

func TestOrientationMetadataMalformed(t *testing.T) {
	var payloads [][]byte
	payloads = append(payloads, nil, []byte("Exif\x00\x00"), []byte("garbage!"))
	for _, order := range []binary.ByteOrder{binary.LittleEndian, binary.BigEndian} {
		valid := metadataTestTIFF(order, 6)
		for n := range valid {
			payloads = append(payloads, append([]byte(nil), valid[:n]...))
		}
		for _, offset := range []uint32{0, 1, 7, 25, 0xfffffffe, 0xffffffff} {
			p := append([]byte(nil), valid...)
			order.PutUint32(p[4:], offset)
			payloads = append(payloads, p)
		}
		p := append([]byte(nil), valid...)
		order.PutUint16(p[8:], 0xffff)
		payloads = append(payloads, p)
		p = append([]byte(nil), valid...)
		order.PutUint16(p[2:], 43)
		payloads = append(payloads, p)
		payloads = append(payloads, metadataTestTIFF(order, 0, 9, 65535))
	}
	for i, payload := range payloads {
		for _, wrap := range []func(...[]byte) []byte{metadataTestJPEG, metadataTestPNG, metadataTestWebP} {
			t.Run(fmt.Sprintf("%d/%p", i, wrap), func(t *testing.T) {
				metadataTestRead(t, wrap(payload), 1, 1)
			})
		}
	}
	for _, input := range [][]byte{nil, []byte("unknown format with trailing bytes"), {0xff, 0xd8}, metadataTestJPEG(), metadataTestPNG(), metadataTestWebP()} {
		metadataTestRead(t, input, 1, 1)
	}
}

func TestOrientationMetadataHugeTruncatedChunks(t *testing.T) {
	png := append([]byte("\x89PNG\r\n\x1a\n"), 0xff, 0xff, 0xff, 0xff, 'e', 'X', 'I', 'f')
	webp := []byte("RIFF\xff\xff\xff\xffWEBPEXIF\xff\xff\xff\xff")
	jpeg := []byte{0xff, 0xd8, 0xff, 0xe1, 0xff, 0xff, 'E', 'x', 'i', 'f', 0, 0}
	for _, input := range [][]byte{png, webp, jpeg} {
		metadataTestRead(t, input, 1, 1)
	}
}

func TestOrientationMetadataReadErrorUnchanged(t *testing.T) {
	terminal := errors.New("source failure")
	input := metadataTestPNG(metadataTestTIFF(binary.LittleEndian, 6))
	r := newOrientationReader(&metadataTestChunkReader{data: input, chunk: len(input), terminal: terminal})
	p := make([]byte, len(input)+10)
	n, err := r.Read(p)
	if n != len(input) || err != terminal || !bytes.Equal(p[:n], input) {
		t.Fatalf("Read = (%d, %v), source result was (%d, %v)", n, err, len(input), terminal)
	}
	if r.Orientation() != 6 || !r.JPEGHeaderDone() {
		t.Fatal("bytes returned with an error were not parsed before returning")
	}
}

func TestOrientationMetadataJPEGHeaderBoundary(t *testing.T) {
	input := metadataTestJPEG(metadataTestTIFF(binary.LittleEndian, 8))
	headerEnd := len(input) - 6 // Scan bytes and EOI are outside the header.
	r := newOrientationReader(&metadataTestChunkReader{data: input, chunk: 1, terminal: io.EOF})
	var p [1]byte
	for i := range headerEnd {
		if r.JPEGHeaderDone() {
			t.Fatalf("JPEG header marked done before byte %d", i)
		}
		if _, err := r.Read(p[:]); err != nil {
			t.Fatal(err)
		}
	}
	if !r.JPEGHeaderDone() || r.Orientation() != 8 {
		t.Fatal("JPEG header not finished at end of SOS header")
	}
	// A fake APP1 in entropy-coded data must not override header metadata.
	trailer := metadataTestJPEGSegment(0xe1, append([]byte("Exif\x00\x00"), metadataTestTIFF(binary.LittleEndian, 3)...))
	input = append(metadataTestJPEG(), trailer...)
	metadataTestRead(t, input, 1, 1)
	for _, malformed := range [][]byte{
		{0xff, 0xd8, 0xff, 0xe1, 0, 1},
		{0xff, 0xd8, 0xff, 0xd9},
		{0xff, 0xd8, 0, 0, 0, 0, 0, 0, 0, 0, 0, 0},
	} {
		metadataTestRead(t, malformed, 1, 1)
	}
}

func TestOrientationMetadataSignaturesAndPadding(t *testing.T) {
	payload := metadataTestTIFF(binary.LittleEndian, 7)
	metadataTestRead(t, metadataTestWebP(append(append([]byte(nil), payload...), 0xaa)), 1, 7)
	metadataTestRead(t, metadataTestPNG(append([]byte("Exif\x00\x00"), payload...)), 1, 1)
	jpeg := append([]byte{0xff, 0xd8}, metadataTestJPEGSegment(0xe1, payload)...)
	jpeg = append(jpeg, 0xff, 0xd9)
	metadataTestRead(t, jpeg, 1, 1)
	// Bytes outside the declared RIFF container are never metadata.
	webp := append(metadataTestWebP(), metadataTestWebPChunk("EXIF", payload)...)
	metadataTestRead(t, webp, 1, 1)
}

func TestOrientationMetadataTruncatedPayloadCandidate(t *testing.T) {
	payload := metadataTestTIFF(binary.LittleEndian, 8)
	png := append([]byte("\x89PNG\r\n\x1a\n"), 0, 0, 1, 0, 'e', 'X', 'I', 'f')
	png = append(png, payload...)
	jpeg := []byte{0xff, 0xd8, 0xff, 0xe1, 1, 0}
	jpeg = append(jpeg, []byte("Exif\x00\x00")...)
	jpeg = append(jpeg, payload...)
	for _, input := range [][]byte{png, jpeg} {
		metadataTestRead(t, input, 1, 1)
	}
}
