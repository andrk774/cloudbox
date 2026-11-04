// Package mailtest — IMAP- и SMTP-сервер в памяти для тестов почты.
//
// Один пользователь: username / password. Письмо, принятое по SMTP, попадает в его INBOX.
// Шифрования нет: тестовый клиент подключается с CB_MAIL_ALLOW_PRIVATE и способом защиты "none".
package mailtest

import (
	"bytes"
	"errors"
	"io"
	"net"
	"sync"
	"time"

	"github.com/emersion/go-imap"
	"github.com/emersion/go-imap/backend/memory"
	imapserver "github.com/emersion/go-imap/server"
	"github.com/emersion/go-sasl"
	"github.com/emersion/go-smtp"
)

const (
	User     = "username"
	Password = "password"
)

type Server struct {
	IMAPAddr string
	SMTPAddr string
	Sent     []Envelope // всё, что принял SMTP

	mu   sync.Mutex
	be   *memory.Backend
	imap *imapserver.Server
	smtp *smtp.Server
}

type Envelope struct {
	From string
	To   []string
	Data []byte
}

// Start поднимает серверы на addr ("127.0.0.1:0" — случайные порты).
// Помимо INBOX создаёт папки Sent, Trash и «Проекты» (кириллица проверяет modified UTF-7).
func Start(imapAddr, smtpAddr string) (*Server, error) {
	s := &Server{be: memory.New()}
	u, err := s.be.Login(nil, User, Password)
	if err != nil {
		return nil, err
	}
	for _, name := range []string{"Sent", "Trash", "Проекты"} {
		if err := u.CreateMailbox(name); err != nil {
			return nil, err
		}
	}

	il, err := net.Listen("tcp", imapAddr)
	if err != nil {
		return nil, err
	}
	s.imap = imapserver.New(s.be)
	s.imap.AllowInsecureAuth = true
	go s.imap.Serve(il)
	s.IMAPAddr = il.Addr().String()

	sl, err := net.Listen("tcp", smtpAddr)
	if err != nil {
		il.Close()
		return nil, err
	}
	s.smtp = smtp.NewServer(smtp.BackendFunc(func(c *smtp.Conn) (smtp.Session, error) { return &session{srv: s}, nil }))
	s.smtp.AllowInsecureAuth = true
	s.smtp.Domain = "localhost"
	s.smtp.ReadTimeout, s.smtp.WriteTimeout = 30*time.Second, 30*time.Second
	go s.smtp.Serve(sl)
	s.SMTPAddr = sl.Addr().String()
	return s, nil
}

// Deliver кладёт письмо в папку пользователя.
func (s *Server) Deliver(folder string, flags []string, raw []byte) error {
	u, err := s.be.Login(nil, User, Password)
	if err != nil {
		return err
	}
	mb, err := u.GetMailbox(folder)
	if err != nil {
		return err
	}
	return mb.CreateMessage(flags, time.Now(), literal(raw))
}

// Count — сколько писем в папке.
func (s *Server) Count(folder string) int {
	u, _ := s.be.Login(nil, User, Password)
	mb, err := u.GetMailbox(folder)
	if err != nil {
		return -1
	}
	return len(mb.(*memory.Mailbox).Messages)
}

func (s *Server) Close() {
	s.imap.Close()
	s.smtp.Close()
}

func literal(b []byte) imap.Literal { return bytes.NewBuffer(b) }

// ---------- SMTP ----------

type session struct {
	srv  *Server
	auth bool
	env  Envelope
}

func (s *session) AuthMechanisms() []string { return []string{sasl.Plain} }

func (s *session) Auth(mech string) (sasl.Server, error) {
	return sasl.NewPlainServer(func(_, user, pass string) error {
		if user != User || pass != Password {
			return errors.New("bad credentials")
		}
		s.auth = true
		return nil
	}), nil
}

func (s *session) Mail(from string, _ *smtp.MailOptions) error {
	if !s.auth {
		return smtp.ErrAuthRequired
	}
	s.env = Envelope{From: from}
	return nil
}

func (s *session) Rcpt(to string, _ *smtp.RcptOptions) error {
	s.env.To = append(s.env.To, to)
	return nil
}

func (s *session) Data(r io.Reader) error {
	b, err := io.ReadAll(r)
	if err != nil {
		return err
	}
	s.env.Data = b
	s.srv.mu.Lock()
	s.srv.Sent = append(s.srv.Sent, s.env)
	s.srv.mu.Unlock()
	return s.srv.Deliver("INBOX", nil, b)
}

func (s *session) Reset()        { s.env = Envelope{} }
func (s *session) Logout() error { return nil }
