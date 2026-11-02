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
