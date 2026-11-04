package main

import (
	"encoding/base64"
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
)

// Config — настройки клиента. Хранятся в папке настроек пользователя (права 0600);
// пароль приложения на Windows дополнительно зашифрован DPAPI — его может расшифровать только этот пользователь.
type Config struct {
	Server    string `json:"server"`
	User      string `json:"user"`
	Password  string `json:"-"`
	Secret    string `json:"password_protected,omitempty"` // защищённый пароль (DPAPI на Windows, base64 в остальных ОС)
	Local     string `json:"local"`
	Remote    string `json:"remote"`
	Interval  int    `json:"interval_sec"`
	Paused    bool   `json:"paused"`
	Autostart bool   `json:"autostart"`
}

func (c *Config) Ready() bool {
	return c.Server != "" && c.User != "" && c.Password != "" && c.Local != ""
}

func (c *Config) interval() int {
	if c.Interval < 10 {
		return 60
	}
	return c.Interval
}

func defaultConfigPath() string {
	dir, err := os.UserConfigDir()
	if err != nil {
		dir = "."
	}
	return filepath.Join(dir, "CloudBox", "desktop.json")
}

func loadConfig(path string) (*Config, error) {
	c := &Config{Remote: "/", Interval: 60}
	b, err := os.ReadFile(path)
	if errors.Is(err, os.ErrNotExist) {
		return c, nil
	}
	if err != nil {
		return nil, err
	}
	if err := json.Unmarshal(b, c); err != nil {
		return nil, err
	}
	if c.Secret != "" {
		raw, err := base64.StdEncoding.DecodeString(c.Secret)
		if err == nil {
			if p, err := unprotect(raw); err == nil {
				c.Password = string(p)
			}
		}
	}
	return c, nil
}

func saveConfig(path string, c *Config) error {
	if c.Password != "" {
		enc, err := protect([]byte(c.Password))
		if err != nil {
			return err
		}
		c.Secret = base64.StdEncoding.EncodeToString(enc)
	}
	b, err := json.MarshalIndent(c, "", "  ")
	if err != nil {
		return err
	}
	if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
		return err
	}
	tmp := path + ".tmp"
	if err := os.WriteFile(tmp, b, 0o600); err != nil {
		return err
	}
	return os.Rename(tmp, path)
}
