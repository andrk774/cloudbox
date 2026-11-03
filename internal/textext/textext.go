// Package textext извлекает текст из документов для полнотекстового поиска.
package textext

import (
	"archive/zip"
	"bytes"
	"context"
	"encoding/xml"
	"errors"
	"io"
	"os"
	"os/exec"
	"path"
	"sort"
	"strings"
	"time"
	"unicode/utf8"
)

// MaxText — сколько символов текста индексировать на файл.
const MaxText = 500_000

var ErrUnsupported = errors.New("тип не индексируется")

var textExt = map[string]bool{
	".txt": true, ".md": true, ".markdown": true, ".csv": true, ".tsv": true, ".log": true, ".json": true,
	".yml": true, ".yaml": true, ".toml": true, ".ini": true, ".conf": true, ".xml": true, ".html": true, ".htm": true,
	".go": true, ".py": true, ".js": true, ".ts": true, ".c": true, ".h": true, ".cpp": true, ".rs": true, ".java": true,
	".sh": true, ".sql": true, ".ino": true, ".css": true, ".tex": true, ".rtf": false,
}

// Supported — будет ли файл индексироваться.
func Supported(name, mime string) bool {
	ext := strings.ToLower(path.Ext(name))
	switch {
	case textExt[ext], strings.HasPrefix(mime, "text/"):
		return true
	case ext == ".pdf" || mime == "application/pdf":
		_, err := exec.LookPath("pdftotext")
		return err == nil
	case ext == ".docx", ext == ".odt", ext == ".xlsx", ext == ".ods", ext == ".pptx", ext == ".odp":
		return true
	}
	return false
}

// Extract достаёт текст из файла (r должен поддерживать ReaderAt для zip-форматов).
func Extract(ctx context.Context, name, mime string, r io.ReadSeeker, size int64, tmpDir string) (string, error) {
	ext := strings.ToLower(path.Ext(name))
	switch {
	case textExt[ext] || strings.HasPrefix(mime, "text/"):
		b, err := io.ReadAll(io.LimitReader(r, 4<<20))
		if err != nil {
			return "", err
		}
		if !utf8.Valid(b) {
			b = bytes.ToValidUTF8(b, []byte(" "))
		}
		s := string(b)
		if ext == ".html" || ext == ".htm" || ext == ".xml" {
			s = stripTags(s)
		}
		return clip(s), nil
	case ext == ".pdf" || mime == "application/pdf":
		return pdfText(ctx, r, tmpDir)
	case ext == ".docx":
		return zipXMLText(r, size, func(n string) bool { return n == "word/document.xml" })
	case ext == ".xlsx":
		return zipXMLText(r, size, func(n string) bool { return n == "xl/sharedStrings.xml" })
	case ext == ".pptx":
		return zipXMLText(r, size, func(n string) bool { return strings.HasPrefix(n, "ppt/slides/slide") && strings.HasSuffix(n, ".xml") })
	case ext == ".odt", ext == ".ods", ext == ".odp":
		return zipXMLText(r, size, func(n string) bool { return n == "content.xml" })
	}
	return "", ErrUnsupported
}

func clip(s string) string {
	s = strings.ReplaceAll(s, "\x00", " ")
	if len(s) > MaxText {
		s = s[:MaxText]
		for !utf8.ValidString(s) {
			s = s[:len(s)-1]
		}
	}
	return s
}

func pdfText(ctx context.Context, r io.Reader, tmpDir string) (string, error) {
	if _, err := exec.LookPath("pdftotext"); err != nil {
		return "", ErrUnsupported
	}
	f, err := os.CreateTemp(tmpDir, "pdf-*")
	if err != nil {
		return "", err
	}
	defer os.Remove(f.Name())
	if _, err := io.Copy(f, io.LimitReader(r, 512<<20)); err != nil {
		f.Close()
		return "", err
	}
	f.Close()
	ctx, cancel := context.WithTimeout(ctx, 60*time.Second)
	defer cancel()
	out, err := exec.CommandContext(ctx, "pdftotext", "-q", "-l", "200", "-enc", "UTF-8", f.Name(), "-").Output()
	if err != nil {
		return "", err
	}
	return clip(string(out)), nil
}

type readerAt interface {
	io.Reader
	io.ReaderAt
}

func zipXMLText(r io.ReadSeeker, size int64, want func(string) bool) (string, error) {
	var ra io.ReaderAt
	if x, ok := r.(io.ReaderAt); ok {
		ra = x
	} else {
		if size > 200<<20 {
			return "", ErrUnsupported
		}
		b, err := io.ReadAll(r)
		if err != nil {
			return "", err
		}
		ra = bytes.NewReader(b)
		size = int64(len(b))
	}
	zr, err := zip.NewReader(ra, size)
	if err != nil {
		return "", err
	}
	var names []*zip.File
	for _, f := range zr.File {
		if want(f.Name) {
			names = append(names, f)
		}
	}
	sort.Slice(names, func(i, j int) bool { return naturalLess(names[i].Name, names[j].Name) })
	var sb strings.Builder
	for _, f := range names {
		rc, err := f.Open()
		if err != nil {
			continue
		}
		xmlText(io.LimitReader(rc, 64<<20), &sb)
		rc.Close()
		if sb.Len() > MaxText {
			break
		}
	}
	return clip(sb.String()), nil
}

// xmlText собирает текст из XML, разделяя абзацы/ячейки переводами строк.
func xmlText(r io.Reader, sb *strings.Builder) {
	dec := xml.NewDecoder(r)
	dec.Strict = false
	for {
		tok, err := dec.Token()
		if err != nil {
			return
		}
		switch t := tok.(type) {
		case xml.CharData:
			sb.Write(t)
		case xml.EndElement:
			switch t.Name.Local {
			case "p", "si", "h", "tr", "br", "tab", "list-item", "table-row":
				sb.WriteByte('\n')
			case "t", "tc", "table-cell":
				sb.WriteByte(' ')
			}
		}
	}
}

func stripTags(s string) string {
	var sb strings.Builder
	in := false
	for _, r := range s {
		switch {
		case r == '<':
			in = true
		case r == '>':
			in = false
			sb.WriteByte(' ')
		case !in:
			sb.WriteRune(r)
		}
	}
	return sb.String()
}

// naturalLess: slide2.xml < slide10.xml
func naturalLess(a, b string) bool {
	na, nb := strings.TrimRight(a, "0123456789.xml"), strings.TrimRight(b, "0123456789.xml")
	if na == nb && len(a) != len(b) {
		return len(a) < len(b)
	}
	return a < b
}
