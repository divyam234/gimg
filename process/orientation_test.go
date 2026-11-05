package process

import (
	"bytes"
	"context"
	"encoding/binary"
	"fmt"
	"hash/crc32"
	"image"
	"image/color"
	"image/draw"
	"image/png"
	"io"
	"testing"

	"github.com/gen2brain/jpegn"
	"github.com/gen2brain/vpx/webp"
)

var orientationOrders = [][]int{
	nil,
	{0, 1, 2, 3, 4, 5}, {2, 1, 0, 5, 4, 3}, {5, 4, 3, 2, 1, 0}, {3, 4, 5, 0, 1, 2},
	{0, 3, 1, 4, 2, 5}, {3, 0, 4, 1, 5, 2}, {5, 2, 4, 1, 3, 0}, {2, 5, 1, 4, 0, 3},
}

func transformTIFF(orientation int) []byte {
	b := make([]byte, 26)
	copy(b, "II")
	binary.LittleEndian.PutUint16(b[2:], 42)
	binary.LittleEndian.PutUint32(b[4:], 8)
	binary.LittleEndian.PutUint16(b[8:], 1)
	binary.LittleEndian.PutUint16(b[10:], 0x112)
	binary.LittleEndian.PutUint16(b[12:], 3)
	binary.LittleEndian.PutUint32(b[14:], 1)
	binary.LittleEndian.PutUint16(b[18:], uint16(orientation))
	return b
}

func transformPNGChunk(kind string, data []byte) []byte {
	b := make([]byte, len(data)+12)
	binary.BigEndian.PutUint32(b, uint32(len(data)))
	copy(b[4:], kind)
	copy(b[8:], data)
	binary.BigEndian.PutUint32(b[len(b)-4:], crc32.ChecksumIEEE(b[4:len(b)-4]))
	return b
}

func transformAddEXIF(t *testing.T, data []byte, format Format, orientation int) []byte {
	t.Helper()
	tiff := transformTIFF(orientation)
	switch format {
	case PNG:
		// Put eXIf after all IDAT chunks, immediately before IEND.
		out := append([]byte(nil), data[:len(data)-12]...)
		out = append(out, transformPNGChunk("eXIf", tiff)...)
		return append(out, data[len(data)-12:]...)
	case JPEG:
		payload := append([]byte("Exif\x00\x00"), tiff...)
		segment := []byte{0xff, 0xe1, byte((len(payload) + 2) >> 8), byte(len(payload) + 2)}
		segment = append(segment, payload...)
		// Deliberately place APP1 after SOF: config reads need not see it.
		offset := 2
		for offset+4 <= len(data) {
			marker := data[offset+1]
			size := int(binary.BigEndian.Uint16(data[offset+2:]))
			if size < 2 || offset+size+2 > len(data) {
				t.Fatal("invalid JPEG fixture")
			}
			offset += size + 2
			if marker == 0xc0 || marker == 0xc2 {
				out := append([]byte(nil), data[:offset]...)
				out = append(out, segment...)
				return append(out, data[offset:]...)
			}
		}
		t.Fatal("JPEG fixture lacks SOF")
	case WebP:
		// VP8X announces metadata for a simple lossless WebP container.
		cfg, err := webp.DecodeConfig(bytes.NewReader(data))
		if err != nil {
			t.Fatal(err)
		}
		payload := make([]byte, 10)
		payload[0] = 8 // EXIF flag
		for i := range 3 {
			payload[4+i] = byte((cfg.Width - 1) >> (8 * i))
			payload[7+i] = byte((cfg.Height - 1) >> (8 * i))
		}
		out := append([]byte(nil), data[:12]...)
		out = append(out, []byte{'V', 'P', '8', 'X', 10, 0, 0, 0}...)
		out = append(out, payload...)
		out = append(out, data[12:]...)
		out = append(out, []byte{'E', 'X', 'I', 'F', byte(len(tiff)), 0, 0, 0}...)
		out = append(out, tiff...)
		binary.LittleEndian.PutUint32(out[4:], uint32(len(out)-8))
		return out
	}
	t.Fatal("unknown format")
	return nil
}

func transformEncode(t *testing.T, img image.Image, format Format) []byte {
	t.Helper()
	var b bytes.Buffer
	var err error
	switch format {
	case PNG:
		err = png.Encode(&b, img)
	case JPEG:
		err = jpegn.Encode(&b, img, &jpegn.EncodeOptions{Quality: 95, Subsampling: jpegn.Subsample444})
	case WebP:
		err = webp.Encode(&b, img, webp.EncodeOptions{Lossless: true, Threads: 1})
	}
	if err != nil {
		t.Fatal(err)
	}
	return b.Bytes()
}

