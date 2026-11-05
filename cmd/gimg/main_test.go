package main

import (
	"bytes"
	"context"
	"fmt"
	"image"
	"image/color"
	"image/jpeg"
	"image/png"
	"io"
	"os"
	"path/filepath"
	"strings"
	"testing"

	resize "github.com/divyam234/gimg"
	"github.com/divyam234/gimg/process"
	nativeheic "github.com/gen2brain/h265/heic"
	"github.com/gen2brain/jpegn"
	"github.com/gen2brain/vpx/webp"
)

func fixture(t *testing.T, dir, format string) string {
	t.Helper()
	img := image.NewNRGBA(image.Rect(0, 0, 48, 32))
	for y := range 32 {
		for x := range 48 {
			alpha := uint8(255)
			if x < 16 {
				alpha = 0
			} else if x < 32 {
				alpha = 128
			}
			img.SetNRGBA(x, y, color.NRGBA{R: uint8(20 + x*3), G: uint8(40 + y*4), B: 120, A: alpha})
		}
	}
	var data bytes.Buffer
	var err error
	switch format {
	case "png":
		err = png.Encode(&data, img)
	case "jpg":
		err = jpegn.Encode(&data, img, &jpegn.EncodeOptions{Quality: 90, Subsampling: jpegn.Subsample420})
	case "webp":
		err = webp.Encode(&data, img, webp.EncodeOptions{Lossless: true, Method: 0, Threads: 1})
	case "heic":
		err = nativeheic.Encode(&data, img, nativeheic.EncodeOptions{Quality: 95})
	default:
		t.Fatalf("unknown fixture format %q", format)
	}
	if err != nil {
		t.Fatal(err)
	}
	// Deliberately omit the extension: decoding must follow the file contents.
	path := filepath.Join(dir, "input-"+format)
	if err := os.WriteFile(path, data.Bytes(), 0600); err != nil {
		t.Fatal(err)
	}
	return path
}

func readOutput(t *testing.T, path, format string) image.Image {
	t.Helper()
	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	var img image.Image
	switch format {
	case "png":
		img, err = png.Decode(bytes.NewReader(data))
	case "jpg", "jpeg":
		img, err = jpeg.Decode(bytes.NewReader(data))
	case "webp":
		img, err = webp.Decode(bytes.NewReader(data), webp.Options{ToRGBA: true, Threads: 1})
	case "heic", "heif":
		img, err = nativeheic.Decode(bytes.NewReader(data), nativeheic.Options{AutoRotate: true, Threads: 1})
	default:
		t.Fatalf("unknown output format %q", format)
	}
	if err != nil {
		t.Fatalf("decode output: %v", err)
	}
	return img
}

func TestCLIExactDimensions(t *testing.T) {
	for _, inputFormat := range []string{"png", "jpg", "webp", "heic"} {
		for _, outputFormat := range []string{"png", "jpg", "jpeg", "webp", "heic", "heif"} {
			t.Run(inputFormat+"-to-"+outputFormat, func(t *testing.T) {
				dir := t.TempDir()
				input := fixture(t, dir, inputFormat)
				output := filepath.Join(dir, "output."+outputFormat)
				args := []string{"-width", "13", "-height", "9", "-workers", "1", "-webp-method", "0"}
				if inputFormat == "jpg" {
					args = append(args, "-jpeg-shrink", "-filter", "bicubic")
				}
				args = append(args, input, output)
				var stderr bytes.Buffer
				if status := run(args, &stderr); status != 0 {
					t.Fatalf("exit %d: %s", status, &stderr)
				}
				img := readOutput(t, output, outputFormat)
				if got := img.Bounds().Size(); got != image.Pt(13, 9) {
					t.Fatalf("dimensions = %v, want (13,9)", got)
				}
				if outputFormat != "jpg" && outputFormat != "jpeg" {
					for _, sample := range []struct {
						x, minAlpha, maxAlpha int
					}{{0, 0, 0}, {6, 126, 130}, {12, 255, 255}} {
						minAlpha, maxAlpha := sample.minAlpha, sample.maxAlpha
						// JPEG fixtures have no alpha. HEIC preserves alpha through
						// lossy coding, so allow a small error even inside flat bands.
						if inputFormat == "jpg" {
							minAlpha, maxAlpha = 255, 255
						}
						if inputFormat == "heic" || outputFormat == "heic" || outputFormat == "heif" {
							wantAlpha := (minAlpha + maxAlpha) / 2
							minAlpha = max(0, wantAlpha-3)
							maxAlpha = min(255, wantAlpha+3)
						}
						_, _, _, alpha := img.At(sample.x, 4).RGBA()
						a := int(alpha >> 8)
						if a < minAlpha || a > maxAlpha {
							t.Errorf("alpha at (%d,4) = %d, want [%d,%d]", sample.x, a, minAlpha, maxAlpha)
						}
					}
				}
			})
		}
	}
}

