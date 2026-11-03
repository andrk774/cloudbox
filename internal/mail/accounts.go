package mail

import (
	"context"
	"crypto/aes"
	"crypto/cipher"
	"crypto/hkdf"
	"crypto/rand"
	"crypto/sha256"
	"database/sql"
	"errors"
	"fmt"
	"net/mail"
	"strconv"
	"strings"
	"time"
)

var (
	ErrNotFound = errors.New("почтовый ящик не найден")
	ErrDisabled = errors.New("почта отключена: задайте постоянный CB_SECRET — им шифруются пароли ящиков")
)

// MaxAccounts — ящиков на пользователя.
const MaxAccounts = 10

type Service struct {
	DB     *sql.DB
	Dialer *Dialer
	key    []byte // ключ шифрования паролей ящиков; nil — почта отключена
}

// NewService. secret — CB_SECRET; пустой (сгенерированный при запуске) отключает почту:
// после перезапуска сохранённые пароли нельзя было бы расшифровать.
func NewService(db *sql.DB, d *Dialer, secret string) *Service {
	s := &Service{DB: db, Dialer: d}
	if secret != "" {
		k, err := hkdf.Key(sha256.New, []byte(secret), nil, "cloudbox-mail-password", 32)
		if err == nil {
			s.key = k
		}
	}
	return s
}

func (s *Service) Enabled() bool { return s != nil && s.key != nil }

// Account — подключённый ящик. Пароль наружу (в JSON) не отдаётся никогда.
type Account struct {
	ID       int64     `json:"id"`
	UserID   int64     `json:"-"`
	Email    string    `json:"email"`
	Name     string    `json:"name"`
	IMAPHost string    `json:"imap_host"`
	IMAPPort int       `json:"imap_port"`
	IMAPSec  string    `json:"imap_sec"`
	SMTPHost string    `json:"smtp_host"`
	SMTPPort int       `json:"smtp_port"`
	SMTPSec  string    `json:"smtp_sec"`
	Login    string    `json:"login"`
	Created  time.Time `json:"created_at"`
	password string
}

// AccountInput — данные для подключения ящика.
type AccountInput struct {
	Email    string `json:"email"`
	Name     string `json:"name"`
	IMAPHost string `json:"imap_host"`
	IMAPPort int    `json:"imap_port"`
	IMAPSec  string `json:"imap_sec"`
	SMTPHost string `json:"smtp_host"`
	SMTPPort int    `json:"smtp_port"`
	SMTPSec  string `json:"smtp_sec"`
	Login    string `json:"login"`
	Password string `json:"password"`
}

func (s *Service) seal(plain string) ([]byte, error) {
	block, err := aes.NewCipher(s.key)
	if err != nil {
		return nil, err
	}
	gcm, err := cipher.NewGCM(block)
	if err != nil {
		return nil, err
	}
	nonce := make([]byte, gcm.NonceSize())
	if _, err := rand.Read(nonce); err != nil {
		return nil, err
	}
	return gcm.Seal(nonce, nonce, []byte(plain), nil), nil
}

func (s *Service) open(enc []byte) (string, error) {
	block, err := aes.NewCipher(s.key)
	if err != nil {
		return "", err
	}
	gcm, err := cipher.NewGCM(block)
	if err != nil {
		return "", err
	}
	if len(enc) < gcm.NonceSize() {
		return "", errors.New("повреждённый пароль ящика")
	}
	p, err := gcm.Open(nil, enc[:gcm.NonceSize()], enc[gcm.NonceSize():], nil)
	if err != nil {
		return "", errors.New("пароль ящика не расшифровывается — изменился CB_SECRET? Подключите ящик заново")
	}
	return string(p), nil
}

