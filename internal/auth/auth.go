// Package auth — пользователи, сессии и пароли приложений.
package auth

import (
	"context"
	"database/sql"
	"errors"
	"regexp"
	"strings"
	"sync"
	"time"

	"github.com/andrk774/cloudbox/internal/secure"
	"github.com/lib/pq"
)

var (
	ErrBadCredentials = errors.New("неверное имя пользователя или пароль")
	ErrUserExists     = errors.New("пользователь уже существует")
	ErrBadUsername    = errors.New("имя: 3–32 символа, латиница, цифры, точка, дефис, подчёркивание")
	ErrWeakPassword   = errors.New("пароль должен быть не короче 8 символов")
	ErrNotFound       = errors.New("не найдено")
)

const SessionTTL = 30 * 24 * time.Hour

var usernameRe = regexp.MustCompile(`^[a-zA-Z0-9_.-]{3,32}$`)

type User struct {
	ID         int64     `json:"id"`
	Username   string    `json:"username"`
	IsAdmin    bool      `json:"is_admin"`
	QuotaBytes int64     `json:"quota_bytes"`
	CreatedAt  time.Time `json:"created_at"`
	UsedBytes  int64     `json:"used_bytes,omitempty"`
	TOTP       bool      `json:"totp_enabled"`
	Email      string    `json:"email"`
	OIDC       bool      `json:"oidc"`
}

type AppPassword struct {
	ID         int64      `json:"id"`
	Name       string     `json:"name"`
	CreatedAt  time.Time  `json:"created_at"`
	LastUsedAt *time.Time `json:"last_used_at"`
}

type Service struct {
	DB           *sql.DB
	DefaultQuota int64  // байт
	Secret       string // для подписи билетов второго шага входа
	// NoPasswordLogin — вход по основному паролю отключён (CB_PASSWORD_LOGIN=false, только OIDC).
	// Действует и на Basic-auth (WebDAV, CalDAV/CardDAV, API): работают только пароли приложений.
	// Администраторов не касается — как и вход в веб-интерфейс, чтобы не потерять доступ к серверу.
	NoPasswordLogin bool

	mu        sync.Mutex
	basicHits map[string]basicEntry // кэш проверок Basic-auth для WebDAV (PBKDF2 дорогой)
}

type basicEntry struct {
	uid int64
	exp time.Time
}

const userCols = `id, username, is_admin, quota_bytes, created_at, totp_enabled, email, oidc_sub IS NOT NULL`

func scanUser(s interface{ Scan(...any) error }) (*User, error) {
	var u User
	if err := s.Scan(&u.ID, &u.Username, &u.IsAdmin, &u.QuotaBytes, &u.CreatedAt, &u.TOTP, &u.Email, &u.OIDC); err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			return nil, ErrNotFound
		}
		return nil, err
	}
	return &u, nil
}

func (s *Service) CountUsers(ctx context.Context) (int, error) {
	var n int
	err := s.DB.QueryRowContext(ctx, `SELECT count(*) FROM users`).Scan(&n)
	return n, err
}

// CreateUser создаёт пользователя. Самый первый пользователь всегда становится администратором.
func (s *Service) CreateUser(ctx context.Context, username, password string, admin bool, quota int64) (*User, error) {
	username = strings.TrimSpace(username)
	if !usernameRe.MatchString(username) {
		return nil, ErrBadUsername
	}
	if len(password) < 8 {
		return nil, ErrWeakPassword
	}
	hash, err := secure.HashPassword(password)
	if err != nil {
		return nil, err
	}
	if quota < 0 {
		quota = s.DefaultQuota
	}
	u, err := scanUser(s.DB.QueryRowContext(ctx, `INSERT INTO users (username, password_hash, is_admin, quota_bytes)
		VALUES ($1, $2, $3 OR NOT EXISTS (SELECT 1 FROM users), $4) RETURNING `+userCols, username, hash, admin, quota))
	var pe *pq.Error
	if errors.As(err, &pe) && pe.Code == "23505" {
		return nil, ErrUserExists
	}
	return u, err
}

func (s *Service) Authenticate(ctx context.Context, username, password string) (*User, error) {
	var hash string
	var uid int64
	err := s.DB.QueryRowContext(ctx, `SELECT id, password_hash FROM users WHERE lower(username)=lower($1)`, username).Scan(&uid, &hash)
	if errors.Is(err, sql.ErrNoRows) {
		secure.VerifyPassword("pbkdf2-sha256$310000$AAAAAAAAAAAAAAAAAAAAAA$AAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAA", password) // выравниваем время ответа
		return nil, ErrBadCredentials
	}
	if err != nil {
		return nil, err
	}
	if !secure.VerifyPassword(hash, password) {
		return nil, ErrBadCredentials
	}
	return s.GetUser(ctx, uid)
}

