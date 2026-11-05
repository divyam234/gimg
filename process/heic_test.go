package process

import (
	"bytes"
	"context"
	"encoding/binary"
	"errors"
	"image"
	"image/color"
	"image/png"
	"testing"

	nativeheic "github.com/gen2brain/h265/heic"
)

func heicTestImage(w, h int, alpha bool) *image.NRGBA {
	img := image.NewNRGBA(image.Rect(0, 0, w, h))
	for y := range h {
		for x := range w {
			a := uint8(255)
			if alpha {
				a = 64
				if x >= w/2 {
					a = 192
				}
			}
			img.SetNRGBA(x, y, color.NRGBA{R: uint8(40 + x*120/w), G: uint8(50 + y*130/h), B: 90, A: a})
		}
	}
	return img
}

func nativeHEICFixture(t *testing.T, img image.Image) []byte {
	t.Helper()
	var b bytes.Buffer
	if err := nativeheic.Encode(&b, img, nativeheic.EncodeOptions{Quality: 95}); err != nil {
		t.Fatal(err)
	}
	return b.Bytes()
}

func TestHEICHeader(t *testing.T) {
	ftyp := func(major string, compatible ...string) []byte {
		b := make([]byte, 16+4*len(compatible))
		binary.BigEndian.PutUint32(b, uint32(len(b)))
		copy(b[4:], "ftyp")
		copy(b[8:], major)
		for i, brand := range compatible {
			copy(b[16+i*4:], brand)
		}
		return b
	}
	for _, brand := range []string{"heic", "heix", "hevc", "hevx", "heim", "heis", "hevm", "hevs", "mif1", "msf1"} {
		if !isHEICHeader(ftyp(brand)) {
			t.Errorf("did not recognize %s", brand)
		}
	}
	for _, b := range [][]byte{
		nil, []byte("not HEIC"), ftyp("heic")[:12], ftyp("zzzz"),
		ftyp("avif"), ftyp("avis"), ftyp("mif1", "avif"), ftyp("msf1", "avis"),
	} {
		if isHEICHeader(b) {
			t.Errorf("recognized unsupported/truncated header %x", b)
		}
	}
	if !isHEICHeader(ftyp("zzzz", "mif1", "heic")) {
		t.Fatal("did not recognize compatible HEVC brand")
	}
	badSize := ftyp("heic")
	binary.BigEndian.PutUint32(badSize, 12)
	if isHEICHeader(badSize) {
		t.Fatal("recognized invalid ftyp size")
	}
	if !isHEICHeader(nativeHEICFixture(t, heicTestImage(16, 16, false))) {
		t.Fatal("did not recognize native encoded HEIC")
	}
}

func TestHEICRoundTripAlpha(t *testing.T) {
	src := heicTestImage(32, 24, true)
	var b bytes.Buffer
	if err := encodeHEIC(&b, src, 95); err != nil {
		t.Fatal(err)
	}
	decoded, err := nativeheic.Decode(bytes.NewReader(b.Bytes()))
	if err != nil {
		t.Fatal(err)
	}
	if decoded.Bounds() != src.Bounds() {
		t.Fatalf("bounds = %v, want %v", decoded.Bounds(), src.Bounds())
	}
	// Inspect unassociated colour as well as alpha: a codec that composites
	// against black before writing the auxiliary alpha would fail this check.
	for _, p := range []image.Point{{8, 8}, {24, 16}} {
		got := color.NRGBAModel.Convert(decoded.At(p.X, p.Y)).(color.NRGBA)
		want := src.NRGBAAt(p.X, p.Y)
		for i, pair := range [][2]uint8{{got.R, want.R}, {got.G, want.G}, {got.B, want.B}, {got.A, want.A}} {
			d := int(pair[0]) - int(pair[1])
			if d < -20 || d > 20 {
				t.Errorf("pixel %v channel %d = %d, want approximately %d", p, i, pair[0], pair[1])
			}
		}
	}
}

func TestHEICDecodeCleanApertureAndLimits(t *testing.T) {
	src := heicTestImage(31, 19, false)
	data := nativeHEICFixture(t, src)
	cfg, err := heicConfig(data)
	if err != nil {
		t.Fatal(err)
	}
	if cfg.Width < 31 || cfg.Height < 19 {
		t.Fatalf("config lost source dimensions: %+v", cfg)
	}
	// Native coding may pad beyond the even-sized container item as well as
	// the clean aperture. Use a generous coded budget for presentation checks;
	// the source-sized budget below separately exercises the allocation cap.
	for _, autoOrient := range []bool{false, true} {
		img, err := decodeHEIC(data, 1, 1<<16, autoOrient)
		if err != nil {
			t.Fatal(err)
		}
		if img.Bounds() != src.Bounds() {
			t.Fatalf("autoOrient=%v: bounds = %v, want %v", autoOrient, img.Bounds(), src.Bounds())
		}
	}
	// The presented pixels fit this budget, but the coded allocation does not.
	// The cap must reach the native decoder itself.
	if _, err := decodeHEIC(data, 1, 31*19, true); err == nil {
		t.Fatal("accepted coded image exceeding source pixel cap")
	}
	if _, err := decodeHEIC(data, 1, 0, true); err == nil {
		t.Fatal("accepted nonpositive source pixel cap")
	}
	if _, err := decodeHEIC([]byte("invalid HEIC"), 1, 1024, true); !errors.Is(err, nativeheic.ErrInvalid) {
		t.Fatalf("native error identity lost: %v", err)
	}
}

