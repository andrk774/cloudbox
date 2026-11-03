// cloudbox-desktop — графический клиент синхронизации CloudBox для Windows, Linux и macOS.
//
// Значок в трее и окно настроек в браузере (как у Syncthing): локальный веб-интерфейс на 127.0.0.1,
// доступный только по случайному токену. Синхронизирует так же, как консольный cloudbox-sync.
//
//	cloudbox-desktop              — запустить (повторный запуск откроет окно уже работающего клиента)
//	cloudbox-desktop -no-browser  — не открывать окно при запуске (для автозапуска)
package main

import (
	"flag"
	"fmt"
	"log"
	"net"
	"net/http"
	"os"
	"os/signal"
	"path/filepath"
	"strings"
	"syscall"
	"time"
)

func main() {
	cfgPath := flag.String("config", defaultConfigPath(), "файл настроек")
	noBrowser := flag.Bool("no-browser", false, "не открывать окно при запуске")
	flag.Parse()

	// Один клиент на пользователя: второй запуск только открывает окно первого.
	lock := filepath.Join(filepath.Dir(*cfgPath), "desktop.lock")
	if u := runningInstance(lock); u != "" {
		openURL(u)
		return
	}

	cfg, err := loadConfig(*cfgPath)
	if err != nil {
		fatal(fmt.Errorf("настройки %s: %w", *cfgPath, err))
	}
	app := newApp(*cfgPath, cfg)
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		fatal(err)
	}
	app.addr = ln.Addr().String()
	srv := &http.Server{Handler: app.Handler(), ReadHeaderTimeout: 10 * time.Second}
	go srv.Serve(ln)
	os.MkdirAll(filepath.Dir(lock), 0o700)
	os.WriteFile(lock, []byte(app.URL()), 0o600)
	defer os.Remove(lock)

	go app.loop()
	if !*noBrowser || !cfg.Ready() {
		openURL(app.URL())
	}
	sig := make(chan os.Signal, 1)
	signal.Notify(sig, os.Interrupt, syscall.SIGTERM)
	go func() { <-sig; app.Quit() }()
	runTray(app) // возвращается, когда пользователь выбрал «Выход» (или app.Quit)
	srv.Close()
}

// runningInstance — адрес окна уже запущенного клиента или "" (lock-файл устарел или его нет).
func runningInstance(lock string) string {
	b, err := os.ReadFile(lock)
	if err != nil {
		return ""
	}
	u := strings.TrimSpace(string(b))
	if !strings.HasPrefix(u, "http://127.0.0.1:") {
		return ""
	}
	c := &http.Client{Timeout: 2 * time.Second, CheckRedirect: func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse }}
	resp, err := c.Get(u)
	if err != nil {
		return ""
	}
	resp.Body.Close()
	if resp.StatusCode != http.StatusFound { // живой клиент принял токен и выдал cookie
		return ""
	}
	return u
}

func fatal(err error) {
	log.Println(err)
	os.Exit(1)
}
