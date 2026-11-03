package main

import (
	"fmt"
	"os"
	"path/filepath"
	"runtime"
)

// setAutostart включает или выключает запуск клиента при входе в систему.
// Windows — ключ реестра HKCU\...\Run (autostart_windows.go), Linux — ~/.config/autostart, macOS — LaunchAgent.
func setAutostart(on bool) error {
	exe, err := os.Executable()
	if err != nil {
		return err
	}
	switch runtime.GOOS {
	case "windows":
		return setAutostartWindows(on, exe)
	case "darwin":
		home, _ := os.UserHomeDir()
		p := filepath.Join(home, "Library", "LaunchAgents", "ru.cloudbox.desktop.plist")
		if !on {
			return removeIfExists(p)
		}
		plist := fmt.Sprintf(`<?xml version="1.0" encoding="UTF-8"?>
<!DOCTYPE plist PUBLIC "-//Apple//DTD PLIST 1.0//EN" "http://www.apple.com/DTDs/PropertyList-1.0.dtd">
<plist version="1.0"><dict>
<key>Label</key><string>ru.cloudbox.desktop</string>
<key>ProgramArguments</key><array><string>%s</string><string>-no-browser</string></array>
<key>RunAtLoad</key><true/>
</dict></plist>
`, xmlEscape(exe))
		return writeFile(p, plist)
	default:
		dir, err := os.UserConfigDir()
		if err != nil {
			return err
		}
		p := filepath.Join(dir, "autostart", "cloudbox-desktop.desktop")
		if !on {
			return removeIfExists(p)
		}
		return writeFile(p, fmt.Sprintf("[Desktop Entry]\nType=Application\nName=CloudBox\nComment=Синхронизация с CloudBox\nExec=\"%s\" -no-browser\nX-GNOME-Autostart-enabled=true\n", exe))
	}
}

func writeFile(p, s string) error {
	if err := os.MkdirAll(filepath.Dir(p), 0o755); err != nil {
		return err
	}
	return os.WriteFile(p, []byte(s), 0o644)
}

func removeIfExists(p string) error {
	if err := os.Remove(p); err != nil && !os.IsNotExist(err) {
		return err
	}
	return nil
}

func xmlEscape(s string) string {
	r := []rune{}
	for _, c := range s {
		switch c {
		case '&':
			r = append(r, []rune("&amp;")...)
		case '<':
			r = append(r, []rune("&lt;")...)
		case '>':
			r = append(r, []rune("&gt;")...)
		default:
			r = append(r, c)
		}
	}
	return string(r)
}
