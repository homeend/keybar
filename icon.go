package main

import (
	"bytes"
	"image"
	"image/color"
	"image/png"
	"math"
)

// trayIcon draws a 64x64 glyph of the Keydial: a dial ring above a 3x2 key grid.
func trayIcon(state string) []byte {
	c := color.RGBA{0xee, 0xee, 0xee, 0xff}
	accent := color.RGBA{0x4c, 0xc9, 0xa6, 0xff}
	switch state {
	case "off":
		c = color.RGBA{0x80, 0x80, 0x80, 0xff}
		accent = c
	case "warn":
		accent = color.RGBA{0xfb, 0x8c, 0x00, 0xff}
	}
	const n = 64
	img := image.NewRGBA(image.Rect(0, 0, n, n))
	for y := 0; y < n; y++ {
		for x := 0; x < n; x++ {
			fx, fy := float64(x)+0.5, float64(y)+0.5
			// dial ring centred at (32,17), r 9..14, with an indicator notch
			d := math.Hypot(fx-32, fy-17)
			if d >= 9 && d <= 14 {
				img.Set(x, y, c)
			}
			if math.Abs(fx-32) <= 2 && fy >= 5 && fy <= 12 {
				img.Set(x, y, accent)
			}
			// keys: 3 columns x 2 rows of rounded squares
			for row := 0; row < 2; row++ {
				for col := 0; col < 3; col++ {
					x0, y0 := 8+float64(col)*17, 35+float64(row)*14
					if roundedRect(fx, fy, x0, y0, 14, 11, 3) {
						img.Set(x, y, c)
					}
				}
			}
		}
	}
	var buf bytes.Buffer
	png.Encode(&buf, img)
	return buf.Bytes()
}

func roundedRect(px, py, x, y, w, h, r float64) bool {
	if px < x || px > x+w || py < y || py > y+h {
		return false
	}
	cx := math.Max(x+r, math.Min(px, x+w-r))
	cy := math.Max(y+r, math.Min(py, y+h-r))
	return math.Hypot(px-cx, py-cy) <= r
}
