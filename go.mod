module github.com/divyam234/gimg

go 1.26.4

require (
	github.com/gen2brain/h265 v0.2.4
	github.com/gen2brain/jpegn v0.6.2
	github.com/gen2brain/pngn v0.1.3
	github.com/gen2brain/vpx v0.2.1
	golang.org/x/sys v0.39.0
)

replace github.com/gen2brain/jpegn => github.com/divyam234/jpegn v0.6.3-0.20261103061655-95c3cb56f29e

replace github.com/gen2brain/vpx => github.com/divyam234/vpx v0.2.2-0.20261105054717-fc5fa9cebc12