func TestCLIOrientationTransforms(t *testing.T) {
	dir := t.TempDir()
	colors := []color.NRGBA{
		{R: 255, A: 255}, {G: 255, A: 255}, {B: 255, A: 255},
		{R: 255, G: 255, A: 255}, {G: 255, B: 255, A: 255}, {R: 255, B: 255, A: 255},
	}
	source := image.NewNRGBA(image.Rect(0, 0, 3, 2))
	for i, c := range colors {
		source.SetNRGBA(i%3, i/3, c)
	}
	var encoded bytes.Buffer
	if err := png.Encode(&encoded, source); err != nil {
		t.Fatal(err)
	}
	input := filepath.Join(dir, "input.png")
	if err := os.WriteFile(input, encoded.Bytes(), 0600); err != nil {
		t.Fatal(err)
	}
	for _, tc := range []struct {
		name  string
		flags []string
		size  image.Point
		order []int
	}{
		{"clockwise", []string{"-rotate", "90"}, image.Pt(2, 3), []int{3, 0, 4, 1, 5, 2}},
		{"counterclockwise", []string{"-rotate", "-90"}, image.Pt(2, 3), []int{2, 5, 1, 4, 0, 3}},
		{"normalized", []string{"-rotate", "450"}, image.Pt(2, 3), []int{3, 0, 4, 1, 5, 2}},
		{"horizontal", []string{"-flip-horizontal"}, image.Pt(3, 2), []int{2, 1, 0, 5, 4, 3}},
		{"vertical", []string{"-flip-vertical"}, image.Pt(3, 2), []int{3, 4, 5, 0, 1, 2}},
		{"rotate-then-horizontal", []string{"-rotate", "90", "-flip-horizontal"}, image.Pt(2, 3), []int{0, 3, 1, 4, 2, 5}},
		{"rotate-then-both", []string{"-rotate", "90", "-flip-horizontal", "-flip-vertical"}, image.Pt(2, 3), []int{2, 5, 1, 4, 0, 3}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			output := filepath.Join(dir, tc.name+".png")
			args := []string{"-mode", "fit", "-width", "3", "-height", "3", "-workers", "1"}
			args = append(args, tc.flags...)
			var stderr bytes.Buffer
			if status := run(append(args, input, output), &stderr); status != 0 {
				t.Fatalf("exit %d: %s", status, &stderr)
			}
			got := readOutput(t, output, "png")
			if got.Bounds().Size() != tc.size {
				t.Fatalf("dimensions = %v, want %v", got.Bounds().Size(), tc.size)
			}
			for i, sourceIndex := range tc.order {
				x, y := i%tc.size.X, i/tc.size.X
				if c := color.NRGBAModel.Convert(got.At(x, y)); c != colors[sourceIndex] {
					t.Errorf("pixel (%d,%d) = %v, want %v", x, y, c, colors[sourceIndex])
				}
			}
		})
	}
}

