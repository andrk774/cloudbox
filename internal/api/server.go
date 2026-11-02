// Package api — HTTP-сервер: REST API, публичные ссылки, WebDAV и встроенный веб-интерфейс.
package api

import (
	"context"
	"encoding/json"
	"errors"
	"io/fs"
	"log"
	"net"
	"net/http"
	"net/url"
	"strings"
	"sync"
	"time"

	"github.com/andrk774/cloudbox/internal/auth"
	"github.com/andrk774/cloudbox/internal/config"
	"github.com/andrk774/cloudbox/internal/files"
)

const sessionCookie = "cb_session"

type Server struct {
	Cfg   config.Config
	Auth  *auth.Service
	Files *files.Service
	DAV   http.Handler
	Web   fs.FS

	limMu sync.Mutex
	lim   map[string]*bucket
}

type ctxKey struct{}

func userFrom(r *http.Request) *auth.User {
	u, _ := r.Context().Value(ctxKey{}).(*auth.User)
	return u
}

func (s *Server) Handler() http.Handler {
	mux := http.NewServeMux()

	// --- авторизация
	mux.HandleFunc("GET /api/auth/status", s.authStatus)
	mux.HandleFunc("POST /api/auth/register", s.register)
	mux.HandleFunc("POST /api/auth/login", s.login)
	mux.HandleFunc("POST /api/auth/logout", s.logout)

	// --- профиль
	mux.HandleFunc("GET /api/me", s.need(s.me))
	mux.HandleFunc("POST /api/me/password", s.need(s.changePassword))
	mux.HandleFunc("GET /api/me/app-passwords", s.need(s.listAppPasswords))
	mux.HandleFunc("POST /api/me/app-passwords", s.need(s.createAppPassword))
	mux.HandleFunc("DELETE /api/me/app-passwords/{id}", s.need(s.deleteAppPassword))

	// --- файлы
	mux.HandleFunc("GET /api/files", s.need(s.listFiles))
	mux.HandleFunc("GET /api/files/{id}", s.need(s.getFile))
	mux.HandleFunc("POST /api/files/folder", s.need(s.mkdir))
	mux.HandleFunc("POST /api/files/upload", s.need(s.upload))
	mux.HandleFunc("GET /api/files/{id}/content", s.need(s.content))
	mux.HandleFunc("GET /api/files/{id}/zip", s.need(s.zip))
	mux.HandleFunc("PATCH /api/files/{id}", s.need(s.moveFile))
	mux.HandleFunc("POST /api/files/{id}/copy", s.need(s.copyFile))
	mux.HandleFunc("DELETE /api/files/{id}", s.need(s.trashFile))
	mux.HandleFunc("GET /api/files/{id}/versions", s.need(s.versions))
	mux.HandleFunc("POST /api/files/{id}/versions/{vid}/restore", s.need(s.restoreVersion))
	mux.HandleFunc("GET /api/search", s.need(s.search))
	mux.HandleFunc("GET /api/recent", s.need(s.recent))

	// --- корзина
	mux.HandleFunc("GET /api/trash", s.need(s.listTrash))
	mux.HandleFunc("POST /api/trash/{id}/restore", s.need(s.restoreTrash))
	mux.HandleFunc("DELETE /api/trash/{id}", s.need(s.purge))
	mux.HandleFunc("DELETE /api/trash", s.need(s.emptyTrash))

	// --- публичные ссылки (управление)
	mux.HandleFunc("GET /api/shares", s.need(s.listShares))
	mux.HandleFunc("POST /api/shares", s.need(s.createShare))
	mux.HandleFunc("DELETE /api/shares/{id}", s.need(s.deleteShare))

	// --- администрирование
	mux.HandleFunc("GET /api/admin/users", s.admin(s.adminListUsers))
	mux.HandleFunc("POST /api/admin/users", s.admin(s.adminCreateUser))
	mux.HandleFunc("PATCH /api/admin/users/{id}", s.admin(s.adminUpdateUser))
	mux.HandleFunc("DELETE /api/admin/users/{id}", s.admin(s.adminDeleteUser))

	// --- публичный доступ по ссылке
	mux.HandleFunc("GET /api/public/{token}", s.pubInfo)
	mux.HandleFunc("POST /api/public/{token}/auth", s.pubAuth)
	mux.HandleFunc("GET /api/public/{token}/list", s.pubList)
	mux.HandleFunc("GET /api/public/{token}/content", s.pubContent)
	mux.HandleFunc("GET /api/public/{token}/zip", s.pubZip)
	mux.HandleFunc("POST /api/public/{token}/upload", s.pubUpload)
	mux.HandleFunc("GET /s/{token}", s.sharePage)

	// --- веб-интерфейс
	mux.HandleFunc("GET /healthz", func(w http.ResponseWriter, r *http.Request) { w.Write([]byte("ok")) })
	mux.Handle("GET /", s.static())

	// WebDAV использует свои методы (PROPFIND, MKCOL, …), поэтому направляем его в обход mux.
	root := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch {
		case s.DAV != nil && (r.URL.Path == "/dav" || strings.HasPrefix(r.URL.Path, "/dav/")):
			s.DAV.ServeHTTP(w, r)
		case r.URL.Path == "/.well-known/webdav" || r.URL.Path == "/remote.php/webdav" || strings.HasPrefix(r.URL.Path, "/remote.php/webdav/"):
			http.Redirect(w, r, "/dav/", http.StatusMovedPermanently)
		default:
			mux.ServeHTTP(w, r)
		}
	})
	return s.middleware(root)
}

