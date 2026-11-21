package api

import (
	"bufio"
	"context"
	"fmt"
	"net"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/andrk774/cloudbox/internal/auth"
	"github.com/andrk774/cloudbox/internal/config"
)

func TestRecoveryRejectsInvalidConfiguration(t *testing.T) {
	for _, raw := range []string{"", "javascript:alert(1)", "https://cloud.example/?redirect=other", "https://user:pass@cloud.example", "https://cloud.example/#other"} {
		s := &Server{Cfg: config.Config{PublicURL: raw, SMTPHost: "smtp.example", SMTPFrom: "a@example.com"}}
		if s.recoveryEnabled() {
			t.Errorf("accepted URL %q", raw)
		}
	}
	s := &Server{}
	w := httptest.NewRecorder()
	s.forgotPassword(w, httptest.NewRequest("POST", "/api/auth/forgot", strings.NewReader(`{"username":"alice"}`)))
	if w.Code != 503 {
		t.Fatal(w.Code)
	}
	s.Auth = &auth.Service{}
	w = httptest.NewRecorder()
	s.redeemRecovery(w, httptest.NewRequest("POST", "/api/auth/recovery", strings.NewReader(`{"token":"bad","purpose":"reset","password":"new-password"}`)))
	if w.Code != 400 {
		t.Fatal(w.Code)
	}
}

func TestRecoverySMTPRefusesPlaintext(t *testing.T) {
	listener, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	defer listener.Close()
	commands := make(chan []string, 1)
	go func() {
		c, err := listener.Accept()
		if err != nil {
			commands <- nil
			return
		}
		defer c.Close()
		c.SetDeadline(time.Now().Add(5 * time.Second))
		fmt.Fprint(c, "220 test SMTP\r\n")
		var seen []string
		scanner := bufio.NewScanner(c)
		for scanner.Scan() {
			cmd := scanner.Text()
			seen = append(seen, cmd)
			if strings.HasPrefix(cmd, "EHLO") {
				fmt.Fprint(c, "250-test\r\n250 AUTH PLAIN\r\n")
			} else {
				fmt.Fprint(c, "502 unsupported\r\n")
			}
		}
		commands <- seen
	}()
	host, port, _ := net.SplitHostPort(listener.Addr().String())
	s := &Server{Cfg: config.Config{SMTPHost: host, SMTPPort: port, SMTPUser: "account", SMTPPassword: "secret", SMTPFrom: "from@example.com", PublicURL: "https://cloud.example"}}
	if err := s.sendRecoveryMail(context.Background(), "to@example.com", "reset", "token"); err == nil {
		t.Fatal("unencrypted SMTP accepted")
	}
	for _, cmd := range <-commands {
		if strings.HasPrefix(cmd, "AUTH") || strings.HasPrefix(cmd, "MAIL") || strings.Contains(cmd, "secret") {
			t.Fatalf("sent sensitive command before TLS: %q", cmd)
		}
	}
}