func transformDecode(t *testing.T, data []byte, format Format) image.Image {
	t.Helper()
	var img image.Image
	var err error
	switch format {
	case PNG:
		img, err = png.Decode(bytes.NewReader(data))
	case JPEG:
		img, err = jpegn.Decode(bytes.NewReader(data), &jpegn.Options{ToRGBA: true})
	case WebP:
		img, err = webp.Decode(bytes.NewReader(data), webp.Options{ToRGBA: true, Threads: 1})
	}
	if err != nil {
		t.Fatal(err)
	}
	return img
}

func transformColorGrid() *image.NRGBA {
	img := image.NewNRGBA(image.Rect(0, 0, 3, 2))
	colors := []color.NRGBA{{255, 0, 0, 255}, {0, 255, 0, 255}, {0, 0, 255, 255}, {200, 100, 20, 128}, {10, 80, 220, 255}, {170, 40, 90, 255}}
	for i, c := range colors {
		img.SetNRGBA(i%3, i/3, c)
	}
	return img
}

// Reference the encoded pipeline's premultiplied RGBA8 representation, including
// PNG's unpremultiplication round-trip. Comparing original straight-alpha
// samples directly would mistake alpha quantization for a permutation error.
// This reference does not invoke the processor or orientation implementation.
func transformReference(t *testing.T, decoded image.Image) image.Image {
	t.Helper()
	rgba := image.NewRGBA(decoded.Bounds())
	draw.Draw(rgba, rgba.Bounds(), decoded, decoded.Bounds().Min, draw.Src)
	return transformDecode(t, transformEncode(t, rgba, PNG), PNG)
}

func assertTransformOrder(t *testing.T, encoded []byte, baseline image.Image, order []int, size image.Point) {
	t.Helper()
	baseline = transformReference(t, baseline)
	got := transformDecode(t, encoded, PNG)
	if got.Bounds().Size() != size {
		t.Fatalf("dimensions=%v, want %v", got.Bounds().Size(), size)
	}
	for i, sourceIndex := range order {
		x, y := i%size.X, i/size.X
		want := color.RGBAModel.Convert(baseline.At(sourceIndex%3, sourceIndex/3))
		if c := color.RGBAModel.Convert(got.At(x, y)); c != want {
			t.Fatalf("pixel (%d,%d)=%v, want %v", x, y, c, want)
		}
	}
}

func TestEXIFAllOrientationsAcrossCodecs(t *testing.T) {
	p := processor(t, Config{})
	for _, format := range []Format{JPEG, PNG, WebP} {
		input := transformEncode(t, transformColorGrid(), format)
		baseline := transformDecode(t, input, format)
		for orientation := 1; orientation <= 8; orientation++ {
			t.Run(fmt.Sprintf("%s/%d", format, orientation), func(t *testing.T) {
				data := transformAddEXIF(t, input, format, orientation)
				var dst bytes.Buffer
				_, err := p.Transform(context.Background(), &dst, struct{ io.Reader }{bytes.NewReader(data)}, Options{Width: 3, Height: 3, Mode: Fit})
				if err != nil {
					t.Fatal(err)
				}
				size := image.Pt(3, 2)
				if orientation >= 5 {
					size = image.Pt(2, 3)
				}
				assertTransformOrder(t, dst.Bytes(), baseline, orientationOrders[orientation], size)
			})
		}
	}
}

func TestExplicitTransformOrderAndGeometry(t *testing.T) {
	p := processor(t, Config{})
	input := transformEncode(t, transformColorGrid(), PNG)
	baseline := transformDecode(t, input, PNG)
	for _, tc := range []struct {
		name  string
		exif  int
		opts  Options
		order []int
		size  image.Point
	}{
		{"rotate90", 1, Options{Rotate: 90}, orientationOrders[6], image.Pt(2, 3)},
		{"rotate180", 1, Options{Rotate: 180}, orientationOrders[3], image.Pt(3, 2)},
		{"negative90", 1, Options{Rotate: -90}, orientationOrders[8], image.Pt(2, 3)},
		{"horizontal", 1, Options{FlipHorizontal: true}, orientationOrders[2], image.Pt(3, 2)},
		{"vertical", 1, Options{FlipVertical: true}, orientationOrders[4], image.Pt(3, 2)},
		{"rotate-then-horizontal", 1, Options{Rotate: 90, FlipHorizontal: true}, orientationOrders[5], image.Pt(2, 3)},
		{"exif-then-explicit", 6, Options{Rotate: 90, FlipHorizontal: true}, orientationOrders[4], image.Pt(3, 2)},
		{"composition-cancels", 6, Options{Rotate: 90, FlipHorizontal: true, FlipVertical: true}, orientationOrders[1], image.Pt(3, 2)},
		{"exif-disabled-explicit-kept", 6, Options{DisableAutoOrient: true, FlipVertical: true}, orientationOrders[4], image.Pt(3, 2)},
	} {
		t.Run(tc.name, func(t *testing.T) {
			var dst bytes.Buffer
			tc.opts.Width, tc.opts.Height = tc.size.X, tc.size.Y
			// Exact no-upscale checks must use the final oriented dimensions.
			_, err := p.Transform(context.Background(), &dst, bytes.NewReader(transformAddEXIF(t, input, PNG, tc.exif)), tc.opts)
			if err != nil {
				t.Fatal(err)
			}
			assertTransformOrder(t, dst.Bytes(), baseline, tc.order, tc.size)
		})
	}
	var cropped bytes.Buffer
	_, err := p.Transform(context.Background(), &cropped, bytes.NewReader(input), Options{Rotate: 90, Width: 2, Height: 2, Mode: Fill, Anchor: Bottom})
	if err != nil {
		t.Fatal(err)
	}
	assertTransformOrder(t, cropped.Bytes(), baseline, []int{4, 1, 5, 2}, image.Pt(2, 2))
	if _, err := p.Transform(context.Background(), io.Discard, bytes.NewReader(input), Options{Width: 2, Height: 3, Rotate: 45}); err == nil {
		t.Fatal("nonorthogonal rotation accepted")
	}
}

