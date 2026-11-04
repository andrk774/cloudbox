// Package api — HTTP-сервер: REST API, публичные ссылки, WebDAV/CalDAV/CardDAV, WOPI и веб-интерфейс.
package api

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"io/fs"
	"log/slog"
	"net"
	"net/http"
	"net/url"
	"strings"
	"time"

	"github.com/andrk774/cloudbox/internal/auth"
	"github.com/andrk774/cloudbox/internal/config"
	"github.com/andrk774/cloudbox/internal/files"
	"github.com/andrk774/cloudbox/internal/mail"
	"github.com/andrk774/cloudbox/internal/metrics"
	"github.com/andrk774/cloudbox/internal/netx"
	"github.com/andrk774/cloudbox/internal/oidc"
	"github.com/andrk774/cloudbox/internal/pim"
	"github.com/andrk774/cloudbox/internal/talk"
	"github.com/andrk774/cloudbox/internal/wopi"
)

const sessionCookie = "cb_session"

// Перебор паролей через Basic-auth в API: не больше 20 неудач за 10 минут с одного адреса.
const (
	basicMaxFails   = 20
	basicFailWindow = 10 * time.Minute
)

type Server struct {
	Cfg    config.Config
	DB     *sql.DB
	Auth   *auth.Service
	Files  *files.Service
	PIM    *pim.Service
	DAV    http.Handler // /dav
	PIMDAV http.Handler // /caldav, /carddav
	WOPI   *wopi.Server
	OIDC   *oidc.Provider
	Web    fs.FS
	Talk   *talk.Service
	ICE    talk.ICEConfig
	Mail   *mail.Service

	lim netx.Limiter
}

type ctxKey struct{}
type ctxTok struct{}

func userFrom(r *http.Request) *auth.User {
	u, _ := r.Context().Value(ctxKey{}).(*auth.User)
	return u
}

func tokenFrom(r *http.Request) string {
	t, _ := r.Context().Value(ctxTok{}).(string)
	return t
}