func TestCLIEXIFOrientationOptOut(t *testing.T) {
	dir := t.TempDir()
	input := fixture(t, dir, "jpg")
	data, err := os.ReadFile(input)
	if err != nil {
		t.Fatal(err)
	}
	source, err := jpegn.Decode(bytes.NewReader(data), &jpegn.Options{ToRGBA: true})
	if err != nil {
		t.Fatal(err)
	}
	// APP1 contains a little-endian TIFF with one SHORT orientation tag: 6
	// means a clockwise quarter turn.
	app1 := []byte{
		0xff, 0xe1, 0x00, 0x22,
		'E', 'x', 'i', 'f', 0, 0,
		'I', 'I', 42, 0, 8, 0, 0, 0,
		1, 0, 0x12, 0x01, 3, 0, 1, 0, 0, 0, 6, 0, 0, 0,
		0, 0, 0, 0,
	}
	oriented := append(bytes.Clone(data[:2]), app1...)
	oriented = append(oriented, data[2:]...)
	if err := os.WriteFile(input, oriented, 0600); err != nil {
		t.Fatal(err)
	}
	for _, disable := range []bool{false, true} {
		t.Run(fmt.Sprintf("disabled-%v", disable), func(t *testing.T) {
			output := filepath.Join(dir, fmt.Sprintf("oriented-%v.png", disable))
			args := []string{"-mode", "fit", "-width", "48", "-height", "48", "-workers", "1"}
			if disable {
				args = append(args, "-auto-orient=false")
			}
			var stderr bytes.Buffer
			if status := run(append(args, input, output), &stderr); status != 0 {
				t.Fatalf("exit %d: %s", status, &stderr)
			}
			got := readOutput(t, output, "png")
			wantSize := image.Pt(32, 48)
			if disable {
				wantSize = image.Pt(48, 32)
			}
			if got.Bounds().Size() != wantSize {
				t.Fatalf("dimensions = %v, want %v", got.Bounds().Size(), wantSize)
			}
			for y := range wantSize.Y {
				for x := range wantSize.X {
					sx, sy := y, 31-x
					if disable {
						sx, sy = x, y
					}
					want := color.RGBAModel.Convert(source.At(sx, sy))
					if c := color.RGBAModel.Convert(got.At(x, y)); c != want {
						t.Fatalf("pixel (%d,%d) = %v, want %v", x, y, c, want)
					}
				}
			}
		})
	}
}

func TestJPEGResizePreservesFullConversionPixels(t *testing.T) {
	dir := t.TempDir()
	input := fixture(t, dir, "jpg")
	data, err := os.ReadFile(input)
	if err != nil {
		t.Fatal(err)
	}
	for _, filter := range []struct {
		name string
		kind resize.Filter
	}{{"bicubic", resize.Bicubic}, {"lanczos3", resize.Lanczos3}} {
		for _, shrink := range []bool{false, true} {
			t.Run(fmt.Sprintf("%s/shrink%v", filter.name, shrink), func(t *testing.T) {
				denominator := 1
				if shrink {
					denominator = 2 // 48x32 input: 4x decoding is smaller than 13x9.
				}
				source, err := jpegn.Decode(bytes.NewReader(data), &jpegn.Options{
					ToRGBA: true, ScaleDenom: denominator,
				})
				if err != nil {
					t.Fatal(err)
				}
				want := image.NewRGBA(image.Rect(0, 0, 13, 9))
				r := resize.New(2)
				err = r.Resize(want, source.(*image.RGBA), filter.kind)
				r.Close()
				if err != nil {
					t.Fatal(err)
				}
				output := filepath.Join(dir, "output.png")
				args := []string{"-width", "13", "-height", "9", "-workers", "2", "-filter", filter.name}
				if shrink {
					args = append(args, "-jpeg-shrink")
				}
				var stderr bytes.Buffer
				if status := run(append(args, input, output), &stderr); status != 0 {
					t.Fatalf("exit %d: %s", status, &stderr)
				}
				got := readOutput(t, output, "png")
				for y := range 9 {
					for x := range 13 {
						if color.RGBAModel.Convert(got.At(x, y)) != want.RGBAAt(x, y) {
							t.Fatalf("pixel (%d,%d) differs from full JPEG conversion", x, y)
						}
					}
				}
			})
		}
	}
}

