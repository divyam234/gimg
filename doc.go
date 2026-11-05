// Package gimg resamples premultiplied RGBA8 images with centered,
// antialiased Bicubic or Lanczos3 filtering. Import it as
// github.com/divyam234/gimg; its package name is gimg.
//
// For encoded JPEG/PNG/WebP/HEIC reader-to-writer processing, automatic
// orientation, right-angle rotation/flips, aspect-preserving fit, anchored
// fill/crop, and optional encoded-result caching, use the process subpackage.
// The diskcache subpackage supplies local storage, expiry, byte-limit eviction, and optional
// background cleanup. Neither package runs a server.
//
// Create a Resizer once, reuse it for successive images, and always call Close
// to stop its persistent worker goroutines and release its scratch storage:
//
//	r := gimg.New(0) // Use the current GOMAXPROCS worker count.
//	defer r.Close()
//	dst := image.NewRGBA(image.Rect(0, 0, 320, 240))
//	if err := r.Resize(dst, src, gimg.Lanczos3); err != nil {
//		return err
//	}
//
// The caller supplies both images and their backing storage. Inputs must use
// image.RGBA's premultiplied representation: each RGB channel is at most alpha.
// Filtering operates directly in the encoded color space, without gamma
// conversion or unpremultiplication. Signed filter lobes are retained between
// passes; the final result saturates to RGBA8 and clamps RGB to alpha, preserving
// valid premultiplied output. Applications needing linear-light resampling must
// arrange color-space conversion separately.
//
// Pixel centers are aligned using (destinationIndex+0.5)*scale-0.5. Reduction
// widens the reconstruction kernel for antialiasing; boundary taps are clipped
// to the image and renormalized. Nonzero image origins, subimages, and positive
// padded strides are supported. Padding is not modified. Invalid image storage
// and overlapping Pix slices are rejected, except that identical pixel mappings
// are an unchanged no-op. Distinct equal-size images are copied exactly.
//
// ResizeRows accepts an RGBARowSource instead of a full source image. Providers
// fill premultiplied RGBA rows in caller-provided buffers and must support
// concurrent reads with separate scratch. Providers own source validation and
// must not alias the destination; an error may leave it partially written.
//
// Native Go assembly is selected automatically: AVX2 on supported amd64 CPUs
// and NEON on arm64. Other CPUs use scalar Go, and the purego build tag forces
// the scalar implementation. No cgo is required.
//
// A Resizer caches the most recent dimensions and filter and retains reusable
// worker buffers. Each worker horizontally filters source rows into a ring
// bounded by the vertical filter's maximum tap count times the destination row
// width, rather than storing a full intermediate image. Row sources additionally
// retain one source-width RGBA row and provider scratch per worker. Initial use
// and larger plans or buffers may allocate. After warm-up, unchanged dimensions,
// filter, and row-provider scratch requirements need no additional storage.
// Caller-owned image allocation is outside this contract. Dimension/filter
// changes rebuild plans, reusing sufficient capacity. Close releases plans
// and buffers, is idempotent, and makes later resize calls return ErrClosed.
// Resize, ResizeRows, and Close are serialized and safe to make concurrently,
// but image storage must not otherwise be accessed while resizing. A Resizer must
// be constructed with New and must not be copied.
//
// The gimg command adds native Go SIMD JPEG, PNG, WebP, and HEIC codecs.
// PNG uses gen2brain/pngn; HEIC uses gen2brain/h265/heic. No CGo, WebAssembly,
// or shared codec libraries are needed. All codec dependencies use upstream
// gen2brain releases directly; applications do not need module replacements.
//
//	CGO_ENABLED=0 go run ./cmd/gimg -width 1280 -height 852 input.jpg output.webp
//
// Its optional -jpeg-shrink flag uses JPEG scaled decoding, which changes output
// quality as well as decode cost; full-resolution decoding is the default.
// Automatic EXIF/HEIF orientation defaults on; -auto-orient=false disables it.
// The -rotate, -flip-horizontal, and -flip-vertical flags apply before resize/crop.
// JPEG decoding uses the upstream codec's RGBA output and may allocate a
// source-sized RGBA image. Nonidentity orientation/rotation/flip compositions use
// one additional oriented RGBA buffer; identity compositions reuse the decoded
// image directly.
//
// Run the retained resize-only benchmarks with:
//
//	CGO_ENABLED=0 go test -run '^$' -bench BenchmarkResize -benchmem
//
// Benchmarks exclude decoding, encoding, and caller-owned output allocation.
package gimg
