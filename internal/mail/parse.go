package mail

import (
	"bytes"
	"encoding/base64"
	"errors"
	"fmt"
	"io"
	"mime"
	"strings"
	"time"

	_ "github.com/emersion/go-message/charset" // windows-1251, KOI8-R и прочие кодировки старых писем
	gomail "github.com/emersion/go-message/mail"
)

// Parsed — письмо, разобранное для показа.
//
// HTML отдаётся как есть: очищает его клиент (DOMPurify) и показывает в изолированном iframe
// без скриптов. Встроенные картинки (cid:) сразу подставлены как data:-адреса.
type Parsed struct {
	UID         uint32            `json:"uid"`
	Folder      string            `json:"folder"`
	Subject     string            `json:"subject"`
	From        []Address         `json:"from"`
	To          []Address         `json:"to"`
	Cc          []Address         `json:"cc"`
	ReplyTo     []Address         `json:"reply_to"`
	Date        time.Time         `json:"date"`
	MessageID   string            `json:"message_id"`
	References  []string          `json:"references"`
	Text        string            `json:"text"`
	HTML        string            `json:"html"`
	Inline      map[string]string `json:"inline"` // Content-ID → data:-адрес
	Attachments []Attachment      `json:"attachments"`
	Flagged     bool              `json:"flagged"`
}

type Attachment struct {
	Index    int    `json:"index"`
	Filename string `json:"filename"`
	Mime     string `json:"mime"`
	Size     int64  `json:"size"`
}

const (
	maxTextPart   = 4 << 20
	maxInlineImg  = 1 << 20
	maxInlineSum  = 6 << 20
	maxAttachSize = maxMessageBytes
)

func toAddrs(list []*gomail.Address) []Address {
	out := []Address{}
	for _, a := range list {
		out = append(out, Address{Name: a.Name, Email: a.Address})
	}
	return out
}

// part — очередная часть письма: текст, картинка или вложение.
type part struct {
	kind     string // text, html, inline, attachment
	mime     string
	filename string
	cid      string
	body     io.Reader
}

// walk обходит части письма в фиксированном порядке — номера вложений стабильны между запросами.
func walk(msg []byte, fn func(p part) error) (*gomail.Reader, error) {
	mr, err := gomail.CreateReader(bytes.NewReader(msg))
	if err != nil && mr == nil {
		return nil, fmt.Errorf("не удалось разобрать письмо: %v", err)
	}
	for {
		p, err := mr.NextPart()
		if err == io.EOF {
			break
		}
		if err != nil {
			// неизвестная кодировка или битая часть — показываем то, что удалось прочитать
			if p == nil {
				break
			}
		}
		var pt part
		switch h := p.Header.(type) {
		case *gomail.InlineHeader:
			ct, _, _ := h.ContentType()
			pt.mime = strings.ToLower(ct)
			pt.cid = strings.Trim(h.Get("Content-Id"), "<> ")
			_, params, _ := mime.ParseMediaType(h.Get("Content-Disposition"))
			pt.filename = params["filename"]
			switch {
			case pt.mime == "text/plain" && pt.filename == "":
				pt.kind = "text"
			case pt.mime == "text/html" && pt.filename == "":
				pt.kind = "html"
			case strings.HasPrefix(pt.mime, "image/") && pt.cid != "":
				pt.kind = "inline"
			default:
				pt.kind = "attachment" // встроенный PDF и т. п. — показываем как вложение
			}
		case *gomail.AttachmentHeader:
			ct, _, _ := h.ContentType()
			pt.mime = strings.ToLower(ct)
			pt.filename, _ = h.Filename()
			pt.cid = strings.Trim(h.Get("Content-Id"), "<> ")
			pt.kind = "attachment"
			// go-message считает вложением любую нетекстовую часть без Content-Disposition, но картинка
			// с Content-ID, явно не помеченная как вложение, — это встроенная картинка письма (cid:)
			disp, _, _ := mime.ParseMediaType(h.Get("Content-Disposition"))
			if pt.cid != "" && strings.HasPrefix(pt.mime, "image/") && disp != "attachment" {
				pt.kind = "inline"
			}
		default:
			continue
		}
		pt.body = p.Body
		if err := fn(pt); err != nil {
			return mr, err
		}
	}
	return mr, nil
}