func (s *Server) static() http.Handler {
	fsrv := http.FileServer(http.FS(s.Web))
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		p := r.URL.Path
		if p != "/" {
			if _, err := fs.Stat(s.Web, strings.TrimPrefix(p, "/")); err != nil {
				// SPA: неизвестный путь отдаём index.html
				r = r.Clone(r.Context())
				r.URL.Path = "/"
			}
		}
		if r.URL.Path == "/" {
			w.Header().Set("Cache-Control", "no-cache")
		}
		fsrv.ServeHTTP(w, r)
	})
}

func (s *Server) sharePage(w http.ResponseWriter, r *http.Request) {
	b, err := fs.ReadFile(s.Web, "share.html")
	if err != nil {
		http.NotFound(w, r)
		return
	}
	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	w.Header().Set("Cache-Control", "no-cache")
	w.Write(b)
}

// ---------- middleware ----------

type statusWriter struct {
	http.ResponseWriter
	code int
}

func (w *statusWriter) WriteHeader(c int) { w.code = c; w.ResponseWriter.WriteHeader(c) }
func (w *statusWriter) Flush() {
	if f, ok := w.ResponseWriter.(http.Flusher); ok {
		f.Flush()
	}
}

func (s *Server) middleware(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		start := time.Now()
		h := w.Header()
		h.Set("X-Content-Type-Options", "nosniff")
		h.Set("Referrer-Policy", "same-origin")
		if !strings.HasPrefix(r.URL.Path, "/dav/") {
			h.Set("X-Frame-Options", "SAMEORIGIN")
		}

		// Защита от CSRF: изменяющие запросы к API только с нашего Origin.
		if strings.HasPrefix(r.URL.Path, "/api/") && r.Method != http.MethodGet && r.Method != http.MethodHead {
			if o := r.Header.Get("Origin"); o != "" && !s.sameOrigin(r, o) {
				writeErr(w, http.StatusForbidden, "запрос с чужого сайта")
				return
			}
		}

		if !strings.HasPrefix(r.URL.Path, "/dav/") {
			if u := s.sessionUser(r); u != nil {
				r = r.WithContext(context.WithValue(r.Context(), ctxKey{}, u))
			}
		}
		sw := &statusWriter{ResponseWriter: w, code: 200}
		next.ServeHTTP(sw, r)
		if strings.HasPrefix(r.URL.Path, "/api/") || strings.HasPrefix(r.URL.Path, "/dav/") || sw.code >= 400 {
			log.Printf("%s %s %d %s", r.Method, r.URL.Path, sw.code, time.Since(start).Round(time.Millisecond))
		}
	})
}

func (s *Server) sameOrigin(r *http.Request, origin string) bool {
	u, err := url.Parse(origin)
	if err != nil {
		return false
	}
	if u.Host == r.Host {
		return true
	}
	if pu, err := url.Parse(s.Cfg.PublicURL); err == nil && pu.Host == u.Host {
		return true
	}
	return false
}

