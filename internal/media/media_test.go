package media

import (
	"bytes"
	"context"
	"encoding/binary"
	"image"
	"image/color"
	"image/jpeg"
	"testing"
	"time"
)

func makeJPEG(w, h int) []byte {
	img := image.NewRGBA(image.Rect(0, 0, w, h))
	for y := 0; y < h; y++ {
		for x := 0; x < w; x++ {
			img.Set(x, y, color.RGBA{uint8(x), uint8(y), 128, 255})
		}
	}
	var buf bytes.Buffer
	jpeg.Encode(&buf, img, nil)
	return buf.Bytes()
}

// withExif вставляет APP1 с Orientation и DateTimeOriginal (big-endian TIFF — проверяем и этот порядок байт).
func withExif(j []byte, orient uint16, date string) []byte {
	s := append([]byte(date), 0)
	t := []byte("MM\x00\x2a\x00\x00\x00\x08")
	be := binary.BigEndian
	add16 := func(v uint16) { t = be.AppendUint16(t, v) }
	add32 := func(v uint32) { t = be.AppendUint32(t, v) }
	add16(2)
	add16(0x0112)
	add16(3)
	add32(1)
	add16(orient)
	add16(0)
	add16(0x8769)
	add16(4)
	add32(1)
	add32(38)
	add32(0)
	add16(1)
	add16(0x9003)
	add16(2)
	add32(uint32(len(s)))
	add32(56)
	add32(0)
	t = append(t, s...)
	app := append([]byte("Exif\x00\x00"), t...)
	seg := []byte{0xFF, 0xE1}
	seg = be.AppendUint16(seg, uint16(len(app)+2))
	seg = append(seg, app...)
	return append(append(append([]byte{}, j[:2]...), seg...), j[2:]...)
}

func TestExifAndThumb(t *testing.T) {
	j := withExif(makeJPEG(400, 200), 6, "2023:05:09 14:15:16")
	ex, err := ParseExif(j, time.UTC)
	if err != nil {
		t.Fatal(err)
	}
	if ex.Orientation != 6 || ex.Taken == nil || ex.Taken.Format(time.DateTime) != "2023-05-09 14:15:16" {
		t.Fatalf("exif: %+v", ex)
	}
	th, err := Thumbnail(context.Background(), bytes.NewReader(j), int64(len(j)), "image/jpeg", 128, t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	cfg, err := jpeg.DecodeConfig(bytes.NewReader(th))
	if err != nil {
		t.Fatal(err)
	}
	if cfg.Width != 64 || cfg.Height != 128 { // 400x200 → 128x64 → поворот на 90°
		t.Fatalf("thumb %dx%d", cfg.Width, cfg.Height)
	}
	if _, err := ParseExif([]byte("not a jpeg"), nil); err == nil {
		t.Fatal("expected error")
	}
}

func TestSmallImageNotUpscaled(t *testing.T) {
	j := makeJPEG(50, 30)
	th, err := Thumbnail(context.Background(), bytes.NewReader(j), int64(len(j)), "image/jpeg", 256, t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	cfg, _ := jpeg.DecodeConfig(bytes.NewReader(th))
	if cfg.Width != 50 || cfg.Height != 30 {
		t.Fatalf("%dx%d", cfg.Width, cfg.Height)
	}
}
