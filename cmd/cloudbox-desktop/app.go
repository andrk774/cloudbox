package main

import (
	"crypto/subtle"
	_ "embed"
	"encoding/json"
	"errors"
	"fmt"
	"net"
	"net/http"
	"net/url"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"time"

	"github.com/andrk774/cloudbox/internal/secure"
	"github.com/andrk774/cloudbox/internal/syncclient"
)

//go:embed ui.html
var uiHTML []byte

const (
	cookieName = "cbd_token"
	maxLogs    = 300
	stateFile  = ".cloudbox-sync.json" // состояние движка в синхронизируемой папке
)

// App — клиент синхронизации: фоновый цикл и локальный веб-интерфейс на 127.0.0.1.
type App struct {
	cfgPath string
	token   string // доступ к интерфейсу: без него локальный сервер не отвечает
	addr    string // 127.0.0.1:порт

	mu        sync.Mutex
	cfg       *Config
	syncer    *syncclient.Syncer
	state     string // setup, idle, syncing, paused, error
	lastRun   time.Time
	lastOK    time.Time
	stats     syncclient.Stats
	lastErr   string
	logs      []string
	trigger   chan struct{}
	quit      chan struct{}
	quitOnce  sync.Once
	onChange  func() // обновить значок в трее
	runSyncer func(*syncclient.Syncer) (syncclient.Stats, error)
}

func newApp(cfgPath string, cfg *Config) *App {
	a := &App{cfgPath: cfgPath, cfg: cfg, token: secure.Token(24), trigger: make(chan struct{}, 1), quit: make(chan struct{}),
		runSyncer: func(s *syncclient.Syncer) (syncclient.Stats, error) { return s.Run() }}
	a.state = a.idleState()
	return a
}

func (a *App) idleState() string {
	switch {
	case !a.cfg.Ready():
		return "setup"
	case a.cfg.Paused:
		return "paused"
	}
	return "idle"
}

func (a *App) logf(f string, args ...any) {
	line := time.Now().Format("15:04:05 ") + fmt.Sprintf(f, args...)
	a.mu.Lock()
	a.logs = append(a.logs, line)
	if len(a.logs) > maxLogs {
		a.logs = a.logs[len(a.logs)-maxLogs:]
	}
	a.mu.Unlock()
}

func (a *App) changed() {
	if a.onChange != nil {
		a.onChange()
	}
}

// SyncNow — запустить синхронизацию, не дожидаясь интервала.
func (a *App) SyncNow() {
	select {
	case a.trigger <- struct{}{}:
	default:
	}
}

func (a *App) Quit() { a.quitOnce.Do(func() { close(a.quit) }) }

func (a *App) SetPaused(p bool) error {
	a.mu.Lock()
	a.cfg.Paused = p
	err := saveConfig(a.cfgPath, a.cfg)
	if a.state != "syncing" {
		a.state = a.idleState()
	}
	a.mu.Unlock()
	a.changed()
	if !p {
		a.SyncNow()
	}
	return err
}

// loop — фоновая синхронизация: по интервалу и по запросу.
func (a *App) loop() {
	timer := time.NewTimer(2 * time.Second) // первый проход вскоре после запуска
	for {
		select {
		case <-a.quit:
			return
		case <-a.trigger:
		case <-timer.C:
		}
		a.syncOnce()
		a.mu.Lock()
		next := time.Duration(a.cfg.interval()) * time.Second
		a.mu.Unlock()
		if !timer.Stop() {
			select {
			case <-timer.C:
			default:
			}
		}
		timer.Reset(next)
	}
}

func (a *App) syncOnce() {
	a.mu.Lock()
	if !a.cfg.Ready() || a.cfg.Paused {
		a.state = a.idleState()
		a.mu.Unlock()
		return
	}
	if a.syncer == nil {
		s, err := syncclient.New(syncclient.Config{Server: a.cfg.Server, User: a.cfg.User, Password: a.cfg.Password,
			Local: a.cfg.Local, Remote: a.cfg.Remote}, a.logf)
		if err != nil {
			a.state, a.lastErr = "error", err.Error()
			a.mu.Unlock()
			a.changed()
			return
		}
		a.syncer = s
	}
	s := a.syncer
	a.state = "syncing"
	a.mu.Unlock()
	a.changed()

	st, err := a.runSyncer(s)

	a.mu.Lock()
	a.lastRun = time.Now()
	if err != nil {
		a.state, a.lastErr = "error", err.Error()
		a.mu.Unlock()
		a.logf("ошибка: %v", err)
		a.changed()
		return
	}
	a.stats, a.lastErr, a.lastOK = st, "", a.lastRun
	a.state = a.idleState()
	a.mu.Unlock()
	if st.Changed() {
		a.logf("готово: загружено %d, скачано %d, удалено локально %d, на сервере %d, конфликтов %d",
			st.Up, st.Down, st.DelLocal, st.DelRemote, st.Conflicts)
	}
	a.changed()
}

