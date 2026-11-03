// Package config читает настройки из переменных окружения.
package config

import (
	"crypto/rand"
	"encoding/hex"
	"log"
	"os"
	"strconv"
	"strings"
)

type Config struct {
	Listen            string
	DatabaseURL       string
	PublicURL         string // внешний адрес, например https://cloud.example.ru
	Secret            string // ключ для подписи cookie публичных ссылок
	AllowRegistration bool
	DefaultQuotaMB    int64 // 0 — без ограничения
	MaxVersions       int   // сколько старых версий хранить на файл
	TrashDays         int   // через сколько дней чистить корзину
	MaxUploadMB       int64
	TmpDir            string

	Storage     string // local | s3
	DataDir     string
	S3Endpoint  string
	S3Region    string
	S3Bucket    string
	S3Access    string
	S3Secret    string
	S3PathStyle bool

	EncryptionKey string // 64 hex — шифрование блобов AES-256-GCM
	Timezone      string // для дат EXIF без смещения и календаря
	LogFormat     string // text | json
	MetricsToken  string

	BackupDir  string
	BackupHour int
	BackupKeep int

	OIDCIssuer       string
	OIDCClientID     string
	OIDCClientSecret string
	OIDCName         string // подпись кнопки входа
	OIDCAutoCreate   bool
	OIDCLinkByName   bool
	PasswordLogin    bool // false — вход только через OIDC

	WOPIEditorURL    string
	WOPIDiscoveryURL string
	WOPIHostURL      string
}

func Load() Config {
	c := Config{
		Listen:            env("CB_LISTEN", ":8080"),
		DatabaseURL:       env("CB_DATABASE_URL", "postgres://cloudbox:cloudbox@localhost:5432/cloudbox?sslmode=disable"),
		PublicURL:         strings.TrimRight(env("CB_PUBLIC_URL", "http://localhost:8080"), "/"),
		Secret:            env("CB_SECRET", ""),
		AllowRegistration: envBool("CB_ALLOW_REGISTRATION", false),
		DefaultQuotaMB:    envInt("CB_DEFAULT_QUOTA_MB", 10240),
		MaxVersions:       int(envInt("CB_MAX_VERSIONS", 10)),
		TrashDays:         int(envInt("CB_TRASH_DAYS", 30)),
		MaxUploadMB:       envInt("CB_MAX_UPLOAD_MB", 4096),
		TmpDir:            env("CB_TMP_DIR", os.TempDir()),
		Storage:           env("CB_STORAGE", "local"),
		DataDir:           env("CB_DATA_DIR", "./data"),
		S3Endpoint:        env("CB_S3_ENDPOINT", ""),
		S3Region:          env("CB_S3_REGION", "garage"),
		S3Bucket:          env("CB_S3_BUCKET", "cloudbox"),
		S3Access:          env("CB_S3_ACCESS_KEY", ""),
		S3Secret:          env("CB_S3_SECRET_KEY", ""),
		S3PathStyle:       envBool("CB_S3_PATH_STYLE", true),

		EncryptionKey: env("CB_ENCRYPTION_KEY", ""),
		Timezone:      env("CB_TZ", env("TZ", "Asia/Yekaterinburg")),
		LogFormat:     env("CB_LOG_FORMAT", "text"),
		MetricsToken:  env("CB_METRICS_TOKEN", ""),

		BackupDir:  env("CB_BACKUP_DIR", ""),
		BackupHour: int(envInt("CB_BACKUP_HOUR", 3)),
		BackupKeep: int(envInt("CB_BACKUP_KEEP", 7)),

		OIDCIssuer:       env("CB_OIDC_ISSUER", ""),
		OIDCClientID:     env("CB_OIDC_CLIENT_ID", ""),
		OIDCClientSecret: env("CB_OIDC_CLIENT_SECRET", ""),
		OIDCName:         env("CB_OIDC_NAME", "Войти через SSO"),
		OIDCAutoCreate:   envBool("CB_OIDC_AUTO_CREATE", true),
		OIDCLinkByName:   envBool("CB_OIDC_LINK_BY_USERNAME", false),
		PasswordLogin:    envBool("CB_PASSWORD_LOGIN", true),

		WOPIEditorURL:    strings.TrimRight(env("CB_WOPI_URL", ""), "/"),
		WOPIDiscoveryURL: env("CB_WOPI_DISCOVERY_URL", ""),
		WOPIHostURL:      env("CB_WOPI_HOST_URL", ""),
	}
	if c.WOPIHostURL == "" {
		c.WOPIHostURL = c.PublicURL
	}
	if c.Secret == "" {
		b := make([]byte, 32)
		_, _ = rand.Read(b)
		c.Secret = hex.EncodeToString(b)
		log.Println("WARN: CB_SECRET не задан — сгенерирован временный, пароли публичных ссылок придётся вводить заново после перезапуска")
	}
	return c
}

func env(k, def string) string {
	if v, ok := os.LookupEnv(k); ok && v != "" {
		return v
	}
	return def
}

func envInt(k string, def int64) int64 {
	if v := os.Getenv(k); v != "" {
		if n, err := strconv.ParseInt(v, 10, 64); err == nil {
			return n
		}
	}
	return def
}

func envBool(k string, def bool) bool {
	if v := os.Getenv(k); v != "" {
		b, err := strconv.ParseBool(v)
		if err == nil {
			return b
		}
	}
	return def
}
