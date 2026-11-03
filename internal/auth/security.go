package auth

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"regexp"
	"strconv"
	"strings"
	"time"

	"github.com/andrk774/cloudbox/internal/secure"
	"github.com/lib/pq"
)

var ErrBadCode = errors.New("неверный код подтверждения")

// ---------- сессии и журнал входов ----------

type Session struct {
	ID        int64     `json:"id"`
	UserAgent string    `json:"user_agent"`
	IP        string    `json:"ip"`
	Method    string    `json:"method"`
	CreatedAt time.Time `json:"created_at"`
	LastSeen  time.Time `json:"last_seen"`
	Current   bool      `json:"current"`
}

func (s *Service) ListSessions(ctx context.Context, uid int64, currentToken string) ([]Session, error) {
	cur := secure.SHA256Hex(currentToken)
	rows, err := s.DB.QueryContext(ctx, `SELECT id, user_agent, ip, method, created_at, last_seen, token_hash=$2
		FROM sessions WHERE user_id=$1 AND expires_at > now() ORDER BY last_seen DESC`, uid, cur)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := []Session{}
	for rows.Next() {
		var x Session
		if err := rows.Scan(&x.ID, &x.UserAgent, &x.IP, &x.Method, &x.CreatedAt, &x.LastSeen, &x.Current); err != nil {
			return nil, err
		}
		out = append(out, x)
	}
	return out, rows.Err()
}

func (s *Service) DeleteSessionByID(ctx context.Context, uid, id int64) error {
	res, err := s.DB.ExecContext(ctx, `DELETE FROM sessions WHERE id=$1 AND user_id=$2`, id, uid)
	if err != nil {
		return err
	}
	if n, _ := res.RowsAffected(); n == 0 {
		return ErrNotFound
	}
	return nil
}

func (s *Service) DeleteOtherSessions(ctx context.Context, uid int64, currentToken string) error {
	_, err := s.DB.ExecContext(ctx, `DELETE FROM sessions WHERE user_id=$1 AND token_hash<>$2`, uid, secure.SHA256Hex(currentToken))
	return err
}

type LoginEvent struct {
	ID        int64     `json:"id"`
	Username  string    `json:"username"`
	IP        string    `json:"ip"`
	UserAgent string    `json:"user_agent"`
	Success   bool      `json:"success"`
	Method    string    `json:"method"`
	At        time.Time `json:"at"`
}

// LogLogin пишет попытку входа. uid может быть 0, если пользователь не найден.
func (s *Service) LogLogin(ctx context.Context, uid int64, username, ip, ua string, ok bool, method string) {
	var u *int64
	if uid > 0 {
		u = &uid
	} else if username != "" {
		var id int64
		if s.DB.QueryRowContext(ctx, `SELECT id FROM users WHERE lower(username)=lower($1)`, username).Scan(&id) == nil {
			u = &id
		}
	}
	if len(ua) > 200 {
		ua = ua[:200]
	}
	if len(username) > 64 {
		username = username[:64]
	}
	s.DB.ExecContext(context.WithoutCancel(ctx), `INSERT INTO login_events (user_id, username, ip, user_agent, success, method)
		VALUES ($1,$2,$3,$4,$5,$6)`, u, username, ip, ua, ok, method)
}

func (s *Service) ListLogins(ctx context.Context, uid int64, limit int) ([]LoginEvent, error) {
	rows, err := s.DB.QueryContext(ctx, `SELECT id, username, ip, user_agent, success, method, at FROM login_events
		WHERE user_id=$1 ORDER BY at DESC LIMIT $2`, uid, limit)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := []LoginEvent{}
	for rows.Next() {
		var e LoginEvent
		if err := rows.Scan(&e.ID, &e.Username, &e.IP, &e.UserAgent, &e.Success, &e.Method, &e.At); err != nil {
			return nil, err
		}
		out = append(out, e)
	}
	return out, rows.Err()
}

// ---------- двухфакторная аутентификация (TOTP) ----------

// SetupTOTP генерирует новый секрет (2FA включится только после подтверждения кодом).
func (s *Service) SetupTOTP(ctx context.Context, uid int64) (string, error) {
	sec := secure.NewTOTPSecret()
	_, err := s.DB.ExecContext(ctx, `UPDATE users SET totp_secret=$1 WHERE id=$2 AND NOT totp_enabled`, sec, uid)
	return sec, err
}

// EnableTOTP проверяет код из приложения, включает 2FA и возвращает коды восстановления.
func (s *Service) EnableTOTP(ctx context.Context, uid int64, code string) ([]string, error) {
	var sec string
	var enabled bool
	if err := s.DB.QueryRowContext(ctx, `SELECT totp_secret, totp_enabled FROM users WHERE id=$1`, uid).Scan(&sec, &enabled); err != nil {
		return nil, err
	}
	if enabled {
		return nil, errors.New("двухфакторная аутентификация уже включена")
	}
	step, ok := secure.VerifyTOTPStep(sec, code, time.Now())
	if sec == "" || !ok {
		return nil, ErrBadCode
	}
	codes := secure.RecoveryCodes(10)
	hashes := make([]string, len(codes))
	for i, c := range codes {
		hashes[i] = secure.SHA256Hex(c)
	}
	// Код подтверждения сразу считается использованным — им же нельзя войти.
	if _, err := s.DB.ExecContext(ctx, `UPDATE users SET totp_enabled=TRUE, recovery_codes=$1, totp_last_step=$3 WHERE id=$2`,
		pq.Array(hashes), uid, step); err != nil {
		return nil, err
	}
	s.flushBasic(uid)
	return codes, nil
}

