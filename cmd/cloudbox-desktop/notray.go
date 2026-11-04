//go:build darwin && !cgo

package main

// Сборка для macOS без cgo (CGO_ENABLED=0) не может показать значок в строке меню:
// клиент работает в фоне, окно открывается в браузере; повторный запуск снова откроет окно.
func runTray(app *App) { <-app.quit }
