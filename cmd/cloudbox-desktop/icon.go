package main

import (
	"bytes"
	"encoding/binary"
	"image"
	"image/color"
	"image/png"
	"runtime"
)

// iconFor рисует значок для трея: облако на круге; цвет — состояние (синий — норма, серый — пауза,
// оранжевый — нужна настройка, красный — ошибка). На Windows трей ждёт ICO, в остальных ОС — PNG.
func iconFor(state string) []byte {
	bg := map[string]color.RGBA{
		"error": {211, 58, 58, 255}, "paused": {120, 128, 140, 255}, "setup": {230, 140, 30, 255},
	}[state]
	if bg.A == 0 {
		bg = color.RGBA{47, 111, 237, 255}
	}
	const n = 32
	img := image.NewRGBA(image.Rect(0, 0, n, n))
	in := func(x, y, cx, cy, r float64) bool { return (x-cx)*(x-cx)+(y-cy)*(y-cy) <= r*r }
	for y := 0; y < n; y++ {
		for x := 0; x < n; x++ {
			fx, fy := float64(x)+.5, float64(y)+.5
			if !in(fx, fy, 16, 16, 15.5) {
				continue
			}
			img.Set(x, y, bg)
			// облако: три круга и основание
			cloud := in(fx, fy, 11.5, 18, 4.5) || in(fx, fy, 16.5, 14.5, 6) || in(fx, fy, 21.5, 18, 4.5) ||
				(fy >= 18 && fy <= 22.5 && fx >= 11.5 && fx <= 21.5)
			if cloud {
				img.Set(x, y, color.RGBA{255, 255, 255, 255})
			}
		}
	}
	var buf bytes.Buffer
	png.Encode(&buf, img)
	if runtime.GOOS != "windows" {
		return buf.Bytes()
	}
	// ICO с вложенным PNG (поддерживается начиная с Windows Vista)
	var ico bytes.Buffer
	binary.Write(&ico, binary.LittleEndian, []uint16{0, 1, 1})
	ico.Write([]byte{n, n, 0, 0})
	binary.Write(&ico, binary.LittleEndian, []uint16{1, 32})
	binary.Write(&ico, binary.LittleEndian, []uint32{uint32(buf.Len()), 22})
	ico.Write(buf.Bytes())
	return ico.Bytes()
}