func TestJPEGHeaderReplayWithoutSeeking(t *testing.T) {
	data, err := os.ReadFile(fixture(t, t.TempDir(), "jpg"))
	if err != nil {
		t.Fatal(err)
	}
	// Put SOF past DecodeConfig's usual prefix, forcing it to read farther.
	metadata := append([]byte{0xff, 0xef, 0x4e, 0x22}, make([]byte, 20000)...)
	largeHeader := append(bytes.Clone(data[:2]), metadata...)
	largeHeader = append(largeHeader, data[2:]...)
	for name, input := range map[string][]byte{"short": data, "large-header": largeHeader} {
		t.Run(name, func(t *testing.T) {
			want, err := jpegn.Decode(bytes.NewReader(input), &jpegn.Options{ToRGBA: true})
			if err != nil {
				t.Fatal(err)
			}
			// Expose only Read, like a pipe; Seek must not be required.
			p, err := process.New(process.Config{Workers: 2})
			if err != nil {
				t.Fatal(err)
			}
			defer p.Close()
			var encoded bytes.Buffer
			_, err = p.Transform(context.Background(), &encoded,
				struct{ io.Reader }{bytes.NewReader(input)},
				process.Options{Width: 48, Height: 32, Filter: resize.Lanczos3})
			if err != nil {
				t.Fatal(err)
			}
			got, err := png.Decode(&encoded)
			if err != nil {
				t.Fatal(err)
			}
			for y := range 32 {
				for x := range 48 {
					if color.RGBAModel.Convert(got.At(x, y)) != color.RGBAModel.Convert(want.At(x, y)) {
						t.Fatalf("streamed JPEG pixel (%d,%d) differs after header replay", x, y)
					}
				}
			}
		})
	}
}

func TestCLIRejectsInvalidArgumentsWithoutTruncation(t *testing.T) {
	dir := t.TempDir()
	input := fixture(t, dir, "png")
	for _, tc := range []struct {
		name  string
		flags []string
		ext   string
	}{
		{"missing-width", []string{"-height", "9"}, ".png"},
		{"missing-height", []string{"-width", "13"}, ".png"},
		{"negative-width", []string{"-width", "-1", "-height", "9"}, ".png"},
		{"zero-height", []string{"-width", "13", "-height", "0"}, ".png"},
		{"overflow", []string{"-width", fmt.Sprint(int(^uint(0) >> 1)), "-height", "9"}, ".png"},
		{"unknown-filter", []string{"-width", "13", "-height", "9", "-filter", "nearest"}, ".png"},
		{"quality-low", []string{"-width", "13", "-height", "9", "-quality", "0"}, ".png"},
		{"quality-high", []string{"-width", "13", "-height", "9", "-quality", "101"}, ".png"},
		{"workers", []string{"-width", "13", "-height", "9", "-workers", "0"}, ".png"},
		{"webp-method", []string{"-width", "13", "-height", "9", "-webp-method", "7"}, ".webp"},
		{"invalid-rotation", []string{"-width", "13", "-height", "9", "-rotate", "45"}, ".png"},
		{"output-format", []string{"-width", "13", "-height", "9"}, ".gif"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			output := filepath.Join(dir, tc.name+tc.ext)
			const sentinel = "do not truncate"
			if err := os.WriteFile(output, []byte(sentinel), 0600); err != nil {
				t.Fatal(err)
			}
			var stderr bytes.Buffer
			if run(append(tc.flags, input, output), &stderr) == 0 || stderr.Len() == 0 {
				t.Fatal("expected nonzero exit and error on stderr")
			}
			data, err := os.ReadFile(output)
			if err != nil || string(data) != sentinel {
				t.Fatalf("output was modified: %q, %v", data, err)
			}
		})
	}
	for _, args := range [][]string{{}, {"-width", "13", "-height", "9", input}, {"-width", "13", "-height", "9", input, "out.png", "extra"}} {
		var stderr bytes.Buffer
		if run(args, &stderr) == 0 || stderr.Len() == 0 {
			t.Fatalf("arguments %q should fail with stderr", args)
		}
	}
}