func (s *Service) DisableTOTP(ctx context.Context, uid int64) error {
	_, err := s.DB.ExecContext(ctx, `UPDATE users SET totp_enabled=FALSE, totp_secret='', recovery_codes='{}', totp_last_step=0 WHERE id=$1`, uid)
	s.flushBasic(uid)
	return err
}

// CheckSecondFactor принимает код TOTP или одноразовый код восстановления (он сгорает).
func (s *Service) CheckSecondFactor(ctx context.Context, uid int64, code string) (bool, error) {
	var sec string
	var codes []string
	if err := s.DB.QueryRowContext(ctx, `SELECT totp_secret, recovery_codes FROM users WHERE id=$1 AND totp_enabled`, uid).
		Scan(&sec, pq.Array(&codes)); err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			return false, nil
		}
		return false, err
	}
	if step, ok := secure.VerifyTOTPStep(sec, code, time.Now()); ok {
		// Принимаем код, только если его интервал новее последнего принятого. Условие в UPDATE делает
		// проверку атомарной: из двух одновременных запросов с одним кодом пройдёт только один.
		return s.consume(ctx, `UPDATE users SET totp_last_step=$1 WHERE id=$2 AND totp_last_step < $1`, step, uid)
	}
	h := secure.SHA256Hex(secure.NormalizeRecovery(code))
	for _, c := range codes {
		if secure.Equal(c, h) {
			// Код восстановления сгорает атомарно: повторный (в т.ч. параллельный) запрос его уже не найдёт.
			return s.consume(ctx, `UPDATE users SET recovery_codes=array_remove(recovery_codes, $1)
				WHERE id=$2 AND $1 = ANY(recovery_codes)`, h, uid)
		}
	}
	return false, nil
}

// consume выполняет условный UPDATE и сообщает, затронул ли он строку.
func (s *Service) consume(ctx context.Context, q string, args ...any) (bool, error) {
	res, err := s.DB.ExecContext(ctx, q, args...)
	if err != nil {
		return false, err
	}
	n, err := res.RowsAffected()
	return n > 0, err
}

func (s *Service) RecoveryCodesLeft(ctx context.Context, uid int64) int {
	var n int
	s.DB.QueryRowContext(ctx, `SELECT cardinality(recovery_codes) FROM users WHERE id=$1`, uid).Scan(&n)
	return n
}

// Ticket — подписанный билет между первым (пароль) и вторым (код) шагом входа, живёт 5 минут.
func (s *Service) Ticket(uid int64) string {
	exp := time.Now().Add(5 * time.Minute).Unix()
	msg := fmt.Sprintf("%d.%d", uid, exp)
	return msg + "." + secure.HMAC(s.Secret, "2fa|"+msg)
}

func (s *Service) CheckTicket(t string) (int64, bool) {
	parts := strings.Split(t, ".")
	if len(parts) != 3 {
		return 0, false
	}
	msg := parts[0] + "." + parts[1]
	if !secure.Equal(parts[2], secure.HMAC(s.Secret, "2fa|"+msg)) {
		return 0, false
	}
	uid, err1 := strconv.ParseInt(parts[0], 10, 64)
	exp, err2 := strconv.ParseInt(parts[1], 10, 64)
	if err1 != nil || err2 != nil || time.Now().Unix() > exp {
		return 0, false
	}
	return uid, true
}

// ---------- вход через OIDC ----------

var notUsernameChars = regexp.MustCompile(`[^a-zA-Z0-9_.-]+`)

// OIDCUser находит пользователя по sub; при autoCreate создаёт нового с именем из preferred_username.
// Если linkByName — существующий локальный пользователь с таким же именем привязывается к sub.
func (s *Service) OIDCUser(ctx context.Context, sub, username, email string, autoCreate, linkByName bool) (*User, error) {
	u, err := scanUser(s.DB.QueryRowContext(ctx, `SELECT `+userCols+` FROM users WHERE oidc_sub=$1`, sub))
	if err == nil {
		return u, nil
	}
	if !errors.Is(err, ErrNotFound) {
		return nil, err
	}
	name := notUsernameChars.ReplaceAllString(username, "")
	if len(name) < 3 {
		if i := strings.Index(email, "@"); i > 0 {
			name = notUsernameChars.ReplaceAllString(email[:i], "")
		}
	}
	if len(name) > 32 {
		name = name[:32]
	}
	if linkByName && name != "" {
		res, err := s.DB.ExecContext(ctx, `UPDATE users SET oidc_sub=$1 WHERE lower(username)=lower($2) AND oidc_sub IS NULL`, sub, name)
		if err != nil {
			return nil, err
		}
		if n, _ := res.RowsAffected(); n > 0 {
			return scanUser(s.DB.QueryRowContext(ctx, `SELECT `+userCols+` FROM users WHERE oidc_sub=$1`, sub))
		}
	}
	if !autoCreate {
		return nil, errors.New("учётная запись не найдена — попросите администратора создать её")
	}
	if len(name) < 3 {
		name = "user" + secure.Token(4)
	}
	base := name
	for i := 2; i < 50; i++ {
		// случайный пароль: войти можно только через OIDC, пока пользователь не задаст свой
		u, err = s.CreateUser(ctx, name, secure.Token(24), false, -1)
		if errors.Is(err, ErrUserExists) {
			name = fmt.Sprintf("%s%d", base, i)
			continue
		}
		if err != nil {
			return nil, err
		}
		if _, err := s.DB.ExecContext(ctx, `UPDATE users SET oidc_sub=$1, email=$2 WHERE id=$3`, sub, email, u.ID); err != nil {
			return nil, err
		}
		return s.GetUser(ctx, u.ID)
	}
	return nil, ErrUserExists
}
