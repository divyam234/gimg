package main

import (
	"context"
	"errors"
	"flag"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"runtime"
	"strings"

	resize "github.com/divyam234/gimg"
	"github.com/divyam234/gimg/process"
)

func main() {
	os.Exit(run(os.Args[1:], os.Stderr))
}

func run(args []string, stderr io.Writer) int {
	flags := flag.NewFlagSet("gimg", flag.ContinueOnError)
	flags.SetOutput(stderr)
	width := flags.Int("width", 0, "output width (required except height-only fit)")
	height := flags.Int("height", 0, "output height (required except width-only fit)")
	modeName := flags.String("mode", "exact", "geometry: exact, fit, or fill")
	anchorName := flags.String("anchor", "center", "fill crop anchor: center, top-left, top, top-right, left, right, bottom-left, bottom, bottom-right")
	upscale := flags.Bool("upscale", true, "allow enlargement (disable with -upscale=false)")
	filterName := flags.String("filter", "lanczos3", "resampling filter: lanczos3 or bicubic")
	quality := flags.Int("quality", 85, "JPEG/WebP/HEIC quality (1–100)")
	workers := flags.Int("workers", runtime.GOMAXPROCS(0), "number of resize, WebP, and HEIC decode workers (positive)")
	webpMethod := flags.Int("webp-method", 4, "WebP speed/quality method (0–6)")
	jpegShrink := flags.Bool("jpeg-shrink", false, "use JPEG scaled decoding; faster, but changes resampling quality")
	autoOrient := flags.Bool("auto-orient", true, "apply EXIF or HEIC container orientation (disable with -auto-orient=false)")
	rotate := flags.Int("rotate", 0, "clockwise rotation in degrees (multiple of 90)")
	flipHorizontal := flags.Bool("flip-horizontal", false, "flip horizontally after rotation")
	flipVertical := flags.Bool("flip-vertical", false, "flip vertically after rotation")
	flags.Usage = func() {
		fmt.Fprintln(stderr, "Usage: gimg -width W -height H [options] INPUT OUTPUT")
		fmt.Fprintln(stderr, "Input: JPEG, PNG, WebP, HEIC (detected by signature). Output: .jpg, .jpeg, .png, .webp, .heic, .heif.")
		fmt.Fprintln(stderr, "Fit accepts either dimension alone. Fill crops using -anchor. JPEG scaled decoding is optional.")
		flags.PrintDefaults()
	}
	if err := flags.Parse(args); err != nil {
		if errors.Is(err, flag.ErrHelp) {
			return 0
		}
		return 1
	}
	fail := func(err error) int {
		fmt.Fprintln(stderr, "gimg:", err)
		return 1
	}
	if flags.NArg() != 2 {
		flags.Usage()
		return fail(errors.New("expected INPUT and OUTPUT"))
	}
	if *workers <= 0 {
		return fail(errors.New("-workers must be positive"))
	}
	if *quality < 1 || *quality > 100 {
		return fail(errors.New("-quality must be between 1 and 100"))
	}
	if *webpMethod < 0 || *webpMethod > 6 {
		return fail(errors.New("-webp-method must be between 0 and 6"))
	}
	opts := process.Options{
		Width: *width, Height: *height, AllowUpscale: *upscale, JPEGShrink: *jpegShrink,
		DisableAutoOrient: !*autoOrient, Rotate: *rotate,
		FlipHorizontal: *flipHorizontal, FlipVertical: *flipVertical,
	}
	switch *modeName {
	case "exact":
		opts.Mode = process.Exact
	case "fit":
		opts.Mode = process.Fit
	case "fill":
		opts.Mode = process.Fill
	default:
		return fail(fmt.Errorf("unsupported mode %q", *modeName))
	}
	switch *filterName {
	case "lanczos3":
		opts.Filter = resize.Lanczos3
	case "bicubic":
		opts.Filter = resize.Bicubic
	default:
		return fail(fmt.Errorf("unsupported filter %q", *filterName))
	}
	anchors := map[string]process.Anchor{
		"center": process.Center, "top-left": process.TopLeft,
		"top": process.Top, "top-right": process.TopRight, "left": process.Left,
		"right": process.Right, "bottom-left": process.BottomLeft,
		"bottom": process.Bottom, "bottom-right": process.BottomRight,
	}
	var ok bool
	if opts.Anchor, ok = anchors[*anchorName]; !ok {
		return fail(fmt.Errorf("unsupported anchor %q", *anchorName))
	}
	output := flags.Arg(1)
	switch strings.ToLower(filepath.Ext(output)) {
	case ".jpg", ".jpeg":
		opts.Encode.Format = process.JPEG
	case ".webp":
		opts.Encode.Format = process.WebP
	case ".png":
		opts.Encode.Format = process.PNG
	case ".heic", ".heif":
		opts.Encode.Format = process.HEIC
	default:
		return fail(fmt.Errorf("unsupported output extension %q", filepath.Ext(output)))
	}
	opts.Encode.Quality = *quality
	opts.Encode.WebPMethod = *webpMethod
	var err error
	if opts, err = process.ValidateOptions(opts); err != nil {
		return fail(err)
	}
	processor, err := process.New(process.Config{Concurrency: 1, Workers: *workers})
	if err != nil {
		return fail(err)
	}
	defer processor.Close()
	input, err := os.Open(flags.Arg(0))
	if err != nil {
		return fail(fmt.Errorf("open input: %w", err))
	}
	// Publish only complete outputs. This also leaves an existing output intact
	// when decoding, transformation, encoding, or writing fails.
	tmp, err := os.CreateTemp(filepath.Dir(output), ".gimg-output-*")
	if err != nil {
		return fail(errors.Join(fmt.Errorf("create output: %w", err), input.Close()))
	}
	defer os.Remove(tmp.Name())
	_, transformErr := processor.Transform(context.Background(), tmp, input, opts)
	if err := errors.Join(transformErr, input.Close(), tmp.Close()); err != nil {
		return fail(fmt.Errorf("decode input or transform output: %w", err))
	}
	if err := os.Rename(tmp.Name(), output); err != nil {
		return fail(fmt.Errorf("publish output: %w", err))
	}
	return 0
}