func TestCLICorruptInputDoesNotCreateOutput(t *testing.T) {
	for name, data := range map[string]string{
		"unknown": "this is not an image",
		"jpeg":    "\xff\xd8broken jpeg",
		"png":     "\x89PNG\r\n\x1a\nbroken png",
		"webp":    "RIFF\x10\x00\x00\x00WEBPbroken webp",
		"heic":    "\x00\x00\x00\x18ftypheic\x00\x00\x00\x00mif1heicbroken heic",
	} {
		t.Run(name, func(t *testing.T) {
			dir := t.TempDir()
			input := filepath.Join(dir, "corrupt")
			output := filepath.Join(dir, "output.png")
			if err := os.WriteFile(input, []byte(data), 0600); err != nil {
				t.Fatal(err)
			}
			var stderr bytes.Buffer
			if run([]string{"-width", "13", "-height", "9", input, output}, &stderr) == 0 || !strings.Contains(stderr.String(), "decode input") {
				t.Fatalf("expected input error, stderr: %s", &stderr)
			}
			if _, err := os.Stat(output); !os.IsNotExist(err) {
				t.Fatalf("output unexpectedly exists, stat error: %v", err)
			}
		})
	}
}

func TestCLIFitFillAndUpscalePolicy(t *testing.T) {
	for _, tc := range []struct {
		name  string
		flags []string
		size  image.Point
	}{
		{"fit-width", []string{"-mode", "fit", "-width", "12"}, image.Pt(12, 8)},
		{"fit-box", []string{"-mode", "fit", "-width", "12", "-height", "12"}, image.Pt(12, 8)},
		{"fit-no-upscale", []string{"-mode", "fit", "-width", "96", "-upscale=false"}, image.Pt(48, 32)},
		{"fill", []string{"-mode", "fill", "-width", "12", "-height", "12", "-anchor", "right"}, image.Pt(12, 12)},
		{"exact-upscale", []string{"-width", "96", "-height", "64"}, image.Pt(96, 64)},
	} {
		t.Run(tc.name, func(t *testing.T) {
			dir := t.TempDir()
			input := fixture(t, dir, "png")
			output := filepath.Join(dir, "output.png")
			args := append([]string{"-workers", "1"}, tc.flags...)
			var stderr bytes.Buffer
			if code := run(append(args, input, output), &stderr); code != 0 {
				t.Fatalf("exit %d: %s", code, &stderr)
			}
			if size := readOutput(t, output, "png").Bounds().Size(); size != tc.size {
				t.Fatalf("dimensions=%v, want %v", size, tc.size)
			}
		})
	}
}

func TestCLIFailedTransformPreservesExistingOutput(t *testing.T) {
	dir := t.TempDir()
	input := fixture(t, dir, "png")
	output := filepath.Join(dir, "output.png")
	const sentinel = "existing output"
	if err := os.WriteFile(output, []byte(sentinel), 0600); err != nil {
		t.Fatal(err)
	}
	var stderr bytes.Buffer
	if run([]string{"-width", "96", "-height", "64", "-upscale=false", "-workers", "1", input, output}, &stderr) == 0 {
		t.Fatal("prohibited enlargement succeeded")
	}
	data, err := os.ReadFile(output)
	if err != nil || string(data) != sentinel {
		t.Fatalf("failed transform modified output: %q, %v", data, err)
	}
}
