package mail

import (
	"context"
	"errors"
	"net"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/andrk774/cloudbox/internal/mail/mailtest"
)

func port(t *testing.T, addr string) int {
	_, p, err := net.SplitHostPort(addr)
	if err != nil {
		t.Fatal(err)
	}
	n, _ := strconv.Atoi(p)
	return n
}

func setup(t *testing.T) (*Service, *Account, *mailtest.Server) {
	t.Helper()
	srv, err := mailtest.Start("127.0.0.1:0", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(srv.Close)
	ip, sp := port(t, srv.IMAPAddr), port(t, srv.SMTPAddr)
	s := NewService(nil, &Dialer{AllowPrivate: true, ExtraPorts: []int{ip, sp}}, "test-secret")
	a := &Account{Email: "me@example.org", Name: "Я", IMAPHost: "127.0.0.1", IMAPPort: ip, IMAPSec: SecNone,
		SMTPHost: "127.0.0.1", SMTPPort: sp, SMTPSec: SecNone, Login: mailtest.User, password: mailtest.Password}
	return s, a, srv
}

// Письмо из старого почтового клиента: текст в windows-1251, HTML, встроенная картинка и вложение
// с русским именем в кодировке RFC 2231.
const legacyMsg = "From: =?utf-8?B?0JzQsNGI0LA=?= <masha@example.org>\r\n" +
	"To: me@example.org\r\n" +
	"Subject: =?utf-8?B?0J7RgtGH0ZHRgiDQviDQv9C+0YXQvtC00LU=?=\r\n" +
	"Date: Mon, 02 Nov 2026 10:00:00 +0500\r\n" +
	"Message-ID: <abc@example.org>\r\n" +
	"MIME-Version: 1.0\r\n" +
	"Content-Type: multipart/mixed; boundary=OUT\r\n\r\n" +
	"--OUT\r\n" +
	"Content-Type: multipart/related; boundary=REL\r\n\r\n" +
	"--REL\r\n" +
	"Content-Type: multipart/alternative; boundary=ALT\r\n\r\n" +
	"--ALT\r\n" +
	"Content-Type: text/plain; charset=windows-1251\r\n" +
	"Content-Transfer-Encoding: 8bit\r\n\r\n" +
	"\xcf\xf0\xe8\xe2\xe5\xf2\r\n" + // «Привет» в windows-1251
	"--ALT\r\n" +
	"Content-Type: text/html; charset=utf-8\r\n\r\n" +
	"<p>Привет <img src=\"cid:pic1\"></p>\r\n" +
	"--ALT--\r\n" +
	"--REL\r\n" +
	"Content-Type: image/png\r\n" +
	"Content-ID: <pic1>\r\n" +
	"Content-Transfer-Encoding: base64\r\n\r\n" +
	"iVBORw0KGgo=\r\n" +
	"--REL--\r\n" +
	"--OUT\r\n" +
	"Content-Type: text/plain; charset=utf-8\r\n" +
	"Content-Disposition: attachment; filename*=utf-8''%D0%BE%D1%82%D1%87%D1%91%D1%82.txt\r\n\r\n" +
	"содержимое вложения\r\n" +
	"--OUT--\r\n"

func TestIMAPFlow(t *testing.T) {
	ctx := context.Background()
	s, a, srv := setup(t)
	if err := srv.Deliver("INBOX", nil, []byte(legacyMsg)); err != nil {
		t.Fatal(err)
	}

	folders, err := s.Folders(ctx, a)
	if err != nil {
		t.Fatal(err)
	}
	got := map[string]string{}
	for _, f := range folders {
		got[f.Name] = f.Special
	}
	if got["INBOX"] != "inbox" || got["Sent"] != "sent" || got["Trash"] != "trash" {
		t.Fatalf("special folders: %v", got)
	}
	if _, ok := got["Проекты"]; !ok {
		t.Fatalf("кириллическая папка (modified UTF-7) не найдена: %v", got)
	}
	// Unread не проверяем: тестовый бэкенд go-imap всегда отвечает STATUS UNSEEN 0 («TODO» в memory/mailbox.go)
	if folders[0].Name != "INBOX" || folders[0].Total != 2 {
		t.Fatalf("INBOX first with counters: %+v", folders[0])
	}

	page, err := s.Messages(ctx, a, "INBOX", 0, "")
	if err != nil || page.Total != 2 || len(page.Messages) != 2 {
		t.Fatalf("page: %+v %v", page, err)
	}
	m0 := page.Messages[0] // новые сверху
	if m0.Subject != "Отчёт о походе" || m0.Seen || !m0.Attachments || m0.From[0].Name != "Маша" {
		t.Fatalf("summary: %+v", m0)
	}

	msg, err := s.Message(ctx, a, "INBOX", m0.UID)
	if err != nil {
		t.Fatal(err)
	}
	if strings.TrimSpace(msg.Text) != "Привет" {
		t.Fatalf("windows-1251 text: %q", msg.Text)
	}
	if !strings.Contains(msg.HTML, "cid:pic1") || !strings.HasPrefix(msg.Inline["pic1"], "data:image/png;base64,") {
		t.Fatalf("html/inline: %q %v", msg.HTML, msg.Inline)
	}
	if len(msg.Attachments) != 1 || msg.Attachments[0].Filename != "отчёт.txt" {
		t.Fatalf("attachments: %+v", msg.Attachments)
	}
	if msg.MessageID != "abc@example.org" {
		t.Fatalf("message-id: %q", msg.MessageID)
	}
	page, _ = s.Messages(ctx, a, "INBOX", 0, "")
	if !page.Messages[0].Seen {
		t.Fatal("открытое письмо должно стать прочитанным")
	}

	raw, err := s.Raw(ctx, a, "INBOX", m0.UID)
	if err != nil {
		t.Fatal(err)
	}
	att, data, err := AttachmentData(raw, 0)
	if err != nil || att.Filename != "отчёт.txt" || strings.TrimSpace(string(data)) != "содержимое вложения" {
		t.Fatalf("attachment data: %+v %q %v", att, data, err)
	}
	if _, _, err := AttachmentData(raw, 1); !errors.Is(err, ErrNoAttachment) {
		t.Fatalf("missing attachment: %v", err)
	}

	if err := s.SetFlag(ctx, a, "INBOX", []uint32{m0.UID}, "flagged", true); err != nil {
		t.Fatal(err)
	}
	if err := s.SetFlag(ctx, a, "INBOX", []uint32{m0.UID}, "deleted", true); !errors.Is(err, ErrBadFlag) {
		t.Fatalf("произвольные флаги ставить нельзя: %v", err)
	}
	page, _ = s.Messages(ctx, a, "INBOX", 0, "")
	if !page.Messages[0].Flagged {
		t.Fatal("flagged")
	}

	if err := s.Move(ctx, a, "INBOX", []uint32{m0.UID}, "Проекты"); err != nil {
		t.Fatal(err)
	}
	if srv.Count("INBOX") != 1 || srv.Count("Проекты") != 1 {
		t.Fatalf("move: inbox=%d projects=%d", srv.Count("INBOX"), srv.Count("Проекты"))
	}
	pp, _ := s.Messages(ctx, a, "Проекты", 0, "")
	perm, err := s.Delete(ctx, a, "Проекты", []uint32{pp.Messages[0].UID})
	if err != nil || perm || srv.Count("Trash") != 1 || srv.Count("Проекты") != 0 {
		t.Fatalf("delete → trash: perm=%v err=%v trash=%d", perm, err, srv.Count("Trash"))
	}
	tp, _ := s.Messages(ctx, a, "Trash", 0, "")
	perm, err = s.Delete(ctx, a, "Trash", []uint32{tp.Messages[0].UID})
	if err != nil || !perm || srv.Count("Trash") != 0 {
		t.Fatalf("delete from trash: perm=%v err=%v trash=%d", perm, err, srv.Count("Trash"))
	}

	bad := *a
	bad.password = "wrong"
	if _, err := s.Folders(ctx, &bad); !errors.Is(err, ErrAuth) {
		t.Fatalf("wrong password: %v", err)
	}
}

func TestSend(t *testing.T) {
	ctx := context.Background()
	s, a, srv := setup(t)
	out := &Outgoing{To: []string{"Маша <masha@example.org>"}, Cc: []string{"petya@example.org"}, Bcc: []string{"secret@example.org"},
		Subject: "Привет", Text: "Строка 1\nСтрока 2", InReplyTo: "<abc@example.org>",
		Files: []OutFile{{Name: "план.txt", Mime: "text/plain", Data: []byte("маршрут")}}}
	if err := s.Send(ctx, a, out); err != nil {
		t.Fatal(err)
	}
	if len(srv.Sent) != 1 {
		t.Fatalf("smtp got %d", len(srv.Sent))
	}
	env := srv.Sent[0]
	if env.From != "me@example.org" || strings.Join(env.To, ",") != "masha@example.org,petya@example.org,secret@example.org" {
		t.Fatalf("envelope: %+v", env)
	}
	data := string(env.Data)
	if strings.Contains(data, "secret@example.org") {
		t.Fatal("скрытая копия (Bcc) не должна попадать в заголовки")
	}
	if !strings.Contains(data, "In-Reply-To: <abc@example.org>") || !strings.Contains(data, "References: <abc@example.org>") {
		t.Fatalf("reply headers:\n%s", data)
	}
	if srv.Count("Sent") != 1 {
		t.Fatalf("копия в «Отправленных»: %d", srv.Count("Sent"))
	}
	p, err := Parse(env.Data)
	if err != nil || p.Subject != "Привет" || !strings.Contains(p.Text, "Строка 2") || len(p.Attachments) != 1 || p.Attachments[0].Filename != "план.txt" {
		t.Fatalf("roundtrip: %+v %v", p, err)
	}

	for _, bad := range []*Outgoing{
		{To: nil, Text: "x"},
		{To: []string{"не адрес"}},
		{To: []string{"a@b.c"}, Subject: "тема\r\nBcc: evil@x.y"}, // подстановка заголовка
	} {
		if err := s.Send(ctx, a, bad); err == nil {
			t.Fatalf("должно быть отклонено: %+v", bad)
		}
	}
	if len(srv.Sent) != 1 {
		t.Fatal("отклонённые письма не должны уходить")
	}
}

func TestDialerSafety(t *testing.T) {
	ctx := context.Background()
	strict := &Dialer{Timeout: 2 * time.Second}
	for _, host := range []string{"127.0.0.1", "10.1.2.3", "192.168.0.10", "169.254.169.254", "::1", "100.64.0.1", "0.0.0.0"} {
		if _, err := strict.Dial(ctx, host, 993, SecTLS); !errors.Is(err, ErrBlockedHost) {
			t.Errorf("%s: внутренний адрес должен быть запрещён, получено %v", host, err)
		}
	}
	if _, err := strict.Dial(ctx, "imap.example.org", 22, SecTLS); !errors.Is(err, ErrBadPort) {
		t.Errorf("порт 22: %v", err)
	}
	if _, err := strict.Dial(ctx, "127.0.0.1", 143, SecNone); !errors.Is(err, ErrPlaintext) {
		t.Errorf("без шифрования без AllowPrivate: %v", err)
	}
	loose := &Dialer{AllowPrivate: true, Timeout: 2 * time.Second}
	if _, err := loose.Dial(ctx, "8.8.8.8", 143, SecNone); !errors.Is(err, ErrPlaintext) {
		t.Errorf("пароль открытым текстом в интернет: %v", err)
	}
	if err := strict.CheckSecurity("ssl3"); err == nil {
		t.Error("неизвестный способ защиты")
	}
}

func TestPasswordEncryption(t *testing.T) {
	s := NewService(nil, nil, "secret-1")
	enc, err := s.seal("пароль")
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(string(enc), "пароль") {
		t.Fatal("пароль хранится открытым текстом")
	}
	if p, err := s.open(enc); err != nil || p != "пароль" {
		t.Fatalf("open: %q %v", p, err)
	}
	if _, err := NewService(nil, nil, "secret-2").open(enc); err == nil {
		t.Fatal("другой CB_SECRET не должен расшифровывать")
	}
	if NewService(nil, nil, "").Enabled() {
		t.Fatal("без CB_SECRET почта должна быть отключена")
	}
}

func TestPresets(t *testing.T) {
	p, ok := PresetFor("Ivan@Ya.Ru")
	if !ok || p.IMAPHost != "imap.yandex.ru" || p.SMTPPort != 465 {
		t.Fatalf("yandex: %+v %v", p, ok)
	}
	if p, ok := PresetFor("a@inbox.ru"); !ok || p.IMAPHost != "imap.mail.ru" {
		t.Fatalf("mail.ru alias: %+v", p)
	}
	if _, ok := PresetFor("a@company.local"); ok {
		t.Fatal("неизвестный домен")
	}
}
