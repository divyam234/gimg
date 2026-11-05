package process

import (
	"bytes"
	"context"
	"encoding/binary"
	"image"
	"image/color"
	"image/png"
	"testing"

	"github.com/gen2brain/pngn"
)

func TestPNGRejectsTruncationAndBadCRC(t *testing.T) {
	input := pngBytes(t, transformColorGrid())
	missingIEND := bytes.Clone(input[:len(input)-12])
	badCRC := bytes.Clone(input)
	found := false
	for offset := 8; offset+12 <= len(badCRC); {
		size := int(binary.BigEndian.Uint32(badCRC[offset:]))
		if string(badCRC[offset+4:offset+8]) == "IDAT" {
			badCRC[offset+8+size] ^= 0x80
			found = true
			break
		}
		offset += 12 + size
	}
	if !found {
		t.Fatal("fixture lacks IDAT")
	}
	p := processor(t, Config{})
	for name, data := range map[string][]byte{"missing-IEND": missingIEND, "bad-IDAT-CRC": badCRC} {
		t.Run(name, func(t *testing.T) {
			var dst bytes.Buffer
			dst.WriteString("unchanged")
			if _, err := p.Transform(context.Background(), &dst, bytes.NewReader(data), Options{Width: 3, Height: 2}); err == nil {
				t.Fatal("damaged PNG accepted")
			}
			if dst.String() != "unchanged" {
				t.Fatal("damaged input wrote encoded output")
			}
		})
	}
}

func TestPNGAdam7PaletteAndSixteenBit(t *testing.T) {
	p := processor(t, Config{})
	palette := color.Palette{color.NRGBA{R: 255, A: 255}, color.NRGBA{G: 255, A: 255}, color.NRGBA{B: 255, A: 128}, color.NRGBA{}}
	paletted := image.NewPaletted(image.Rect(0, 0, 9, 5), palette)
	for y := range 5 {
		for x := range 9 {
			paletted.SetColorIndex(x, y, uint8((x+2*y)%4))
		}
	}
	gray := image.NewGray16(image.Rect(0, 0, 7, 5))
	for y := range 5 {
		for x := range 7 {
			gray.SetGray16(x, y, color.Gray16{Y: uint16(x+10*y) * 257})
		}
	}
	for _, tc := range []struct {
		name      string
		img       image.Image
		interlace bool
	}{{"palette-Adam7", paletted, true}, {"gray16", gray, false}} {
		t.Run(tc.name, func(t *testing.T) {
			var source bytes.Buffer
			if err := pngn.Encode(&source, tc.img, &pngn.EncodeOptions{Interlace: tc.interlace}); err != nil {
				t.Fatal(err)
			}
			var dst bytes.Buffer
			size := tc.img.Bounds().Size()
			_, err := p.Transform(context.Background(), &dst, struct{ *bytes.Buffer }{&source}, Options{Width: size.X, Height: size.Y})
			if err != nil {
				t.Fatal(err)
			}
			// An independent standard decoder must accept the newly encoded PNG.
			got, err := png.Decode(&dst)
			if err != nil {
				t.Fatal(err)
			}
			want := transformReference(t, tc.img)
			if got.Bounds().Size() != size {
				t.Fatalf("dimensions=%v want%v", got.Bounds().Size(), size)
			}
			for y := range size.Y {
				for x := range size.X {
					if color.RGBAModel.Convert(got.At(x, y)) != color.RGBAModel.Convert(want.At(x, y)) {
						t.Fatalf("sample(%d,%d)=%v want%v", x, y, got.At(x, y), want.At(x, y))
					}
				}
			}
		})
	}
}