func (s *Server) Handler() http.Handler {
	mux := http.NewServeMux()
	h := mux.HandleFunc

	// --- авторизация
	h("GET /api/auth/status", s.authStatus)
	h("POST /api/auth/register", s.register)
	h("POST /api/auth/login", s.login)
	h("POST /api/auth/totp", s.loginTOTP)
	h("POST /api/auth/logout", s.logout)
	h("GET /api/auth/oidc/start", s.oidcStart)
	h("GET /api/auth/oidc/callback", s.oidcCallback)

	// --- профиль и безопасность
	h("GET /api/me", s.need(s.me))
	h("POST /api/me/password", s.need(s.changePassword))
	h("GET /api/me/app-passwords", s.need(s.listAppPasswords))
	h("POST /api/me/app-passwords", s.need(s.createAppPassword))
	h("DELETE /api/me/app-passwords/{id}", s.need(s.deleteAppPassword))
	h("GET /api/me/sessions", s.need(s.listSessions))
	h("DELETE /api/me/sessions/{id}", s.need(s.deleteSession))
	h("DELETE /api/me/sessions", s.need(s.deleteOtherSessions))
	h("GET /api/me/logins", s.need(s.listLogins))
	h("POST /api/me/totp/setup", s.need(s.totpSetup))
	h("POST /api/me/totp/enable", s.need(s.totpEnable))
	h("POST /api/me/totp/disable", s.need(s.totpDisable))

	// --- файлы
	h("GET /api/files", s.need(s.listFiles))
	h("GET /api/files/{id}", s.need(s.getFile))
	h("POST /api/files/folder", s.need(s.mkdir))
	h("POST /api/files/upload", s.need(s.upload))
	h("GET /api/files/{id}/content", s.need(s.content))
	h("PUT /api/files/{id}/content", s.need(s.saveContent))
	h("GET /api/files/{id}/thumb", s.need(s.thumb))
	h("GET /api/files/{id}/zip", s.need(s.zip))
	h("PATCH /api/files/{id}", s.need(s.moveFile))
	h("POST /api/files/{id}/copy", s.need(s.copyFile))
	h("DELETE /api/files/{id}", s.need(s.trashFile))
	h("GET /api/files/{id}/versions", s.need(s.versions))
	h("POST /api/files/{id}/versions/{vid}/restore", s.need(s.restoreVersion))
	h("GET /api/files/{id}/access", s.need(s.fileAccess))
	h("PUT /api/files/{id}/favorite", s.need(s.setFavorite(true)))
	h("DELETE /api/files/{id}/favorite", s.need(s.setFavorite(false)))
	h("PUT /api/files/{id}/tags", s.need(s.setNodeTags))
	h("GET /api/files/{id}/comments", s.need(s.listComments))
	h("POST /api/files/{id}/comments", s.need(s.addComment))
	h("DELETE /api/comments/{cid}", s.need(s.deleteComment))
	h("GET /api/search", s.need(s.search))
	h("GET /api/recent", s.need(s.recent))
	h("GET /api/favorites", s.need(s.favorites))
	h("GET /api/gallery", s.need(s.gallery))
	h("GET /api/notes", s.need(s.notes))
	h("GET /api/sync/tree", s.need(s.syncTree))

	// --- докачка
	h("POST /api/uploads", s.need(s.createUpload))
	h("GET /api/uploads/{id}", s.need(s.getUpload))
	h("PUT /api/uploads/{id}", s.need(s.appendUpload))
	h("POST /api/uploads/{id}/complete", s.need(s.completeUpload))
	h("DELETE /api/uploads/{id}", s.need(s.deleteUpload))

	// --- теги
	h("GET /api/tags", s.need(s.listTags))
	h("POST /api/tags", s.need(s.createTag))
	h("DELETE /api/tags/{id}", s.need(s.deleteTag))
	h("GET /api/tags/{id}/files", s.need(s.taggedFiles))

	// --- корзина
	h("GET /api/trash", s.need(s.listTrash))
	h("POST /api/trash/{id}/restore", s.need(s.restoreTrash))
	h("DELETE /api/trash/{id}", s.need(s.purge))
	h("DELETE /api/trash", s.need(s.emptyTrash))

	// --- публичные ссылки и общий доступ
	h("GET /api/shares", s.need(s.listShares))
	h("POST /api/shares", s.need(s.createShare))
	h("DELETE /api/shares/{id}", s.need(s.deleteShare))
	h("POST /api/user-shares", s.need(s.createUserShare))
	h("PATCH /api/user-shares/{id}", s.need(s.updateUserShare))
	h("DELETE /api/user-shares/{id}", s.need(s.deleteUserShare))
	h("GET /api/shared-with-me", s.need(s.sharedWithMe))
	h("GET /api/directory", s.need(s.directory))

	// --- журнал и уведомления
	h("GET /api/activity", s.need(s.activity))
	h("GET /api/notifications", s.need(s.listNotifications))
	h("POST /api/notifications/read", s.need(s.readNotifications))
	h("DELETE /api/notifications", s.need(s.clearNotifications))

	// --- календари и контакты
	h("GET /api/pim", s.need(s.pimInfo))
	h("GET /api/calendars", s.need(s.listCalendars))
	h("POST /api/calendars", s.need(s.createCalendar))
	h("DELETE /api/calendars/{id}", s.need(s.deleteCollection))
	h("GET /api/events", s.need(s.listEvents))
	h("POST /api/events", s.need(s.createEvent))
	h("DELETE /api/events/{id}", s.need(s.deleteObject))
	h("GET /api/contacts", s.need(s.listContacts))
	h("POST /api/contacts", s.need(s.createContact))
	h("DELETE /api/contacts/{id}", s.need(s.deleteObject))

	// --- чат и звонки
	h("GET /api/talk/events", s.need(s.talkEvents))
	h("GET /api/talk/config", s.need(s.talkConfig))
	h("GET /api/talk/rooms", s.need(s.talkRooms))
	h("POST /api/talk/rooms", s.need(s.talkCreateRoom))
	h("POST /api/talk/direct", s.need(s.talkDirect))
	h("GET /api/talk/rooms/{id}", s.need(s.talkRoom))
	h("PATCH /api/talk/rooms/{id}", s.need(s.talkRename))
	h("POST /api/talk/rooms/{id}/members", s.need(s.talkAddMembers))
	h("DELETE /api/talk/rooms/{id}/members/{uid}", s.need(s.talkRemoveMember))
	h("GET /api/talk/rooms/{id}/messages", s.need(s.talkMessages))
	h("POST /api/talk/rooms/{id}/messages", s.need(s.talkPost))
	h("POST /api/talk/rooms/{id}/read", s.need(s.talkRead))
	h("POST /api/talk/rooms/{id}/call/join", s.need(s.talkJoin))
	h("POST /api/talk/rooms/{id}/call/leave", s.need(s.talkLeave))
	h("POST /api/talk/rooms/{id}/call/signal", s.need(s.talkSignal))

	// --- сквозное шифрование (E2E-папки)
	h("GET /api/e2e/keys", s.need(s.e2eKeys))
	h("PUT /api/e2e/keys", s.need(s.e2ePutKeys))
	h("DELETE /api/e2e/keys", s.need(s.e2eDeleteKeys))
	h("POST /api/e2e/folders", s.need(s.e2eCreateFolder))
	h("GET /api/e2e/meta/{id}", s.need(s.e2eMeta))
	h("PUT /api/e2e/meta/{id}", s.need(s.e2ePutMeta))

	// --- почта
	h("GET /api/mail", s.need(s.mailInfo))
	h("GET /api/mail/preset", s.need(s.mailPreset))
	h("GET /api/mail/accounts", s.need(s.mailAccounts))
	h("POST /api/mail/accounts", s.need(s.mailAddAccount))
	h("DELETE /api/mail/accounts/{id}", s.need(s.mailDeleteAccount))
	h("GET /api/mail/accounts/{id}/folders", s.need(s.mailFolders))
	h("GET /api/mail/accounts/{id}/messages", s.need(s.mailMessages))
	h("GET /api/mail/accounts/{id}/message", s.need(s.mailMessage))
	h("GET /api/mail/accounts/{id}/attachment", s.need(s.mailAttachment))
	h("POST /api/mail/accounts/{id}/attachment/save", s.need(s.mailSaveAttachment))
	h("POST /api/mail/accounts/{id}/flag", s.need(s.mailFlag))
	h("POST /api/mail/accounts/{id}/move", s.need(s.mailMove))
	h("POST /api/mail/accounts/{id}/delete", s.need(s.mailDelete))
	h("POST /api/mail/accounts/{id}/send", s.need(s.mailSend))

	// --- администрирование
	h("GET /api/admin/users", s.admin(s.adminListUsers))
	h("POST /api/admin/users", s.admin(s.adminCreateUser))
	h("PATCH /api/admin/users/{id}", s.admin(s.adminUpdateUser))
	h("DELETE /api/admin/users/{id}", s.admin(s.adminDeleteUser))
	h("GET /api/admin/groups", s.admin(s.adminListGroups))
	h("POST /api/admin/groups", s.admin(s.adminCreateGroup))
	h("DELETE /api/admin/groups/{id}", s.admin(s.adminDeleteGroup))
	h("PUT /api/admin/groups/{id}/members", s.admin(s.adminSetMembers))

	// --- публичный доступ по ссылке
	h("GET /api/public/{token}", s.pubInfo)
	h("POST /api/public/{token}/auth", s.pubAuth)
	h("GET /api/public/{token}/list", s.pubList)
	h("GET /api/public/{token}/content", s.pubContent)
	h("GET /api/public/{token}/thumb", s.pubThumb)
	h("GET /api/public/{token}/zip", s.pubZip)
	h("POST /api/public/{token}/upload", s.pubUpload)
	h("GET /s/{token}", s.sharePage)

	// --- онлайн-редактор
	h("GET /office/{id}", s.officePage)

	// --- служебное
	h("GET /healthz", func(w http.ResponseWriter, r *http.Request) { w.Write([]byte("ok")) })
	mux.Handle("GET /metrics", s.metricsGuard(metrics.Handler(s.DB, s.Cfg.MetricsToken)))
	mux.Handle("GET /", s.static())

	// WebDAV/CalDAV/CardDAV/WOPI используют свои методы (PROPFIND, REPORT, …) — направляем их в обход mux.
	root := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		p := r.URL.Path
		switch {
		case s.DAV != nil && (p == "/dav" || strings.HasPrefix(p, "/dav/")):
			s.DAV.ServeHTTP(w, r)
		case s.PIMDAV != nil && (strings.HasPrefix(p, "/caldav") || strings.HasPrefix(p, "/carddav") ||
			p == "/.well-known/caldav" || p == "/.well-known/carddav"):
			s.PIMDAV.ServeHTTP(w, r)
		case s.WOPI.Enabled() && strings.HasPrefix(p, "/wopi/files/"):
			s.WOPI.ServeHTTP(w, r)
		case p == "/.well-known/webdav" || p == "/remote.php/webdav" || strings.HasPrefix(p, "/remote.php/webdav/"):
			http.Redirect(w, r, "/dav/", http.StatusMovedPermanently)
		default:
			mux.ServeHTTP(w, r)
		}
	})
	return s.middleware(root)
}