// inliner решает, встроить картинку в письмо или показать вложением. Одно и то же решение
// используют Parse и AttachmentData — иначе номера вложений разъехались бы между запросами.
type inliner struct{ sum int }

func (in *inliner) fits(p part, size int) bool {
	if p.kind != "inline" || size > maxInlineImg || in.sum+size > maxInlineSum || !safeImage(p.mime) {
		return false
	}
	in.sum += size
	return true
}

// Parse разбирает письмо для показа.
func Parse(msg []byte) (*Parsed, error) {
	out := &Parsed{From: []Address{}, To: []Address{}, Cc: []Address{}, ReplyTo: []Address{}, Inline: map[string]string{},
		Attachments: []Attachment{}, References: []string{}}
	var inl inliner
	idx := 0
	mr, err := walk(msg, func(p part) error {
		if p.kind == "text" || p.kind == "html" {
			b, _ := io.ReadAll(io.LimitReader(p.body, maxTextPart))
			if p.kind == "text" && out.Text == "" {
				out.Text = string(b)
			} else if p.kind == "html" && out.HTML == "" {
				out.HTML = string(b)
			}
			return nil
		}
		b, _ := io.ReadAll(io.LimitReader(p.body, maxAttachSize))
		if inl.fits(p, len(b)) {
			out.Inline[p.cid] = "data:" + p.mime + ";base64," + base64.StdEncoding.EncodeToString(b)
			return nil
		}
		out.Attachments = append(out.Attachments, Attachment{Index: idx, Filename: nameOr(p.filename, p.cid, p.mime), Mime: p.mime, Size: int64(len(b))})
		idx++
		return nil
	})
	if mr == nil {
		return nil, err
	}
	h := mr.Header
	out.Subject, _ = h.Subject()
	if l, err := h.AddressList("From"); err == nil {
		out.From = toAddrs(l)
	}
	if l, err := h.AddressList("To"); err == nil {
		out.To = toAddrs(l)
	}
	if l, err := h.AddressList("Cc"); err == nil {
		out.Cc = toAddrs(l)
	}
	if l, err := h.AddressList("Reply-To"); err == nil {
		out.ReplyTo = toAddrs(l)
	}
	out.Date, _ = h.Date()
	out.MessageID, _ = h.MessageID()
	if refs, err := h.MsgIDList("References"); err == nil {
		out.References = refs
	}
	return out, nil
}

// inlineSafe — картинки, которые можно встроить data:-адресом (SVG может содержать скрипты — нельзя).
func safeImage(m string) bool {
	switch m {
	case "image/png", "image/jpeg", "image/gif", "image/webp", "image/bmp":
		return true
	}
	return false
}

func nameOr(filename, cid, mimeType string) string {
	if filename != "" {
		return filename
	}
	ext := ""
	if exts, _ := mime.ExtensionsByType(mimeType); len(exts) > 0 {
		ext = exts[0]
	}
	if cid != "" {
		return cid + ext
	}
	return "вложение" + ext
}

var ErrNoAttachment = errors.New("вложение не найдено")

// AttachmentData — n-е вложение письма (нумерация как в Parse).
func AttachmentData(msg []byte, n int) (*Attachment, []byte, error) {
	var found *Attachment
	var data []byte
	var inl inliner
	idx := 0
	_, err := walk(msg, func(p part) error {
		if p.kind == "text" || p.kind == "html" {
			return nil
		}
		b, _ := io.ReadAll(io.LimitReader(p.body, maxAttachSize))
		if inl.fits(p, len(b)) {
			return nil // встроенная картинка показывается в письме, номер вложения не занимает
		}
		if idx == n {
			found = &Attachment{Index: idx, Filename: nameOr(p.filename, p.cid, p.mime), Mime: p.mime, Size: int64(len(b))}
			data = b
			return io.EOF // нашли — дальше не читаем
		}
		idx++
		return nil
	})
	if found != nil {
		return found, data, nil
	}
	if err != nil && err != io.EOF {
		return nil, nil, err
	}
	return nil, nil, ErrNoAttachment
}