func (s *Server) sessionUser(r *http.Request) *auth.User {
	tok := ""
	if c, err := r.Cookie(sessionCookie); err == nil {
		tok = c.Value
	}
	if h := r.Header.Get("Authorization"); strings.HasPrefix(h, "Bearer ") {
		tok = strings.TrimPrefix(h, "Bearer ")
	}
	if tok == "" {
		return nil
	}
	u, err := s.Auth.UserBySession(r.Context(), tok)
	if err != nil {
		return nil
	}
	return u
}

func (s *Server) need(h http.HandlerFunc) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		if userFrom(r) == nil {
			writeErr(w, http.StatusUnauthorized, "нужно войти")
			return
		}
		h(w, r)
	}
}

func (s *Server) admin(h http.HandlerFunc) http.HandlerFunc {
	return s.need(func(w http.ResponseWriter, r *http.Request) {
		if !userFrom(r).IsAdmin {
			writeErr(w, http.StatusForbidden, "только для администратора")
			return
		}
		h(w, r)
	})
}

// ---------- ограничение частоты (вход, пароли ссылок) ----------

type bucket struct {
	n     int
	reset time.Time
}

// allow — не более max попыток за окно window с одного IP для данного действия.
func (s *Server) allow(r *http.Request, action string, max int, window time.Duration) bool {
	key := action + "|" + clientIP(r)
	s.limMu.Lock()
	defer s.limMu.Unlock()
	if s.lim == nil {
		s.lim = map[string]*bucket{}
	}
	now := time.Now()
	if len(s.lim) > 50000 {
		for k, b := range s.lim {
			if now.After(b.reset) {
				delete(s.lim, k)
			}
		}
	}
	b, ok := s.lim[key]
	if !ok || now.After(b.reset) {
		b = &bucket{reset: now.Add(window)}
		s.lim[key] = b
	}
	b.n++
	return b.n <= max
}

func clientIP(r *http.Request) string {
	host, _, err := net.SplitHostPort(r.RemoteAddr)
	if err != nil {
		host = r.RemoteAddr
	}
	// За обратным прокси (nginx/Caddy на той же машине или в docker-сети) берём X-Real-IP / X-Forwarded-For
	if ip := net.ParseIP(host); ip != nil && (ip.IsLoopback() || ip.IsPrivate()) {
		if xr := r.Header.Get("X-Real-IP"); xr != "" {
			return xr
		}
		if xf := r.Header.Get("X-Forwarded-For"); xf != "" {
			return strings.TrimSpace(strings.Split(xf, ",")[0])
		}
	}
	return host
}

// ---------- JSON и ошибки ----------

func readJSON(r *http.Request, v any) error {
	dec := json.NewDecoder(http.MaxBytesReader(nil, r.Body, 1<<20))
	if err := dec.Decode(v); err != nil {
		return errors.New("некорректный JSON")
	}
	return nil
}

func writeJSON(w http.ResponseWriter, code int, v any) {
	w.Header().Set("Content-Type", "application/json; charset=utf-8")
	w.Header().Set("Cache-Control", "no-store")
	w.WriteHeader(code)
	json.NewEncoder(w).Encode(v)
}

func writeErr(w http.ResponseWriter, code int, msg string) {
	writeJSON(w, code, map[string]string{"error": msg})
}

// fail переводит доменные ошибки в HTTP-коды.
func fail(w http.ResponseWriter, err error) {
	switch {
	case errors.Is(err, files.ErrNotFound), errors.Is(err, auth.ErrNotFound):
		writeErr(w, http.StatusNotFound, err.Error())
	case errors.Is(err, files.ErrExists), errors.Is(err, auth.ErrUserExists):
		writeErr(w, http.StatusConflict, err.Error())
	case errors.Is(err, files.ErrQuota):
		writeErr(w, http.StatusInsufficientStorage, err.Error())
	case errors.Is(err, files.ErrTooLarge):
		writeErr(w, http.StatusRequestEntityTooLarge, err.Error())
	case errors.Is(err, files.ErrNotDir), errors.Is(err, files.ErrIsDir), errors.Is(err, files.ErrInvalidName),
		errors.Is(err, files.ErrCycle), errors.Is(err, auth.ErrBadUsername), errors.Is(err, auth.ErrWeakPassword):
		writeErr(w, http.StatusBadRequest, err.Error())
	case errors.Is(err, auth.ErrBadCredentials):
		writeErr(w, http.StatusUnauthorized, err.Error())
	default:
		log.Println("internal error:", err)
		writeErr(w, http.StatusInternalServerError, "внутренняя ошибка сервера")
	}
}