func (in *AccountInput) normalize() error {
	in.Email = strings.TrimSpace(in.Email)
	a, err := mail.ParseAddress(in.Email)
	if err != nil || a.Address != in.Email || len(in.Email) > 254 {
		return errors.New("неверный адрес почты")
	}
	in.Name = strings.TrimSpace(in.Name)
	if len([]rune(in.Name)) > 100 {
		return errors.New("имя отправителя — до 100 символов")
	}
	in.IMAPHost, in.SMTPHost = strings.TrimSpace(strings.ToLower(in.IMAPHost)), strings.TrimSpace(strings.ToLower(in.SMTPHost))
	if in.IMAPHost == "" || in.SMTPHost == "" {
		return errors.New("укажите серверы IMAP и SMTP")
	}
	in.Login = strings.TrimSpace(in.Login)
	if in.Login == "" {
		in.Login = in.Email
	}
	if in.Password == "" {
		return errors.New("укажите пароль (для Яндекса, Mail.ru и Gmail — пароль приложения)")
	}
	if in.IMAPSec == "" {
		in.IMAPSec = SecTLS
	}
	if in.SMTPSec == "" {
		in.SMTPSec = SecTLS
	}
	if in.IMAPPort == 0 {
		in.IMAPPort = map[string]int{SecTLS: 993, SecSTARTTLS: 143, SecNone: 143}[in.IMAPSec]
	}
	if in.SMTPPort == 0 {
		in.SMTPPort = map[string]int{SecTLS: 465, SecSTARTTLS: 587, SecNone: 25}[in.SMTPSec]
	}
	return nil
}

// AddAccount проверяет вход на IMAP и SMTP и только тогда сохраняет ящик.
func (s *Service) AddAccount(ctx context.Context, uid int64, in AccountInput) (*Account, error) {
	if !s.Enabled() {
		return nil, ErrDisabled
	}
	if err := in.normalize(); err != nil {
		return nil, err
	}
	var n int
	if err := s.DB.QueryRowContext(ctx, `SELECT count(*) FROM mail_accounts WHERE user_id=$1`, uid).Scan(&n); err != nil {
		return nil, err
	}
	if n >= MaxAccounts {
		return nil, fmt.Errorf("не больше %d ящиков", MaxAccounts)
	}
	a := &Account{UserID: uid, Email: in.Email, Name: in.Name, IMAPHost: in.IMAPHost, IMAPPort: in.IMAPPort, IMAPSec: in.IMAPSec,
		SMTPHost: in.SMTPHost, SMTPPort: in.SMTPPort, SMTPSec: in.SMTPSec, Login: in.Login, password: in.Password}
	c, err := s.connect(ctx, a)
	if err != nil {
		return nil, fmt.Errorf("IMAP: %w", err)
	}
	c.close()
	if err := s.checkSMTP(ctx, a); err != nil {
		return nil, fmt.Errorf("SMTP: %w", err)
	}
	enc, err := s.seal(in.Password)
	if err != nil {
		return nil, err
	}
	err = s.DB.QueryRowContext(ctx, `INSERT INTO mail_accounts (user_id, email, display_name, imap_host, imap_port, imap_sec,
		smtp_host, smtp_port, smtp_sec, login, password_enc) VALUES ($1,$2,$3,$4,$5,$6,$7,$8,$9,$10,$11) RETURNING id, created_at`,
		uid, a.Email, a.Name, a.IMAPHost, a.IMAPPort, a.IMAPSec, a.SMTPHost, a.SMTPPort, a.SMTPSec, a.Login, enc).Scan(&a.ID, &a.Created)
	return a, err
}

const accCols = `id, user_id, email, display_name, imap_host, imap_port, imap_sec, smtp_host, smtp_port, smtp_sec, login, created_at`

func scanAccount(sc interface{ Scan(...any) error }, extra ...any) (*Account, error) {
	var a Account
	dst := append([]any{&a.ID, &a.UserID, &a.Email, &a.Name, &a.IMAPHost, &a.IMAPPort, &a.IMAPSec, &a.SMTPHost, &a.SMTPPort, &a.SMTPSec, &a.Login, &a.Created}, extra...)
	if err := sc.Scan(dst...); err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			return nil, ErrNotFound
		}
		return nil, err
	}
	return &a, nil
}

func (s *Service) Accounts(ctx context.Context, uid int64) ([]Account, error) {
	rows, err := s.DB.QueryContext(ctx, `SELECT `+accCols+` FROM mail_accounts WHERE user_id=$1 ORDER BY id`, uid)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := []Account{}
	for rows.Next() {
		a, err := scanAccount(rows)
		if err != nil {
			return nil, err
		}
		out = append(out, *a)
	}
	return out, rows.Err()
}