func (s *Server) metricsGuard(h http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if s.Cfg.MetricsToken == "" {
			// без токена — только из локальной/docker-сети (Prometheus рядом)
			host, _, _ := net.SplitHostPort(r.RemoteAddr)
			ip := net.ParseIP(host)
			if ip == nil || !(ip.IsLoopback() || ip.IsPrivate()) || r.Header.Get("X-Forwarded-For") != "" {
				http.NotFound(w, r)
				return
			}
		}
		h.ServeHTTP(w, r)
	})
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
		switch {
		case r.URL.Path == "/" || strings.HasSuffix(r.URL.Path, ".js") || strings.HasSuffix(r.URL.Path, ".css") ||
			strings.HasSuffix(r.URL.Path, ".webmanifest"):
			w.Header().Set("Cache-Control", "no-cache")
		}
		if strings.HasSuffix(r.URL.Path, ".webmanifest") {
			w.Header().Set("Content-Type", "application/manifest+json")
		}
		if r.URL.Path == "/sw.js" {
			w.Header().Set("Service-Worker-Allowed", "/")
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
	code  int
	bytes int64
}

func (w *statusWriter) WriteHeader(c int) { w.code = c; w.ResponseWriter.WriteHeader(c) }
func (w *statusWriter) Write(b []byte) (int, error) {
	n, err := w.ResponseWriter.Write(b)
	w.bytes += int64(n)
	return n, err
}
func (w *statusWriter) Flush() {
	if f, ok := w.ResponseWriter.(http.Flusher); ok {
		f.Flush()
	}
}

