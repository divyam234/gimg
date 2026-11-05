package gimg

func horizontalScalar(dst []int16, src []byte, spans []span, coefficients []int16) {
	for x, s := range spans {
		var red, green, blue, alpha int32
		for j, weight := range coefficients[s.offset : s.offset+s.count] {
			pixel := src[(s.start+j)*4:][:4]
			w := int32(weight)
			red += int32(pixel[0]) * w
			green += int32(pixel[1]) * w
			blue += int32(pixel[2]) * w
			alpha += int32(pixel[3]) * w
		}
		pixel := dst[x*4:][:4]
		pixel[0] = int16((red + (1 << (horizontalShift - 1))) >> horizontalShift)
		pixel[1] = int16((green + (1 << (horizontalShift - 1))) >> horizontalShift)
		pixel[2] = int16((blue + (1 << (horizontalShift - 1))) >> horizontalShift)
		pixel[3] = int16((alpha + (1 << (horizontalShift - 1))) >> horizontalShift)
	}
}

func verticalScalar(dst []byte, rows [][]int16, coefficients []int16) {
	verticalScalarRange(dst, rows, coefficients, 0)
}

// start is a channel offset aligned to a complete RGBA pixel.
func verticalScalarRange(dst []byte, rows [][]int16, coefficients []int16, start int) {
	for x := start; x < len(dst); x += 4 {
		var sums [4]int32
		for j, row := range rows {
			pixel := row[x:][:4]
			weight := int32(coefficients[j])
			sums[0] += int32(pixel[0]) * weight
			sums[1] += int32(pixel[1]) * weight
			sums[2] += int32(pixel[2]) * weight
			sums[3] += int32(pixel[3]) * weight
		}
		pixel := dst[x:][:4]
		for c, sum := range sums {
			value := (sum + (1 << (verticalShift - 1))) >> verticalShift
			if value < 0 {
				value = 0
			}
			if value > 255 {
				value = 255
			}
			pixel[c] = byte(value)
		}
		for c := range 3 {
			if pixel[c] > pixel[3] {
				pixel[c] = pixel[3]
			}
		}
	}
}