// Status — то, что показывает интерфейс и трей.
type Status struct {
	State       string           `json:"state"`
	Server      string           `json:"server"`
	User        string           `json:"user"`
	HasPassword bool             `json:"has_password"`
	Local       string           `json:"local"`
	Remote      string           `json:"remote"`
	Interval    int              `json:"interval_sec"`
	Paused      bool             `json:"paused"`
	Autostart   bool             `json:"autostart"`
	LastRun     *time.Time       `json:"last_run,omitempty"`
	LastOK      *time.Time       `json:"last_ok,omitempty"`
	Stats       syncclient.Stats `json:"stats"`
	Error       string           `json:"error,omitempty"`
	Logs        []string         `json:"logs"`
}

func (a *App) Status() Status {
	a.mu.Lock()
	defer a.mu.Unlock()
	st := Status{State: a.state, Server: a.cfg.Server, User: a.cfg.User, HasPassword: a.cfg.Password != "", Local: a.cfg.Local,
		Remote: a.cfg.Remote, Interval: a.cfg.interval(), Paused: a.cfg.Paused, Autostart: a.cfg.Autostart, Stats: a.stats,
		Error: a.lastErr, Logs: append([]string{}, a.logs...)}
	if !a.lastRun.IsZero() {
		t := a.lastRun
		st.LastRun = &t
	}
	if !a.lastOK.IsZero() {
		t := a.lastOK
		st.LastOK = &t
	}
	return st
}

// ConfigInput — форма настройки. Пустой пароль — оставить сохранённый.
type ConfigInput struct {
	Server    string `json:"server"`
	User      string `json:"user"`
	Password  string `json:"password"`
	Local     string `json:"local"`
	Remote    string `json:"remote"`
	Interval  int    `json:"interval_sec"`
	Autostart bool   `json:"autostart"`
}

var checkServer = syncclient.Check

func (a *App) Configure(in ConfigInput) error {
	in.Server = strings.TrimRight(strings.TrimSpace(in.Server), "/")
	in.User, in.Local = strings.TrimSpace(in.User), strings.TrimSpace(in.Local)
	if in.Remote = "/" + strings.Trim(strings.TrimSpace(in.Remote), "/"); in.Remote == "/" {
		in.Remote = "/"
	}
	u, err := url.Parse(in.Server)
	if err != nil || (u.Scheme != "https" && u.Scheme != "http") || u.Host == "" {
		return errors.New("адрес сервера — например https://cloud.example.ru")
	}
	if in.User == "" || in.Local == "" {
		return errors.New("укажите пользователя и локальную папку")
	}
	a.mu.Lock()
	old := *a.cfg
	a.mu.Unlock()
	if in.Password == "" {
		in.Password = old.Password
	}
	if in.Password == "" {
		return errors.New("укажите пароль приложения (Настройки CloudBox → Пароли приложений)")
	}
	if err := checkServer(in.Server, in.User, in.Password); err != nil {
		return err
	}
	local, err := filepath.Abs(syncclient.ExpandHome(in.Local))
	if err != nil {
		return err
	}
	if err := os.MkdirAll(local, 0o755); err != nil {
		return fmt.Errorf("локальная папка: %v", err)
	}
	// Другой сервер, пользователь или папка на сервере для той же локальной папки: старое состояние
	// описывает другую пару папок, и по нему движок счёл бы файлы удалёнными. Сбрасываем его —
	// первый проход тогда только объединит содержимое обеих сторон, ничего не удаляя.
	if local == old.Local && (in.Server != old.Server || in.User != old.User || in.Remote != old.Remote) {
		os.Remove(filepath.Join(local, stateFile))
	}
	if in.Autostart != old.Autostart {
		if err := setAutostart(in.Autostart); err != nil {
			return fmt.Errorf("автозапуск: %v", err)
		}
	}
	a.mu.Lock()
	a.cfg.Server, a.cfg.User, a.cfg.Password, a.cfg.Local, a.cfg.Remote = in.Server, in.User, in.Password, local, in.Remote
	a.cfg.Interval, a.cfg.Autostart = in.Interval, in.Autostart
	a.syncer = nil
	a.lastErr = ""
	if a.state != "syncing" {
		a.state = a.idleState()
	}
	err = saveConfig(a.cfgPath, a.cfg)
	a.mu.Unlock()
	if err != nil {
		return err
	}
	a.logf("настройки сохранены: %s ⇄ %s%s", local, in.Server, in.Remote)
	a.changed()
	a.SyncNow()
	return nil
}

// ---------- локальный веб-интерфейс ----------

// URL — адрес интерфейса с токеном (открывается в браузере, токен уходит в cookie).
func (a *App) URL() string { return "http://" + a.addr + "/?t=" + a.token }

