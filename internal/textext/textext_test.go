package textext

import (
	"archive/zip"
	"bytes"
	"context"
	"strings"
	"testing"
)

func TestDocx(t *testing.T) {
	var buf bytes.Buffer
	z := zip.NewWriter(&buf)
	w, _ := z.Create("word/document.xml")
	w.Write([]byte(`<w:document xmlns:w="x"><w:body><w:p><w:r><w:t>Первый абзац</w:t></w:r></w:p><w:p><w:r><w:t>Второй</w:t></w:r></w:p></w:body></w:document>`))
	z.Close()
	got, err := Extract(context.Background(), "a.docx", "", bytes.NewReader(buf.Bytes()), int64(buf.Len()), t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(got, "Первый абзац") || !strings.Contains(got, "Второй") {
		t.Fatalf("%q", got)
	}
}

func TestTextAndHTML(t *testing.T) {
	got, _ := Extract(context.Background(), "a.html", "text/html", strings.NewReader("<p>Привет <b>мир</b></p>"), 0, "")
	if strings.Contains(got, "<") || !strings.Contains(got, "Привет") {
		t.Fatalf("%q", got)
	}
	if _, err := Extract(context.Background(), "a.bin", "application/octet-stream", strings.NewReader("x"), 1, ""); err != ErrUnsupported {
		t.Fatal("expected unsupported")
	}
}
