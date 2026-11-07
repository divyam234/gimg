# gimg

A Go image resizer with reusable workers, SIMD-accelerated filters, and an image-processing pipeline for web image delivery.

The core package provides **bicubic** and **Lanczos3** RGBA resizing. The `process` package adds JPEG, PNG, WebP, and HEIC input/output, thumbnail and crop modes, EXIF orientation, rotation/flip operations, and optional disk caching. ARM64 NEON and amd64 AVX2 kernels are available where supported.

> **Benchmark status:** gimg is competitive on selected image conversions, but **libvips remains faster on the measured mixed JPEG/PNG → WebP throughput workload**. The numbers below come from controlled local experiments with real images, **not deployed production traffic**. See the [complete benchmark tables and methodology](WEBP_BENCHMARK_2026-11-07.md).

## Build and use

Requires Go **1.26.4+** (per `go.mod`).

```sh
go test ./...
go build -o gimg ./cmd/gimg

# Resize a JPEG to fit inside a 320×240 box and encode WebP.
./gimg -mode fit -width 320 -height 240 -jpeg-shrink \
  -quality 85 -webp-method 4 input.jpg thumbnail.webp

# Convert an existing PNG to WebP.
./gimg -mode fit -width 1280 -height 960 \
  -quality 85 -webp-method 4 input.png output.webp
```

`-jpeg-shrink` enables faster scaled JPEG decoding but may change resampling quality; it is **off by default**. The `-webp-method` flag controls encoder speed/fidelity tradeoffs; method **4** is the default. For available flags, run `./gimg -help`.

### Go API

```go
package main

import (
    "context"
    "os"

    gimg "github.com/divyam234/gimg"
    "github.com/divyam234/gimg/process"
)

func main() {
    p, err := process.New(process.Config{Concurrency: 2, Workers: 2})
    if err != nil { panic(err) }
    defer p.Close()

    in, err := os.Open("input.jpg")
    if err != nil { panic(err) }
    defer in.Close()

    out, err := os.Create("thumbnail.webp")
    if err != nil { panic(err) }
    defer out.Close()

    _, err = p.Transform(context.Background(), out, in, process.Options{
        Width: 320, Height: 240,
        Mode: process.Fit,
        Filter: gimg.Lanczos3,
        JPEGShrink: true,
        Encode: process.EncodeOptions{
            Format: process.WebP,
            Quality: 85,
            WebPMethod: 4,
        },
    })
    if err != nil { panic(err) }
}
```

## gimg vs libvips: measured results

Tested **2026-11-07** at gimg revision `3cebf09` on Linux ARM64 (Neoverse-N1, 2-CPU cgroup quota) against libvips 8.18.7. The single-request benchmarks used persistent processes with two workers, five warm-ups, and 35 timed conversions per case. Each conversion includes input decoding, resizing, and WebP encoding; libvips' operation cache was disabled. Both used nominal WebP quality **85** and encoding effort/method **4**.

| Input → output | gimg mean | libvips mean | gimg p95 | libvips p95 | Faster mean |
|---|---:|---:|---:|---:|---|
| JPEG landscape → WebP 320×240 (JPEG shrink) | 183.89 ms | **130.35 ms** | 196.08 ms | 148.95 ms | libvips |
| JPEG architecture → WebP 320×240 (JPEG shrink) | 275.05 ms | **177.01 ms** | 301.17 ms | 200.97 ms | libvips |
| JPEG landscape → WebP 1280×960 (JPEG shrink) | 482.88 ms | **462.69 ms** | 537.18 ms | 502.40 ms | libvips |
| Opaque photo PNG → WebP 320×240 | 348.41 ms | **286.56 ms** | 378.82 ms | 311.41 ms | libvips |
| Transparent photo PNG → WebP 320×240 | **377.56 ms** | 419.88 ms | 406.93 ms | 456.89 ms | gimg |
| Transparent photo PNG → WebP 1280×960 | 921.21 ms | **899.94 ms** | 956.11 ms | 932.89 ms | libvips |
| JPEG landscape → WebP 320×240 (**no** JPEG shrink) | 351.33 ms | **129.34 ms** | 494.15 ms | 156.64 ms | libvips |

The full report contains **95% bootstrap intervals, output byte sizes, memory usage, quality checks, source-image hashes, and concurrent throughput results**:

**[Read the complete JPEG/PNG → WebP benchmark report](WEBP_BENCHMARK_2026-11-07.md)**

### Mixed-concurrency result

An additional four-process, mixed real-image test (two JPEG and two PNG conversions, 32 measured requests per round, two rounds per engine) gave **7.49 requests/s for gimg** versus **9.93 requests/s for libvips**, including startup and warm-up overhead. In that test, libvips achieved approximately **33% higher throughput**. This is a small local sample, not a production service-level percentile.

### Interpretation

- **Do not equate equal quality numbers with equal visual quality.** Encoders and resizing pipelines differ, and the output files do not contain identical pixels.
- **Peak RSS is a process high-water measurement**, affected by Go/C allocators and worker lifetime; it is not per-request memory allocation.
- **These are not production SLO measurements.** They omit HTTP request handling, network fetches, queueing, cold-cache behavior, fleet heterogeneity, and sustained traffic. The limited corpus is not representative of all user uploads.
- **No benchmark-driven optimization is included in the source at this revision.** The benchmark report documents the baseline implementation, not an unpublished prototype.

## Benchmark artifacts

The [benchmark report](WEBP_BENCHMARK_2026-11-07.md) is the publication-ready record. Reproduction scripts, raw timing JSON, source fixtures, and encoded outputs remain in the local `.bench-local/` working directory and are not part of the library source changes.
