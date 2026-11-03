// Package media — EXIF, миниатюры изображений, видео и PDF.
package media

import (
	"bytes"
	"encoding/binary"
	"errors"
	"io"
	"strings"
	"time"
)

// Exif — то немногое из EXIF, что нужно облаку.
type Exif struct {
	Orientation int        // 1..8, 0 — неизвестно
	Taken       *time.Time // DateTimeOriginal
}

// ReadExif читает EXIF из начала JPEG (достаточно первых ~256 КБ).
func ReadExif(r io.Reader, loc *time.Location) (*Exif, error) {
	buf, err := io.ReadAll(io.LimitReader(r, 256<<10))
	if err != nil {
		return nil, err
	}
	return ParseExif(buf, loc)
}

func ParseExif(b []byte, loc *time.Location) (*Exif, error) {
	if len(b) < 4 || b[0] != 0xFF || b[1] != 0xD8 {
		return nil, errors.New("not a jpeg")
	}
	pos := 2
	for pos+4 <= len(b) {
		if b[pos] != 0xFF {
			return nil, errors.New("bad marker")
		}
		marker := b[pos+1]
		if marker == 0xD8 || (marker >= 0xD0 && marker <= 0xD7) || marker == 0x01 {
			pos += 2
			continue
		}
		if marker == 0xDA || marker == 0xD9 { // начались данные изображения — EXIF уже не будет
			break
		}
		segLen := int(binary.BigEndian.Uint16(b[pos+2:]))
		if segLen < 2 || pos+2+segLen > len(b) {
			break
		}
		seg := b[pos+4 : pos+2+segLen]
		if marker == 0xE1 && bytes.HasPrefix(seg, []byte("Exif\x00\x00")) {
			return parseTIFF(seg[6:], loc)
		}
		pos += 2 + segLen
	}
	return nil, errors.New("no exif")
}

type tiff struct {
	b  []byte
	bo binary.ByteOrder
}

func (t *tiff) u16(off int) (uint16, bool) {
	if off < 0 || off+2 > len(t.b) {
		return 0, false
	}
	return t.bo.Uint16(t.b[off:]), true
}

func (t *tiff) u32(off int) (uint32, bool) {
	if off < 0 || off+4 > len(t.b) {
		return 0, false
	}
	return t.bo.Uint32(t.b[off:]), true
}

type ifdEntry struct {
	tag, typ uint16
	count    uint32
	valueOff int // смещение значения (или самого поля, если значение помещается в 4 байта)
}

func (t *tiff) ifd(off int) []ifdEntry {
	n, ok := t.u16(off)
	if !ok || n > 500 {
		return nil
	}
	out := make([]ifdEntry, 0, n)
	for i := 0; i < int(n); i++ {
		e := off + 2 + i*12
		tag, ok1 := t.u16(e)
		typ, ok2 := t.u16(e + 2)
		cnt, ok3 := t.u32(e + 4)
		if !ok1 || !ok2 || !ok3 {
			break
		}
		size := typeSize(typ) * int(cnt)
		vo := e + 8
		if size > 4 {
			p, _ := t.u32(e + 8)
			vo = int(p)
		}
		out = append(out, ifdEntry{tag, typ, cnt, vo})
	}
	return out
}

func typeSize(typ uint16) int {
	switch typ {
	case 1, 2, 6, 7:
		return 1
	case 3, 8:
		return 2
	case 4, 9, 11:
		return 4
	case 5, 10, 12:
		return 8
	}
	return 1
}

func (t *tiff) ascii(e ifdEntry) string {
	end := e.valueOff + int(e.count)
	if e.valueOff < 0 || end > len(t.b) || e.count > 64 {
		return ""
	}
	return strings.TrimRight(string(t.b[e.valueOff:end]), "\x00 ")
}

func parseTIFF(b []byte, loc *time.Location) (*Exif, error) {
	if len(b) < 8 {
		return nil, errors.New("short tiff")
	}
	t := &tiff{b: b}
	switch string(b[:2]) {
	case "II":
		t.bo = binary.LittleEndian
	case "MM":
		t.bo = binary.BigEndian
	default:
		return nil, errors.New("bad byte order")
	}
	ifd0, _ := t.u32(4)
	ex := &Exif{}
	var dateTime, original, offset string
	for _, e := range t.ifd(int(ifd0)) {
		switch e.tag {
		case 0x0112:
			if v, ok := t.u16(e.valueOff); ok && v >= 1 && v <= 8 {
				ex.Orientation = int(v)
			}
		case 0x0132:
			dateTime = t.ascii(e)
		case 0x8769:
			sub, _ := t.u32(e.valueOff)
			for _, se := range t.ifd(int(sub)) {
				switch se.tag {
				case 0x9003:
					original = t.ascii(se)
				case 0x9011:
					offset = t.ascii(se)
				}
			}
		}
	}
	s := original
	if s == "" {
		s = dateTime
	}
	if s != "" && !strings.HasPrefix(s, "0000") {
		if loc == nil {
			loc = time.Local
		}
		var tm time.Time
		var err error
		if offset != "" {
			tm, err = time.Parse("2006:01:02 15:04:05-07:00", s+offset)
		} else {
			tm, err = time.ParseInLocation("2006:01:02 15:04:05", s, loc)
		}
		if err == nil && tm.Year() > 1970 && tm.Year() < 2200 {
			ex.Taken = &tm
		}
	}
	return ex, nil
}