func routeOf(p string) string {
	switch {
	case strings.HasPrefix(p, "/api/public/"), strings.HasPrefix(p, "/s/"):
		return "public"
	case strings.HasPrefix(p, "/api/"):
		return "api"
	case strings.HasPrefix(p, "/dav"):
		return "webdav"
	case strings.HasPrefix(p, "/caldav"), strings.HasPrefix(p, "/carddav"), strings.HasPrefix(p, "/.well-known/ca"):
		return "pim"
	case strings.HasPrefix(p, "/wopi/"), strings.HasPrefix(p, "/office/"):
		return "office"
	}
	return "static"
}

func (s *Server) middleware(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		start := time.Now()
		h := w.Header()
		h.Set("X-Content-Type-Options", "nosniff")
		h.Set("Referrer-Policy", "same-origin")
		route := routeOf(r.URL.Path)
		if route == "api" || route == "public" || route == "static" {
			h.Set("X-Frame-Options", "SAMEORIGIN")
		}

		// Защита от CSRF: изменяющие запросы к API только с нашего Origin.
		if strings.HasPrefix(r.URL.Path, "/api/") && r.Method != http.MethodGet && r.Method != http.MethodHead {
			if o := r.Header.Get("Origin"); o != "" && !s.sameOrigin(r, o) {
				writeErr(w, http.StatusForbidden, "запрос с чужого сайта")
				return
			}
		}

		if route == "api" || route == "static" || route == "office" {
			if u, tok := s.sessionUser(r); u != nil {
				ctx := context.WithValue(r.Context(), ctxKey{}, u)
				ctx = context.WithValue(ctx, ctxTok{}, tok)
				r = r.WithContext(ctx)
			}
		}
		sw := &statusWriter{ResponseWriter: w, code: 200}
		next.ServeHTTP(sw, r)
		d := time.Since(start)
		metrics.Observe(route, r.Method, sw.code, d)
		if route != "static" || sw.code >= 400 {
			attrs := []any{"method", r.Method, "path", r.URL.Path, "status", sw.code, "ms", d.Milliseconds(), "ip", clientIP(r)}
			if u := userFrom(r); u != nil {
				attrs = append(attrs, "user", u.Username)
			}
			if sw.code >= 500 && sw.code != http.StatusInsufficientStorage {
				slog.Error("http", attrs...)
			} else {
				slog.Info("http", attrs...)
			}
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

// sessionUser: cookie сессии, Bearer-токен или Basic (пароль приложения — для клиента синхронизации).
func (s *Server) sessionUser(r *http.Request) (*auth.User, string) {
	tok := ""
	if c, err := r.Cookie(sessionCookie); err == nil {
		tok = c.Value
	}
	hdr := r.Header.Get("Authorization")
	if strings.HasPrefix(hdr, "Bearer ") {
		tok = strings.TrimPrefix(hdr, "Bearer ")
	}
	if tok != "" {
		if u, err := s.Auth.UserBySession(r.Context(), tok); err == nil {
			return u, tok
		}
		return nil, ""
	}
	if strings.HasPrefix(hdr, "Basic ") && strings.HasPrefix(r.URL.Path, "/api/") {
		// Считаем только неудачные попытки, как в WebDAV: клиент синхронизации с паролем приложения
		// шлёт сотни запросов в минуту, и лимит на все запросы отрезал бы его посреди синхронизации.
		if user, pass, ok := r.BasicAuth(); ok {
			key := "basic-fail|" + clientIP(r)
			if s.lim.Blocked(key, basicMaxFails, basicFailWindow) {
				return nil, ""
			}
			if u, err := s.Auth.CheckBasic(r.Context(), user, pass); err == nil {
				return u, ""
			}
			s.lim.Fail(key, basicFailWindow)
		}
	}
	return nil, ""
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

// ---------- права на узлы ----------

// acc проверяет доступ текущего пользователя к узлу. Ответ уже отправлен, если ok == false.
func (s *Server) acc(w http.ResponseWriter, r *http.Request, id string, needWrite bool) (*files.Perm, *files.Node, bool) {
	perm, n, err := s.Files.Access(r.Context(), userFrom(r).ID, id)
	if err != nil {
		fail(w, err)
		return nil, nil, false
	}
	if needWrite && !perm.Write {
		fail(w, files.ErrForbidden)
		return nil, nil, false
	}
	return perm, n, true
}

// parentAcc — папка, в которой что-то создают ("" или "root" — свой корень). Нужны права на запись.
func (s *Server) parentAcc(w http.ResponseWriter, r *http.Request, id string) (*files.Perm, *string, bool) {
	if id == "" || id == "root" {
		return files.OwnPerm(userFrom(r).ID), nil, true
	}
	perm, n, ok := s.acc(w, r, id, true)
	if !ok {
		return nil, nil, false
	}
	if !n.IsDir {
		fail(w, files.ErrNotDir)
		return nil, nil, false
	}
	return perm, &n.ID, true
}

func (s *Server) logAct(r *http.Request, owner int64, action string, n *files.Node, details string) {
	u := userFrom(r)
	if u == nil {
		s.Files.Log(r.Context(), owner, nil, "гость", action, n, details, clientIP(r))
		return
	}
	id := u.ID
	s.Files.Log(r.Context(), owner, &id, u.Username, action, n, details, clientIP(r))
}

// ---------- ограничение частоты ----------

// allow — не более max попыток за окно window с одного IP для данного действия.
func (s *Server) allow(r *http.Request, action string, max int, window time.Duration) bool {
	return s.lim.Allow(action+"|"+clientIP(r), max, window)
}

// clientIP — адрес клиента с учётом доверенных прокси (CB_TRUSTED_PROXIES).
func clientIP(r *http.Request) string { return netx.ClientIP(r) }

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

func okJSON(w http.ResponseWriter) { writeJSON(w, 200, map[string]bool{"ok": true}) }

// fail переводит доменные ошибки в HTTP-коды.
func fail(w http.ResponseWriter, err error) {
	switch {
	case errors.Is(err, files.ErrNotFound), errors.Is(err, auth.ErrNotFound), errors.Is(err, pim.ErrNotFound), errors.Is(err, talk.ErrNotFound),
		errors.Is(err, mail.ErrNotFound), errors.Is(err, mail.ErrNoSuchMsg), errors.Is(err, mail.ErrNoAttachment):
		writeErr(w, http.StatusNotFound, err.Error())
	case errors.Is(err, files.ErrExists), errors.Is(err, auth.ErrUserExists), errors.Is(err, files.ErrShared), errors.Is(err, files.ErrTagExists):
		writeErr(w, http.StatusConflict, err.Error())
	case errors.Is(err, files.ErrConflict):
		writeErr(w, http.StatusPreconditionFailed, err.Error())
	case errors.Is(err, files.ErrLocked):
		writeErr(w, http.StatusLocked, err.Error())
	case errors.Is(err, files.ErrQuota):
		writeErr(w, http.StatusInsufficientStorage, err.Error())
	case errors.Is(err, files.ErrTooLarge):
		writeErr(w, http.StatusRequestEntityTooLarge, err.Error())
	case errors.Is(err, files.ErrForbidden), errors.Is(err, talk.ErrForbidden):
		writeErr(w, http.StatusForbidden, err.Error())
	case errors.Is(err, files.ErrOffset):
		writeErr(w, http.StatusConflict, err.Error())
	case errors.Is(err, files.ErrNotDir), errors.Is(err, files.ErrIsDir), errors.Is(err, files.ErrInvalidName),
		errors.Is(err, files.ErrCycle), errors.Is(err, files.ErrCrossOwner),
		errors.Is(err, auth.ErrBadUsername), errors.Is(err, auth.ErrWeakPassword), errors.Is(err, auth.ErrBadCode):
		writeErr(w, http.StatusBadRequest, err.Error())
	case errors.Is(err, auth.ErrBadCredentials):
		writeErr(w, http.StatusUnauthorized, err.Error())
	case errors.Is(err, files.ErrNoThumb):
		writeErr(w, http.StatusNotFound, err.Error())
	default:
		var ve interface{ Error() string } = err
		msg := ve.Error()
		// ошибки валидации из сервисов (на русском) отдаём как 400, остальное — 500
		if isUserMessage(msg) {
			writeErr(w, http.StatusBadRequest, msg)
			return
		}
		slog.Error("internal error", "err", err)
		writeErr(w, http.StatusInternalServerError, "внутренняя ошибка сервера")
	}
}

// isUserMessage — сообщение на кириллице без технических деталей.
func isUserMessage(m string) bool {
	if m == "" || strings.Contains(m, "pq:") || strings.Contains(m, "sql") {
		return false
	}
	for _, r := range m {
		if r >= 'а' && r <= 'я' || r >= 'А' && r <= 'Я' {
			return true
		}
	}
	return false
}
