//go:build !windows

package main

// В Linux и macOS пароль лежит в файле настроек с правами 0600 (как у git-credential-store и rclone):
// прочитать его может только владелец учётной записи. Это пароль приложения — его можно отозвать
// в настройках CloudBox, не меняя основной пароль.
func protect(data []byte) ([]byte, error) { return data, nil }

func unprotect(data []byte) ([]byte, error) { return data, nil }
