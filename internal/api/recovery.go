package api

import (
	"context"
	"crypto/tls"
	"errors"
	"fmt"
	"log/slog"
	"net"
	"net/http"
	"net/smtp"
	"net/url"
	"strings"
	"time"

	"github.com/andrk774/cloudbox/internal/auth"
	"github.com/andrk774/cloudbox/internal/secure"
)

func (s *Server) recoveryEnabled() bool {
	u, err := url.Parse(s.Cfg.PublicURL)
	_, fromErr := auth.RecoveryEmail(s.Cfg.SMTPFrom)
	return s.Cfg.SMTPHost != "" && fromErr == nil && err == nil && u.Host != "" &&
		(u.Scheme == "https" || u.Scheme == "http") && u.RawQuery == "" && u.Fragment == "" && u.User == nil
}

func (s *Server) getRecovery(w http.ResponseWriter, r *http.Request) {
	email, err := s.Auth.GetRecoveryEmail(r.Context(), userFrom(r).ID)
	if err != nil {
		fail(w, err)
		return
	}
	writeJSON(w, 200, map[string]any{"enabled": s.recoveryEnabled(), "email": email})
}

func (s *Server) setRecovery(w http.ResponseWriter, r *http.Request) {
	if !s.recoveryEnabled() {
		writeErr(w, 503, "администратор ещё не настроил отправку писем")
		return
	}
	if !s.allow(r, "recovery-email", 5, time.Hour) {
		writeErr(w, 429, "слишком много попыток")
		return
	}
	var req struct {
		Email    string `json:"email"`
		Password string `json:"password"`
		Code     string `json:"code"`
	}
	if err := readJSON(r, &req); err != nil {
		writeErr(w, 400, err.Error())
		return
	}
	email, err := auth.RecoveryEmail(req.Email)
	if err != nil {
		writeErr(w, 400, err.Error())
		return
	}
	u := userFrom(r)
	if _, err = s.Auth.Authenticate(r.Context(), u.Username, req.Password); err != nil {
		fail(w, err)
		return
	}
	if u.TOTP {
		good, err := s.Auth.CheckSecondFactor(r.Context(), u.ID, req.Code)
		if err != nil {
			fail(w, err)
			return
		}
		if !good {
			fail(w, auth.ErrBadCode)
			return
		}
	}
	token, err := s.Auth.IssueAccountToken(r.Context(), u.ID, "verify", email)
	if err != nil {
		fail(w, err)
		return
	}
	if err = s.sendRecoveryMail(r.Context(), email, "verify", token); err != nil {
		slog.Error("recovery email delivery failed")
		writeErr(w, 502, "не удалось отправить письмо — проверьте настройки SMTP")
		return
	}
	okJSON(w)
}

func (s *Server) forgotPassword(w http.ResponseWriter, r *http.Request) {
	if !s.recoveryEnabled() {
		writeErr(w, 503, "восстановление через email не настроено")
		return
	}
	if !s.allow(r, "forgot", 5, time.Hour) {
		writeErr(w, 429, "слишком много попыток")
		return
	}
	var req struct {
		Username string `json:"username"`
	}
	if err := readJSON(r, &req); err != nil {
		writeErr(w, 400, err.Error())
		return
	}
	name := strings.ToLower(strings.TrimSpace(req.Username))
	// Bound background delivery work; response never reveals whether the account exists.
	if len(name) > 32 || !s.lim.Allow("forgot-user:"+secure.SHA256Hex(name), 3, time.Hour) || !s.lim.Allow("forgot-global", 100, time.Hour) {
		okJSON(w)
		return
	}
	go func() {
		ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
		defer cancel()
		var uid int64
		var email string
		err := s.DB.QueryRowContext(ctx, `SELECT id,recovery_email FROM users WHERE lower(username)=$1 AND recovery_email<>'' AND ($2 OR is_admin) AND oidc_sub IS NULL`, name, s.Cfg.PasswordLogin).Scan(&uid, &email)
		if err != nil {
			return
		}
		token, err := s.Auth.IssueAccountToken(ctx, uid, "reset", email)
		if err == nil {
			err = s.sendRecoveryMail(ctx, email, "reset", token)
		}
		if err != nil {
			slog.Error("password recovery delivery failed")
		}
	}()
	okJSON(w)
}

func (s *Server) redeemRecovery(w http.ResponseWriter, r *http.Request) {
	if !s.allow(r, "redeem-recovery", 15, 15*time.Minute) {
		writeErr(w, 429, "слишком много попыток")
		return
	}
	var req struct {
		Token    string `json:"token"`
		Purpose  string `json:"purpose"`
		Password string `json:"password"`
	}
	if err := readJSON(r, &req); err != nil {
		writeErr(w, 400, err.Error())
		return
	}
	if err := s.Auth.RedeemAccountToken(r.Context(), req.Token, req.Purpose, req.Password); err != nil {
		if errors.Is(err, auth.ErrAccountToken) || errors.Is(err, auth.ErrWeakPassword) {
			writeErr(w, 400, err.Error())
		} else {
			fail(w, err)
		}
		return
	}
	okJSON(w)
}

// System SMTP is configured by the administrator; never use a user's mailbox credentials.
// Require verified TLS for both implicit TLS (465) and STARTTLS (other ports).
func (s *Server) sendRecoveryMail(ctx context.Context, to, purpose, token string) error {
	if _, err := auth.RecoveryEmail(to); err != nil {
		return err
	}
	address := net.JoinHostPort(s.Cfg.SMTPHost, s.Cfg.SMTPPort)
	d := &net.Dialer{Timeout: 10 * time.Second}
	conn, err := d.DialContext(ctx, "tcp", address)
	if err != nil {
		return err
	}
	defer conn.Close()
	deadline := time.Now().Add(25 * time.Second)
	if dl, ok := ctx.Deadline(); ok && dl.Before(deadline) {
		deadline = dl
	}
	conn.SetDeadline(deadline)
	tlsConfig := &tls.Config{ServerName: s.Cfg.SMTPHost, MinVersion: tls.VersionTLS12}
	if s.Cfg.SMTPPort == "465" {
		conn = tls.Client(conn, tlsConfig)
	}
	c, err := smtp.NewClient(conn, s.Cfg.SMTPHost)
	if err != nil {
		return err
	}
	defer c.Close()
	if s.Cfg.SMTPPort != "465" {
		if err = c.StartTLS(tlsConfig); err != nil {
			return err
		}
	}
	if s.Cfg.SMTPUser != "" {
		if err = c.Auth(smtp.PlainAuth("", s.Cfg.SMTPUser, s.Cfg.SMTPPassword, s.Cfg.SMTPHost)); err != nil {
			return err
		}
	}
	if err = c.Mail(s.Cfg.SMTPFrom); err != nil {
		return err
	}
	if err = c.Rcpt(to); err != nil {
		return err
	}
	w, err := c.Data()
	if err != nil {
		return err
	}
	link := s.Cfg.PublicURL + "/#/recovery?purpose=" + purpose + "&token=" + url.QueryEscape(token)
	_, err = fmt.Fprintf(w, "From: %s\r\nTo: %s\r\nSubject: CloudBox account confirmation\r\nMIME-Version: 1.0\r\nContent-Type: text/plain; charset=UTF-8\r\n\r\n%s\r\n\r\nСсылка CloudBox действует 30 минут. Если вы не запрашивали её, проигнорируйте письмо.\r\n", s.Cfg.SMTPFrom, to, link)
	if err != nil {
		return err
	}
	if err = w.Close(); err != nil {
		return err
	}
	return c.Quit()
}