// heicTransformFixture replaces the last colour property of an opaque,
// even-sized native fixture with irot/imir plus an unassociated free box.
// Every enclosing size and media offset stays unchanged. No existing property
// index moves, because colr is the last property in this fixture.
func heicTransformFixture(t *testing.T, data []byte, typ string, value byte) []byte {
	t.Helper()
	out := bytes.Clone(data)
	children := func(start, end int, wanted string) (int, int) {
		for off := start; off+8 <= end; {
			n := int(binary.BigEndian.Uint32(out[off:]))
			if n < 8 || n > end-off {
				t.Fatalf("invalid native fixture box at %d", off)
			}
			if string(out[off+4:off+8]) == wanted {
				return off, off + n
			}
			off += n
		}
		t.Fatalf("native fixture missing %s", wanted)
		return 0, 0
	}
	meta, metaEnd := children(0, len(out), "meta")
	iprp, iprpEnd := children(meta+12, metaEnd, "iprp")
	ipco, ipcoEnd := children(iprp+8, iprpEnd, "ipco")
	colr, colrEnd := children(ipco+8, ipcoEnd, "colr")
	if colrEnd != ipcoEnd || colrEnd-colr < 17 {
		t.Fatal("native fixture colour property cannot be replaced without moving indices")
	}
	clear(out[colr:colrEnd])
	binary.BigEndian.PutUint32(out[colr:], 9)
	copy(out[colr+4:], typ)
	out[colr+8] = value
	binary.BigEndian.PutUint32(out[colr+9:], uint32(colrEnd-colr-9))
	copy(out[colr+13:], "free")
	return out
}

func TestHEICContainerPresentationOptOut(t *testing.T) {
	data := nativeHEICFixture(t, heicTestImage(32, 24, false))
	for _, typ := range []string{"irot", "imir"} {
		t.Run(typ, func(t *testing.T) {
			fixture := heicTransformFixture(t, data, typ, 1)
			raw, err := decodeHEIC(fixture, 1, 1<<16, false)
			if err != nil {
				t.Fatal(err)
			}
			presented, err := decodeHEIC(fixture, 2, 1<<16, true)
			if err != nil {
				t.Fatal(err)
			}
			wantBounds := image.Rect(0, 0, 32, 24)
			if raw.Bounds() != wantBounds {
				t.Fatalf("opt-out bounds = %v, want %v", raw.Bounds(), wantBounds)
			}
			if typ == "irot" {
				wantBounds = image.Rect(0, 0, 24, 32)
			}
			if presented.Bounds() != wantBounds {
				t.Fatalf("presented bounds = %v, want %v", presented.Bounds(), wantBounds)
			}
			for y := range 24 {
				for x := range 32 {
					dx, dy := 31-x, y
					if typ == "irot" {
						dx, dy = y, 31-x
					}
					got := color.NRGBAModel.Convert(presented.At(dx, dy))
					want := color.NRGBAModel.Convert(raw.At(x, y))
					if got != want {
						t.Fatalf("pixel (%d,%d): presented %v, raw %v", x, y, got, want)
					}
				}
			}
		})
	}
}

func TestHEICProcessorGeometry(t *testing.T) {
	p := processor(t, Config{})
	rotated := heicTransformFixture(t, nativeHEICFixture(t, heicTestImage(32, 24, false)), "irot", 1)
	odd := nativeHEICFixture(t, heicTestImage(31, 19, false))
	for _, tt := range []struct {
		name          string
		data          []byte
		width, height int
		disable       bool
		rotate        int
	}{
		{"container rotation", rotated, 12, 16, false, 0},
		{"container rotation disabled", rotated, 12, 9, true, 0},
		{"explicit rotation after presentation", rotated, 12, 9, false, 90},
		{"clean aperture", odd, 31, 19, false, 0},
	} {
		t.Run(tt.name, func(t *testing.T) {
			var out bytes.Buffer
			result, err := p.Transform(context.Background(), &out, bytes.NewReader(tt.data), Options{
				Width: tt.width, Mode: Fit, DisableAutoOrient: tt.disable, Rotate: tt.rotate,
				Encode: EncodeOptions{Format: PNG},
			})
			if err != nil {
				t.Fatal(err)
			}
			got, err := png.Decode(bytes.NewReader(out.Bytes()))
			if err != nil {
				t.Fatal(err)
			}
			want := image.Rect(0, 0, tt.width, tt.height)
			if got.Bounds() != want || result.Width != tt.width || result.Height != tt.height {
				t.Fatalf("decoded %v, result %+v, want %v", got.Bounds(), result, want)
			}
		})
	}
}

func TestHEICProcessorOutputPreservesAlpha(t *testing.T) {
	p := processor(t, Config{})
	src := heicTestImage(32, 24, true)
	var out bytes.Buffer
	result, err := p.Transform(context.Background(), &out, bytes.NewReader(pngBytes(t, src)), Options{
		Width: 16, Height: 12, Encode: EncodeOptions{Format: HEIC, Quality: 95},
	})
	if err != nil {
		t.Fatal(err)
	}
	img, err := nativeheic.Decode(bytes.NewReader(out.Bytes()))
	if err != nil {
		t.Fatal(err)
	}
	if img.Bounds() != image.Rect(0, 0, 16, 12) || result.Format != HEIC {
		t.Fatalf("decoded %v, result %+v", img.Bounds(), result)
	}
	for _, sample := range []struct{ x, alpha int }{{3, 64}, {12, 192}} {
		got := color.NRGBAModel.Convert(img.At(sample.x, 6)).(color.NRGBA)
		if d := int(got.A) - sample.alpha; d < -20 || d > 20 {
			t.Errorf("alpha at x=%d = %d, want approximately %d", sample.x, got.A, sample.alpha)
		}
	}
}