func TestEXIFJPEGScaledDecodeUsesOrientedDimensions(t *testing.T) {
	p := processor(t, Config{})
	img := image.NewRGBA(image.Rect(0, 0, 48, 32))
	for y := range 32 {
		for x := range 48 {
			img.SetRGBA(x, y, color.RGBA{uint8(x * 5), uint8(y * 7), uint8(x + y), 255})
		}
	}
	input := transformEncode(t, img, JPEG)
	raw, err := jpegn.Decode(bytes.NewReader(input), &jpegn.Options{ToRGBA: true, ScaleDenom: 2})
	if err != nil {
		t.Fatal(err)
	}
	var dst bytes.Buffer
	r, err := p.Transform(context.Background(), &dst, bytes.NewReader(transformAddEXIF(t, input, JPEG, 6)), Options{Width: 16, Height: 24, Mode: Fit, JPEGShrink: true})
	if err != nil {
		t.Fatal(err)
	}
	if r.Width != 16 || r.Height != 24 {
		t.Fatalf("scaled dimensions: %+v", r)
	}
	got := transformDecode(t, dst.Bytes(), PNG)
	for y := range 24 {
		for x := range 16 {
			want := color.RGBAModel.Convert(raw.At(y, 15-x))
			if c := color.RGBAModel.Convert(got.At(x, y)); c != want {
				t.Fatalf("scaled pixel (%d,%d)=%v, want %v", x, y, c, want)
			}
		}
	}
}

func TestCachedOrientationOptions(t *testing.T) {
	p := cachedProcessor(t)
	input := transformEncode(t, transformColorGrid(), PNG)
	baseline := transformDecode(t, input, PNG)
	oriented := transformAddEXIF(t, input, PNG, 6)
	opens := 0
	src := Source{Key: "oriented", Version: "1", Open: func(context.Context) (io.ReadCloser, error) {
		opens++
		return io.NopCloser(bytes.NewReader(oriented)), nil
	}}
	for _, tc := range []struct {
		opts  Options
		hit   bool
		order []int
		size  image.Point
	}{
		{Options{}, false, orientationOrders[6], image.Pt(2, 3)},
		{Options{}, true, orientationOrders[6], image.Pt(2, 3)},
		{Options{DisableAutoOrient: true}, false, orientationOrders[1], image.Pt(3, 2)},
		{Options{Rotate: 90}, false, orientationOrders[3], image.Pt(3, 2)},
		{Options{Rotate: 450}, true, orientationOrders[3], image.Pt(3, 2)},
		{Options{FlipHorizontal: true}, false, orientationOrders[5], image.Pt(2, 3)},
		{Options{FlipVertical: true}, false, orientationOrders[7], image.Pt(2, 3)},
	} {
		tc.opts.Width, tc.opts.Height, tc.opts.Mode = 3, 3, Fit
		var dst bytes.Buffer
		r, err := p.TransformCached(context.Background(), &dst, src, tc.opts)
		if err != nil {
			t.Fatal(err)
		}
		if r.CacheHit != tc.hit {
			t.Fatalf("options %+v: hit=%v, want %v", tc.opts, r.CacheHit, tc.hit)
		}
		assertTransformOrder(t, dst.Bytes(), baseline, tc.order, tc.size)
	}
	if opens != 5 {
		t.Fatalf("source opens=%d, want5 distinct transformations", opens)
	}
}
