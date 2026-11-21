package auth

import (
	"context"
	"database/sql"
	"errors"
	"net/mail"
	"strings"
	"time"

	"github.com/andrk774/cloudbox/internal/secure"
)

var ErrAccountToken = errors.New("ссылка недействительна или истекла — запросите новую")

func RecoveryEmail(raw string) (string, error) {
	raw = strings.TrimSpace(raw)
	a, err := mail.ParseAddress(raw)
	if err != nil || a.Address != raw || strings.ContainsAny(raw, "\r\n") || len(raw) > 254 {
		return "", errors.New("укажите корректный email без имени получателя")
	}
	return raw, nil
}

func (s *Service) GetRecoveryEmail(ctx context.Context, uid int64) (string, error) {
	var email string
	err := s.DB.QueryRowContext(ctx, `SELECT recovery_email FROM users WHERE id=$1`, uid).Scan(&email)
	return email, err
}

// Tokens are bound to the password and (for reset) the verified email at issuance.
// Lock the user first in both issuance and redemption to serialize concurrent requests.
func (s *Service) IssueAccountToken(ctx context.Context, uid int64, purpose, email string) (string, error) {
	if purpose != "verify" && purpose != "reset" {
		return "", ErrAccountToken
	}
	tx, err := s.DB.BeginTx(ctx, nil)
	if err != nil {
		return "", err
	}
	defer tx.Rollback()
	var hash, current string
	if err = tx.QueryRowContext(ctx, `SELECT password_hash, recovery_email FROM users WHERE id=$1 FOR UPDATE`, uid).Scan(&hash, &current); err != nil {
		return "", err
	}
	if purpose == "reset" && (current == "" || current != email) {
		return "", ErrAccountToken
	}
	token := secure.Token(32)
	_, err = tx.ExecContext(ctx, `INSERT INTO account_tokens(token_hash,user_id,purpose,email,password_hash,expires_at)
 VALUES($1,$2,$3,$4,$5,$6) ON CONFLICT(user_id,purpose) DO UPDATE SET token_hash=EXCLUDED.token_hash,
 email=EXCLUDED.email,password_hash=EXCLUDED.password_hash,expires_at=EXCLUDED.expires_at`, secure.SHA256Hex(token), uid, purpose, email, hash, time.Now().Add(30*time.Minute))
	if err != nil {
		return "", err
	}
	return token, tx.Commit()
}

func (s *Service) RedeemAccountToken(ctx context.Context, token, purpose, password string) error {
	if len(token) != 43 || (purpose != "reset" && purpose != "verify") {
		return ErrAccountToken
	}
	var newHash string
	var err error
	if purpose == "reset" {
		if len(password) < 8 || len(password) > 1024 {
			return ErrWeakPassword
		}
		newHash, err = secure.HashPassword(password)
		if err != nil {
			return err
		}
	}
	tx, err := s.DB.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer tx.Rollback()
	th := secure.SHA256Hex(token)
	var uid int64
	err = tx.QueryRowContext(ctx, `SELECT user_id FROM account_tokens WHERE token_hash=$1 AND purpose=$2`, th, purpose).Scan(&uid)
	if errors.Is(err, sql.ErrNoRows) {
		return ErrAccountToken
	}
	if err != nil {
		return err
	}
	var currentHash, currentEmail string
	if err = tx.QueryRowContext(ctx, `SELECT password_hash,recovery_email FROM users WHERE id=$1 FOR UPDATE`, uid).Scan(&currentHash, &currentEmail); err != nil {
		return err
	}
	var email string
	err = tx.QueryRowContext(ctx, `DELETE FROM account_tokens WHERE token_hash=$1 AND purpose=$2 AND expires_at>now()
 AND password_hash=$3 AND ($2='verify' OR email=$4) RETURNING email`, th, purpose, currentHash, currentEmail).Scan(&email)
	if errors.Is(err, sql.ErrNoRows) {
		return ErrAccountToken
	}
	if err != nil {
		return err
	}
	if purpose == "verify" {
		_, err = tx.ExecContext(ctx, `UPDATE users SET recovery_email=$1 WHERE id=$2`, email, uid)
	} else {
		_, err = tx.ExecContext(ctx, `UPDATE users SET password_hash=$1 WHERE id=$2`, newHash, uid)
		if err == nil {
			_, err = tx.ExecContext(ctx, `DELETE FROM sessions WHERE user_id=$1`, uid)
		}
		if err == nil {
			_, err = tx.ExecContext(ctx, `DELETE FROM app_passwords WHERE user_id=$1`, uid)
		}
	}
	if err != nil {
		return err
	}
	if _, err = tx.ExecContext(ctx, `DELETE FROM account_tokens WHERE user_id=$1`, uid); err != nil {
		return err
	}
	if err = tx.Commit(); err == nil {
		s.flushBasic(uid)
	}
	return err
}
