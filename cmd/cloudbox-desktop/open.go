package main

import (
	"os/exec"
	"runtime"
)

// openURL открывает адрес (или папку) в программе по умолчанию.
func openURL(target string) error {
	switch runtime.GOOS {
	case "windows":
		return exec.Command("rundll32", "url.dll,FileProtocolHandler", target).Start()
	case "darwin":
		return exec.Command("open", target).Start()
	default:
		return exec.Command("xdg-open", target).Start()
	}
}
