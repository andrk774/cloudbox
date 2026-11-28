package auth

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"net/url"
	"os"
	"sync"
	"testing"
	"time"

	dbpkg "github.com/andrk774/cloudbox/internal/db"
	"github.com/andrk774/cloudbox/internal/secure"
)

func TestRecoveryEmail(t *testing.T) {
	for _, bad := range []string{"", "nobody", "Name <a@example.com>", "a@example.com\r\nBcc: b@example.com", "a@example.com,b@example.com"} {
		if _, err := RecoveryEmail(bad); err == nil {
			t.Errorf("accepted %q", bad)
		}
	}
	if got, err := RecoveryEmail(" alice@example.com "); err != nil || got != "alice@example.com" {
		t.Fatalf("%q %v", got, err)
	}
}

func TestRecoveryInvalidTokenWithoutDatabase(t *testing.T) {
	s := &Service{}
	if !errors.Is(s.RedeemAccountToken(context.Background(), "bad", "reset", "longpassword"), ErrAccountToken) {
		t.Fatal("invalid token accepted")
	}
	if !errors.Is(s.RedeemAccountToken(context.Background(), secure.Token(32), "reset", "short"), ErrWeakPassword) {
		t.Fatal("weak password accepted")
	}
}

func recoveryDB(t *testing.T) *sql.DB {
	t.Helper()
	raw := os.Getenv("CB_TEST_DATABASE_URL")
	if raw == "" {
		t.Skip("CB_TEST_DATABASE_URL not set (PostgreSQL integration)")
	}
	admin, err := sql.Open("postgres", raw)
	if err != nil {
		t.Fatal(err)
	}
	schema := fmt.Sprintf("recovery_test_%d", time.Now().UnixNano())
	if _, err = admin.Exec(`CREATE SCHEMA ` + schema); err != nil {
		admin.Close()
		t.Fatal(err)
	}
	t.Cleanup(func() { admin.Exec(`DROP SCHEMA ` + schema + ` CASCADE`); admin.Close() })
	u, err := url.Parse(raw)
	if err != nil {
		t.Fatal(err)
	}
	q := u.Query()
	q.Set("search_path", schema)
	u.RawQuery = q.Encode()
	d, err := dbpkg.Open(context.Background(), u.String())
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { d.Close() })
	return d
}

func TestRecoveryLifecycle(t *testing.T) {
	ctx := context.Background()
	d := recoveryDB(t)
	s := &Service{DB: d}
	u, err := s.CreateUser(ctx, "alice", "old-password", false, 0)
	if err != nil {
		t.Fatal(err)
	}
	issue := func(purpose, email string) string {
		t.Helper()
		token, err := s.IssueAccountToken(ctx, u.ID, purpose, email)
		if err != nil {
			t.Fatal(err)
		}
		return token
	}
	redeem := func(token, purpose, password string) {
		t.Helper()
		if err := s.RedeemAccountToken(ctx, token, purpose, password); err != nil {
			t.Fatal(err)
		}
	}
	old := issue("verify", "old@example.com")
	token := issue("verify", "alice@example.com")
	if err := s.RedeemAccountToken(ctx, old, "verify", ""); !errors.Is(err, ErrAccountToken) {
		t.Fatalf("superseded: %v", err)
	}
	redeem(token, "verify", "")
	if got, _ := s.GetRecoveryEmail(ctx, u.ID); got != "alice@example.com" {
		t.Fatal(got)
	}
	if err := s.RedeemAccountToken(ctx, token, "verify", ""); !errors.Is(err, ErrAccountToken) {
		t.Fatal("replay", err)
	}
	if _, err := s.IssueAccountToken(ctx, u.ID, "reset", "other@example.com"); !errors.Is(err, ErrAccountToken) {
		t.Fatal("unverified email", err)
	}
	token = issue("reset", "alice@example.com")
	if _, err := d.Exec(`UPDATE account_tokens SET expires_at=now()-interval '1 second'`); err != nil {
		t.Fatal(err)
	}
	if err := s.RedeemAccountToken(ctx, token, "reset", "new-password"); !errors.Is(err, ErrAccountToken) {
		t.Fatal("expired", err)
	}
	token = issue("reset", "alice@example.com")
	if err := s.SetPassword(ctx, u.ID, "changed-password"); err != nil {
		t.Fatal(err)
	}
	if err := s.RedeemAccountToken(ctx, token, "reset", "new-password"); !errors.Is(err, ErrAccountToken) {
		t.Fatal("old password token", err)
	}
	session, err := s.CreateSession(ctx, u.ID, "test", "127.0.0.1", "password")
	if err != nil {
		t.Fatal(err)
	}
	_, _, err = s.CreateAppPassword(ctx, u.ID, "phone")
	if err != nil {
		t.Fatal(err)
	}
	if _, err = d.Exec(`UPDATE users SET totp_enabled=true WHERE id=$1`, u.ID); err != nil {
		t.Fatal(err)
	}
	token = issue("reset", "alice@example.com")
	var wg sync.WaitGroup
	results := make(chan error, 2)
	for i := 0; i < 2; i++ {
		wg.Add(1)
		go func() { defer wg.Done(); results <- s.RedeemAccountToken(ctx, token, "reset", "new-password") }()
	}
	wg.Wait()
	close(results)
	successes := 0
	for err := range results {
		if err == nil {
			successes++
		} else if !errors.Is(err, ErrAccountToken) {
			t.Fatal(err)
		}
	}
	if successes != 1 {
		t.Fatalf("concurrent redemptions: %d", successes)
	}
	if _, err := s.UserBySession(ctx, session); !errors.Is(err, ErrNotFound) {
		t.Fatal("old session survives", err)
	}
	if aps, err := s.ListAppPasswords(ctx, u.ID); err != nil || len(aps) != 0 {
		t.Fatal("app passwords survive", err)
	}
	current, err := s.Authenticate(ctx, "alice", "new-password")
	if err != nil || !current.TOTP {
		t.Fatal("password/2FA", err)
	}
	if _, err := s.Authenticate(ctx, "alice", "changed-password"); !errors.Is(err, ErrBadCredentials) {
		t.Fatal("old password works")
	}
}
