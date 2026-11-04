package mail

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"io"
	"net/mail"
	"strings"
	"time"
	"unicode/utf8"

	gomail "github.com/emersion/go-message/mail"
	"github.com/emersion/go-sasl"
	"github.com/emersion/go-smtp"
)

const (
	MaxRecipients = 50
	MaxSendBytes  = 25 << 20 // большинство серверов не принимают письма больше 25 МБ
)

var (
	ErrNoRecipients = errors.New("укажите хотя бы одного получателя")
	ErrTooMany      = fmt.Errorf("не больше %d получателей", MaxRecipients)
	ErrSendTooBig   = errors.New("письмо с вложениями больше 25 МБ — большинство серверов его не примут")
)

func (s *Service) smtpClient(ctx context.Context, a *Account) (*smtp.Client, error) {
	conn, err := s.Dialer.Dial(ctx, a.SMTPHost, a.SMTPPort, a.SMTPSec)
	if err != nil {
		return nil, err
	}
	conn.SetDeadline(time.Now().Add(sessionDeadline))
	var c *smtp.Client
	if a.SMTPSec == SecSTARTTLS {
		if c, err = smtp.NewClientStartTLS(conn, TLSConfig(a.SMTPHost)); err != nil {
			conn.Close()
			return nil, fmt.Errorf("STARTTLS: %v", err)
		}
	} else {
		c = smtp.NewClient(conn)
	}
	if err := c.Hello("cloudbox"); err != nil {
		c.Close()
		return nil, fmt.Errorf("сервер %s не отвечает как SMTP", a.SMTPHost)
	}
	if ok, _ := c.Extension("AUTH"); ok {
		if err := c.Auth(sasl.NewPlainClient("", a.Login, a.password)); err != nil {
			c.Close()
			return nil, ErrAuth
		}
	}
	return c, nil
}

func (s *Service) checkSMTP(ctx context.Context, a *Account) error {
	c, err := s.smtpClient(ctx, a)
	if err != nil {
		return err
	}
	return c.Quit()
}

// Outgoing — новое письмо.
type Outgoing struct {
	To         []string  `json:"to"`
	Cc         []string  `json:"cc"`
	Bcc        []string  `json:"bcc"`
	Subject    string    `json:"subject"`
	Text       string    `json:"text"`
	InReplyTo  string    `json:"in_reply_to"`
	References []string  `json:"references"`
	Files      []OutFile `json:"-"`
}

type OutFile struct {
	Name string
	Mime string
	Data []byte
}

func parseList(list []string) ([]*gomail.Address, error) {
	var out []*gomail.Address
	for _, item := range list {
		for _, raw := range strings.FieldsFunc(item, func(r rune) bool { return r == ',' || r == ';' }) {
			if raw = strings.TrimSpace(raw); raw == "" {
				continue
			}
			a, err := mail.ParseAddress(raw)
			if err != nil {
				return nil, fmt.Errorf("неверный адрес «%s»", raw)
			}
			out = append(out, (*gomail.Address)(a))
		}
	}
	return out, nil
}

// build собирает MIME-письмо. Возвращает текст письма и список адресов для конверта SMTP.
func build(a *Account, m *Outgoing, now time.Time) ([]byte, []string, error) {
	to, err := parseList(m.To)
	if err != nil {
		return nil, nil, err
	}
	cc, err := parseList(m.Cc)
	if err != nil {
		return nil, nil, err
	}
	bcc, err := parseList(m.Bcc)
	if err != nil {
		return nil, nil, err
	}
	all := append(append(append([]*gomail.Address{}, to...), cc...), bcc...)
	if len(all) == 0 {
		return nil, nil, ErrNoRecipients
	}
	if len(all) > MaxRecipients {
		return nil, nil, ErrTooMany
	}
	if utf8.RuneCountInString(m.Subject) > 500 || strings.ContainsAny(m.Subject, "\r\n") {
		return nil, nil, errors.New("тема — одна строка до 500 символов")
	}
	var h gomail.Header
	h.SetDate(now)
	h.SetAddressList("From", []*gomail.Address{{Name: a.Name, Address: a.Email}})
	h.SetAddressList("To", to)
	if len(cc) > 0 {
		h.SetAddressList("Cc", cc)
	}
	h.SetSubject(m.Subject)
	if err := h.GenerateMessageIDWithHostname(domainOf(a.Email)); err != nil {
		return nil, nil, err
	}
	if id := strings.Trim(m.InReplyTo, "<> "); id != "" && !strings.ContainsAny(id, "\r\n") {
		h.SetMsgIDList("In-Reply-To", []string{id})
		refs := []string{}
		for _, r := range m.References {
			if r = strings.Trim(r, "<> "); r != "" && !strings.ContainsAny(r, "\r\n") {
				refs = append(refs, r)
			}
		}
		h.SetMsgIDList("References", append(refs, id))
	}
	h.Set("User-Agent", "CloudBox")

	var buf bytes.Buffer
	text := strings.ReplaceAll(strings.ReplaceAll(m.Text, "\r\n", "\n"), "\n", "\r\n")
	if len(m.Files) == 0 {
		w, err := gomail.CreateSingleInlineWriter(&buf, h)
		if err != nil {
			return nil, nil, err
		}
		io.WriteString(w, text)
		w.Close()
	} else {
		mw, err := gomail.CreateWriter(&buf, h)
		if err != nil {
			return nil, nil, err
		}
		tw, err := mw.CreateInline()
		if err != nil {
			return nil, nil, err
		}
		var th gomail.InlineHeader
		th.Set("Content-Type", "text/plain; charset=utf-8")
		pw, err := tw.CreatePart(th)
		if err != nil {
			return nil, nil, err
		}
		io.WriteString(pw, text)
		pw.Close()
		tw.Close()
		for _, f := range m.Files {
			var ah gomail.AttachmentHeader
			mt := f.Mime
			if mt == "" {
				mt = "application/octet-stream"
			}
			ah.Set("Content-Type", mt)
			ah.SetFilename(f.Name)
			aw, err := mw.CreateAttachment(ah)
			if err != nil {
				return nil, nil, err
			}
			aw.Write(f.Data)
			aw.Close()
		}
		mw.Close()
	}
	if buf.Len() > MaxSendBytes {
		return nil, nil, ErrSendTooBig
	}
	rcpt := make([]string, len(all))
	for i, x := range all {
		rcpt[i] = x.Address
	}
	return buf.Bytes(), rcpt, nil
}

func domainOf(email string) string {
	if i := strings.LastIndex(email, "@"); i >= 0 {
		return email[i+1:]
	}
	return "cloudbox.local"
}

// Send отправляет письмо и кладёт копию в «Отправленные».
// Gmail сохраняет отправленное сам — для него копию не дублируем.
func (s *Service) Send(ctx context.Context, a *Account, m *Outgoing) error {
	msg, rcpt, err := build(a, m, time.Now())
	if err != nil {
		return err
	}
	c, err := s.smtpClient(ctx, a)
	if err != nil {
		return err
	}
	if err := c.SendMail(a.Email, rcpt, bytes.NewReader(msg)); err != nil {
		c.Close()
		return fmt.Errorf("сервер не принял письмо: %v", err)
	}
	c.Quit()
	if !strings.HasSuffix(a.SMTPHost, "gmail.com") {
		if err := s.appendSent(context.WithoutCancel(ctx), a, msg); err != nil {
			return fmt.Errorf("письмо отправлено, но не сохранено в «Отправленных»: %v", err)
		}
	}
	return nil
}