// guard пускает только свой браузер:
//   - Host — ровно наш адрес (защита от DNS rebinding: чужой сайт под своим доменом до нас не достучится);
//   - токен в cookie (или ?t= при первом открытии);
//   - изменяющие запросы — только со своего Origin (защита от CSRF).
func (a *App) guard(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_, port, _ := net.SplitHostPort(a.addr)
		if r.Host != a.addr && r.Host != "localhost:"+port {
			http.Error(w, "forbidden", http.StatusForbidden)
			return
		}
		if t := r.URL.Query().Get("t"); r.Method == http.MethodGet && r.URL.Path == "/" && t != "" {
			if subtle.ConstantTimeCompare([]byte(t), []byte(a.token)) != 1 {
				http.Error(w, "forbidden", http.StatusForbidden)
				return
			}
			http.SetCookie(w, &http.Cookie{Name: cookieName, Value: a.token, Path: "/", HttpOnly: true, SameSite: http.SameSiteStrictMode})
			http.Redirect(w, r, "/", http.StatusFound)
			return
		}
		c, err := r.Cookie(cookieName)
		if err != nil || subtle.ConstantTimeCompare([]byte(c.Value), []byte(a.token)) != 1 {
			w.Header().Set("Content-Type", "text/plain; charset=utf-8")
			w.WriteHeader(http.StatusForbidden)
			w.Write([]byte("Откройте CloudBox через значок в трее или повторным запуском программы."))
			return
		}
		if r.Method != http.MethodGet {
			if o := r.Header.Get("Origin"); o != "http://"+r.Host {
				http.Error(w, "forbidden", http.StatusForbidden)
				return
			}
		}
		w.Header().Set("X-Frame-Options", "DENY")
		w.Header().Set("Cache-Control", "no-store")
		next.ServeHTTP(w, r)
	})
}

func writeJSON(w http.ResponseWriter, code int, v any) {
	w.Header().Set("Content-Type", "application/json; charset=utf-8")
	w.WriteHeader(code)
	json.NewEncoder(w).Encode(v)
}

func (a *App) Handler() http.Handler {
	mux := http.NewServeMux()
	mux.HandleFunc("GET /{$}", func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/html; charset=utf-8")
		w.Header().Set("Content-Security-Policy", "default-src 'none'; script-src 'unsafe-inline'; style-src 'unsafe-inline'; connect-src 'self'")
		w.Write(uiHTML)
	})
	mux.HandleFunc("GET /api/status", func(w http.ResponseWriter, r *http.Request) { writeJSON(w, 200, a.Status()) })
	mux.HandleFunc("POST /api/config", func(w http.ResponseWriter, r *http.Request) {
		var in ConfigInput
		if err := json.NewDecoder(http.MaxBytesReader(w, r.Body, 64<<10)).Decode(&in); err != nil {
			writeJSON(w, 400, map[string]string{"error": "некорректный запрос"})
			return
		}
		if err := a.Configure(in); err != nil {
			writeJSON(w, 400, map[string]string{"error": err.Error()})
			return
		}
		writeJSON(w, 200, a.Status())
	})
	mux.HandleFunc("POST /api/sync", func(w http.ResponseWriter, r *http.Request) {
		a.SyncNow()
		writeJSON(w, 200, map[string]bool{"ok": true})
	})
	mux.HandleFunc("POST /api/pause", func(w http.ResponseWriter, r *http.Request) {
		var in struct {
			Paused bool `json:"paused"`
		}
		json.NewDecoder(http.MaxBytesReader(w, r.Body, 1<<10)).Decode(&in)
		if err := a.SetPaused(in.Paused); err != nil {
			writeJSON(w, 500, map[string]string{"error": err.Error()})
			return
		}
		writeJSON(w, 200, a.Status())
	})
	mux.HandleFunc("POST /api/open", func(w http.ResponseWriter, r *http.Request) {
		a.mu.Lock()
		p := a.cfg.Local
		a.mu.Unlock()
		if p == "" {
			writeJSON(w, 400, map[string]string{"error": "папка не настроена"})
			return
		}
		openURL(p)
		writeJSON(w, 200, map[string]bool{"ok": true})
	})
	mux.HandleFunc("POST /api/web", func(w http.ResponseWriter, r *http.Request) {
		a.mu.Lock()
		s := a.cfg.Server
		a.mu.Unlock()
		if s != "" {
			openURL(s)
		}
		writeJSON(w, 200, map[string]bool{"ok": true})
	})
	mux.HandleFunc("POST /api/quit", func(w http.ResponseWriter, r *http.Request) {
		writeJSON(w, 200, map[string]bool{"ok": true})
		go func() { time.Sleep(200 * time.Millisecond); a.Quit() }()
	})
	return a.guard(mux)
}