func (s *Service) GetUser(ctx context.Context, uid int64) (*User, error) {
	return scanUser(s.DB.QueryRowContext(ctx, `SELECT `+userCols+` FROM users WHERE id=$1`, uid))
}

func (s *Service) ListUsers(ctx context.Context) ([]User, error) {
	rows, err := s.DB.QueryContext(ctx, `SELECT u.id, u.username, u.is_admin, u.quota_bytes, u.created_at, u.totp_enabled, u.email,
		u.oidc_sub IS NOT NULL, COALESCE((SELECT SUM(size) FROM versions v WHERE v.owner_id=u.id),0) FROM users u ORDER BY u.id`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := []User{}
	for rows.Next() {
		var u User
		if err := rows.Scan(&u.ID, &u.Username, &u.IsAdmin, &u.QuotaBytes, &u.CreatedAt, &u.TOTP, &u.Email, &u.OIDC, &u.UsedBytes); err != nil {
			return nil, err
		}
		out = append(out, u)
	}
	return out, rows.Err()
}

func (s *Service) UpdateUser(ctx context.Context, uid int64, quota *int64, admin *bool) error {
	if quota != nil {
		if _, err := s.DB.ExecContext(ctx, `UPDATE users SET quota_bytes=$1 WHERE id=$2`, *quota, uid); err != nil {
			return err
		}
	}
	if admin != nil {
		if _, err := s.DB.ExecContext(ctx, `UPDATE users SET is_admin=$1 WHERE id=$2`, *admin, uid); err != nil {
			return err
		}
	}
	return nil
}

func (s *Service) SetPassword(ctx context.Context, uid int64, password string) error {
	if len(password) < 8 {
		return ErrWeakPassword
	}
	hash, err := secure.HashPassword(password)
	if err != nil {
		return err
	}
	if _, err := s.DB.ExecContext(ctx, `UPDATE users SET password_hash=$1 WHERE id=$2`, hash, uid); err != nil {
		return err
	}
	// Сменили пароль — разлогиниваем все сессии и сбрасываем кэш WebDAV.
	s.DB.ExecContext(ctx, `DELETE FROM sessions WHERE user_id=$1`, uid)
	s.flushBasic(uid)
	return nil
}

// DeleteUser удаляет пользователя; его блобы ставятся в очередь на удаление.
func (s *Service) DeleteUser(ctx context.Context, uid int64) error {
	tx, err := s.DB.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer tx.Rollback()
	if _, err := tx.ExecContext(ctx, `INSERT INTO blob_gc (blob_key) SELECT blob_key FROM versions WHERE owner_id=$1 ON CONFLICT DO NOTHING`, uid); err != nil {
		return err
	}
	if _, err := tx.ExecContext(ctx, `DELETE FROM users WHERE id=$1`, uid); err != nil {
		return err
	}
	s.flushBasic(uid)
	return tx.Commit()
}

// ---------- сессии ----------

func (s *Service) CreateSession(ctx context.Context, uid int64, ua, ip, method string) (string, error) {
	tok := secure.Token(32)
	if len(ua) > 200 {
		ua = ua[:200]
	}
	_, err := s.DB.ExecContext(ctx, `INSERT INTO sessions (token_hash, user_id, user_agent, expires_at, ip, method) VALUES ($1,$2,$3,$4,$5,$6)`,
		secure.SHA256Hex(tok), uid, ua, time.Now().Add(SessionTTL), ip, method)
	return tok, err
}

func (s *Service) UserBySession(ctx context.Context, tok string) (*User, error) {
	if tok == "" {
		return nil, ErrNotFound
	}
	h := secure.SHA256Hex(tok)
	u, err := scanUser(s.DB.QueryRowContext(ctx, `SELECT u.id, u.username, u.is_admin, u.quota_bytes, u.created_at, u.totp_enabled,
		u.email, u.oidc_sub IS NOT NULL
		FROM sessions s JOIN users u ON u.id=s.user_id WHERE s.token_hash=$1 AND s.expires_at > now()`, h))
	if err != nil {
		return nil, err
	}
	// Скользящее продление: не чаще раза в сутки на сессию
	s.DB.ExecContext(ctx, `UPDATE sessions SET expires_at=$1 WHERE token_hash=$2 AND expires_at < $3`,
		time.Now().Add(SessionTTL), h, time.Now().Add(SessionTTL-24*time.Hour))
	s.DB.ExecContext(ctx, `UPDATE sessions SET last_seen=now() WHERE token_hash=$1 AND last_seen < now() - interval '5 minutes'`, h)
	return u, nil
}

func (s *Service) DeleteSession(ctx context.Context, tok string) {
	s.DB.ExecContext(ctx, `DELETE FROM sessions WHERE token_hash=$1`, secure.SHA256Hex(tok))
}

func (s *Service) Cleanup(ctx context.Context) {
	s.DB.ExecContext(ctx, `DELETE FROM sessions WHERE expires_at < now()`)
}

// ---------- пароли приложений и Basic-auth (WebDAV) ----------

func (s *Service) CreateAppPassword(ctx context.Context, uid int64, name string) (string, *AppPassword, error) {
	name = strings.TrimSpace(name)
	if name == "" || len(name) > 64 {
		return "", nil, errors.New("укажите название (до 64 символов)")
	}
	// Формат как у Nextcloud: 5 групп по 5 символов — удобно вводить с телефона
	raw := strings.ToLower(strings.NewReplacer("-", "", "_", "").Replace(secure.Token(24)))
	for len(raw) < 25 {
		raw += strings.ToLower(secure.Token(8))
	}
	plain := raw[0:5] + "-" + raw[5:10] + "-" + raw[10:15] + "-" + raw[15:20] + "-" + raw[20:25]
	hash, err := secure.HashPassword(plain)
	if err != nil {
		return "", nil, err
	}
	var ap AppPassword
	err = s.DB.QueryRowContext(ctx, `INSERT INTO app_passwords (user_id, name, password_hash) VALUES ($1,$2,$3)
		RETURNING id, name, created_at`, uid, name, hash).Scan(&ap.ID, &ap.Name, &ap.CreatedAt)
	return plain, &ap, err
}

func (s *Service) ListAppPasswords(ctx context.Context, uid int64) ([]AppPassword, error) {
	rows, err := s.DB.QueryContext(ctx, `SELECT id, name, created_at, last_used_at FROM app_passwords WHERE user_id=$1 ORDER BY id`, uid)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := []AppPassword{}
	for rows.Next() {
		var ap AppPassword
		var lu sql.NullTime
		if err := rows.Scan(&ap.ID, &ap.Name, &ap.CreatedAt, &lu); err != nil {
			return nil, err
		}
		if lu.Valid {
			ap.LastUsedAt = &lu.Time
		}
		out = append(out, ap)
	}
	return out, rows.Err()
}

func (s *Service) DeleteAppPassword(ctx context.Context, uid, id int64) error {
	res, err := s.DB.ExecContext(ctx, `DELETE FROM app_passwords WHERE id=$1 AND user_id=$2`, id, uid)
	if err != nil {
		return err
	}
	if n, _ := res.RowsAffected(); n == 0 {
		return ErrNotFound
	}
	s.flushBasic(uid)
	return nil
}

// CheckBasic проверяет логин/пароль для WebDAV: основной пароль или пароль приложения.
func (s *Service) CheckBasic(ctx context.Context, username, password string) (*User, error) {
	cacheKey := secure.SHA256Hex(strings.ToLower(username) + "\x00" + password)
	s.mu.Lock()
	if s.basicHits == nil {
		s.basicHits = map[string]basicEntry{}
	}
	e, ok := s.basicHits[cacheKey]
	s.mu.Unlock()
	if ok && time.Now().Before(e.exp) {
		return s.GetUser(ctx, e.uid)
	}

	u, err := s.Authenticate(ctx, username, password)
	if err == nil && (u.TOTP || (s.NoPasswordLogin && !u.IsAdmin)) {
		// При включённой 2FA или входе только через OIDC основной пароль в WebDAV/API не работает —
		// только пароли приложений.
		u, err = nil, ErrBadCredentials
	}
	if errors.Is(err, ErrBadCredentials) {
		u, err = s.checkAppPassword(ctx, username, password)
	}
	if err != nil {
		return nil, err
	}
	s.mu.Lock()
	if len(s.basicHits) > 10000 {
		s.basicHits = map[string]basicEntry{}
	}
	s.basicHits[cacheKey] = basicEntry{uid: u.ID, exp: time.Now().Add(5 * time.Minute)}
	s.mu.Unlock()
	return u, nil
}

func (s *Service) checkAppPassword(ctx context.Context, username, password string) (*User, error) {
	rows, err := s.DB.QueryContext(ctx, `SELECT a.id, a.user_id, a.password_hash FROM app_passwords a
		JOIN users u ON u.id=a.user_id WHERE lower(u.username)=lower($1)`, username)
	if err != nil {
		return nil, err
	}
	type cand struct {
		id, uid int64
		hash    string
	}
	var cands []cand
	for rows.Next() {
		var c cand
		if rows.Scan(&c.id, &c.uid, &c.hash) == nil {
			cands = append(cands, c)
		}
	}
	rows.Close()
	for _, c := range cands {
		if secure.VerifyPassword(c.hash, password) {
			s.DB.ExecContext(ctx, `UPDATE app_passwords SET last_used_at=now() WHERE id=$1`, c.id)
			return s.GetUser(ctx, c.uid)
		}
	}
	return nil, ErrBadCredentials
}

func (s *Service) flushBasic(uid int64) {
	s.mu.Lock()
	for k, e := range s.basicHits {
		if e.uid == uid {
			delete(s.basicHits, k)
		}
	}
	s.mu.Unlock()
}