// Account — ящик пользователя uid вместе с расшифрованным паролем (для подключения).
func (s *Service) Account(ctx context.Context, uid int64, id string) (*Account, error) {
	if !s.Enabled() {
		return nil, ErrDisabled
	}
	n, err := strconv.ParseInt(id, 10, 64)
	if err != nil {
		return nil, ErrNotFound
	}
	var enc []byte
	a, err := scanAccount(s.DB.QueryRowContext(ctx, `SELECT `+accCols+`, password_enc FROM mail_accounts WHERE id=$1 AND user_id=$2`, n, uid), &enc)
	if err != nil {
		return nil, err
	}
	if a.password, err = s.open(enc); err != nil {
		return nil, err
	}
	return a, nil
}

func (s *Service) DeleteAccount(ctx context.Context, uid int64, id string) error {
	n, err := strconv.ParseInt(id, 10, 64)
	if err != nil {
		return ErrNotFound
	}
	res, err := s.DB.ExecContext(ctx, `DELETE FROM mail_accounts WHERE id=$1 AND user_id=$2`, n, uid)
	if err != nil {
		return err
	}
	if k, _ := res.RowsAffected(); k == 0 {
		return ErrNotFound
	}
	return nil
}

// Preset — настройки популярных почтовых сервисов (подставляются по домену адреса).
type Preset struct {
	IMAPHost string `json:"imap_host"`
	IMAPPort int    `json:"imap_port"`
	IMAPSec  string `json:"imap_sec"`
	SMTPHost string `json:"smtp_host"`
	SMTPPort int    `json:"smtp_port"`
	SMTPSec  string `json:"smtp_sec"`
	Hint     string `json:"hint,omitempty"`
}

var presets = map[string]Preset{
	"yandex.ru": {"imap.yandex.ru", 993, SecTLS, "smtp.yandex.ru", 465, SecTLS, "Нужен пароль приложения: id.yandex.ru → Безопасность → Пароли приложений, и включённый доступ по IMAP в настройках Почты."},
	"mail.ru":   {"imap.mail.ru", 993, SecTLS, "smtp.mail.ru", 465, SecTLS, "Нужен пароль для внешнего приложения: Настройки → Безопасность → Пароли для внешних приложений."},
	"gmail.com": {"imap.gmail.com", 993, SecTLS, "smtp.gmail.com", 465, SecTLS, "Нужен пароль приложения (myaccount.google.com/apppasswords), для него включите двухэтапную аутентификацию."},
	"outlook.com": {"outlook.office365.com", 993, SecTLS, "smtp-mail.outlook.com", 587, SecSTARTTLS,
		"Microsoft отключает вход по паролю для IMAP; может понадобиться пароль приложения или он не сработает вовсе."},
	"icloud.com": {"imap.mail.me.com", 993, SecTLS, "smtp.mail.me.com", 587, SecSTARTTLS, "Нужен пароль приложения: appleid.apple.com → Вход и безопасность."},
	"rambler.ru": {"imap.rambler.ru", 993, SecTLS, "smtp.rambler.ru", 465, SecTLS, "Включите доступ по IMAP: Настройки → Программы."},
}

var presetAlias = map[string]string{
	"ya.ru": "yandex.ru", "yandex.com": "yandex.ru", "narod.ru": "yandex.ru",
	"bk.ru": "mail.ru", "inbox.ru": "mail.ru", "list.ru": "mail.ru", "internet.ru": "mail.ru",
	"googlemail.com": "gmail.com", "hotmail.com": "outlook.com", "live.com": "outlook.com",
	"me.com": "icloud.com", "mac.com": "icloud.com", "lenta.ru": "rambler.ru", "ro.ru": "rambler.ru",
}

// PresetFor — известные настройки по адресу почты (только встроенная таблица, без запросов в сеть).
func PresetFor(email string) (Preset, bool) {
	at := strings.LastIndex(email, "@")
	if at < 0 {
		return Preset{}, false
	}
	d := strings.ToLower(strings.TrimSpace(email[at+1:]))
	if a, ok := presetAlias[d]; ok {
		d = a
	}
	p, ok := presets[d]
	return p, ok
}
