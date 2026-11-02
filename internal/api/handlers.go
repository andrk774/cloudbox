package api

import (
	"archive/zip"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"path"
	"strconv"
	"strings"
	"time"

	"github.com/andrk774/cloudbox/internal/auth"
	"github.com/andrk774/cloudbox/internal/files"
	"github.com/andrk774/cloudbox/internal/storage"
)

// ---------- авторизация ----------

func (s *Server) authStatus(w http.ResponseWriter, r *http.Request) {
	n, err := s.Auth.CountUsers(r.Context())
	if err != nil {
		fail(w, err)
		return
	}
	writeJSON(w, 200, map[string]any{
		"registration_open": s.Cfg.AllowRegistration || n == 0,
		"first_user":        n == 0,
		"logged_in":         userFrom(r) != nil,
	})
}

type credentials struct {
	Username string `json:"username"`
	Password string `json:"password"`
}

func (s *Server) register(w http.ResponseWriter, r *http.Request) {
	if !s.allow(r, "register", 10, time.Hour) {
		writeErr(w, http.StatusTooManyRequests, "слишком много попыток, попробуйте позже")
		return
	}
	n, err := s.Auth.CountUsers(r.Context())
	if err != nil {
		fail(w, err)
		return
	}
	if n > 0 && !s.Cfg.AllowRegistration {
		writeErr(w, http.StatusForbidden, "регистрация закрыта — попросите администратора создать учётную запись")
		return
	}
	var c credentials
	if err := readJSON(r, &c); err != nil {
		writeErr(w, 400, err.Error())
		return
	}
	u, err := s.Auth.CreateUser(r.Context(), c.Username, c.Password, false, -1)
	if err != nil {
		fail(w, err)
		return
	}
	s.startSession(w, r, u)
}

func (s *Server) login(w http.ResponseWriter, r *http.Request) {
	if !s.allow(r, "login", 10, 5*time.Minute) {
		writeErr(w, http.StatusTooManyRequests, "слишком много попыток входа, подождите 5 минут")
		return
	}
	var c credentials
	if err := readJSON(r, &c); err != nil {
		writeErr(w, 400, err.Error())
		return
	}
	u, err := s.Auth.Authenticate(r.Context(), c.Username, c.Password)
	if err != nil {
		fail(w, err)
		return
	}
	s.startSession(w, r, u)
}

func (s *Server) startSession(w http.ResponseWriter, r *http.Request, u *auth.User) {
	tok, err := s.Auth.CreateSession(r.Context(), u.ID, r.UserAgent())
	if err != nil {
		fail(w, err)
		return
	}
	http.SetCookie(w, &http.Cookie{
		Name: sessionCookie, Value: tok, Path: "/", HttpOnly: true,
		Secure: strings.HasPrefix(s.Cfg.PublicURL, "https://"), SameSite: http.SameSiteLaxMode,
		MaxAge: int(auth.SessionTTL.Seconds()),
	})
	writeJSON(w, 200, map[string]any{"user": u, "token": tok})
}

func (s *Server) logout(w http.ResponseWriter, r *http.Request) {
	if c, err := r.Cookie(sessionCookie); err == nil {
		s.Auth.DeleteSession(r.Context(), c.Value)
	}
	http.SetCookie(w, &http.Cookie{Name: sessionCookie, Value: "", Path: "/", MaxAge: -1, HttpOnly: true})
	writeJSON(w, 200, map[string]bool{"ok": true})
}

// ---------- профиль ----------

func (s *Server) me(w http.ResponseWriter, r *http.Request) {
	u := userFrom(r)
	used, quota, err := s.Files.Usage(r.Context(), u.ID)
	if err != nil {
		fail(w, err)
		return
	}
	writeJSON(w, 200, map[string]any{
		"user": u, "used_bytes": used, "quota_bytes": quota,
		"webdav_url":       s.Cfg.PublicURL + "/dav/",
		"max_upload_bytes": s.Cfg.MaxUploadMB << 20,
	})
}

func (s *Server) changePassword(w http.ResponseWriter, r *http.Request) {
	u := userFrom(r)
	var req struct {
		Old string `json:"old_password"`
		New string `json:"new_password"`
	}
	if err := readJSON(r, &req); err != nil {
		writeErr(w, 400, err.Error())
		return
	}
	if _, err := s.Auth.Authenticate(r.Context(), u.Username, req.Old); err != nil {
		writeErr(w, 400, "текущий пароль указан неверно")
		return
	}
	if err := s.Auth.SetPassword(r.Context(), u.ID, req.New); err != nil {
		fail(w, err)
		return
	}
	s.startSession(w, r, u) // старые сессии сброшены — выдаём новую
}

func (s *Server) listAppPasswords(w http.ResponseWriter, r *http.Request) {
	list, err := s.Auth.ListAppPasswords(r.Context(), userFrom(r).ID)
	if err != nil {
		fail(w, err)
		return
	}
	writeJSON(w, 200, list)
}

func (s *Server) createAppPassword(w http.ResponseWriter, r *http.Request) {
	var req struct {
		Name string `json:"name"`
	}
	if err := readJSON(r, &req); err != nil {
		writeErr(w, 400, err.Error())
		return
	}
	plain, ap, err := s.Auth.CreateAppPassword(r.Context(), userFrom(r).ID, req.Name)
	if err != nil {
		writeErr(w, 400, err.Error())
		return
	}
	writeJSON(w, 201, map[string]any{"password": plain, "app_password": ap})
}

func (s *Server) deleteAppPassword(w http.ResponseWriter, r *http.Request) {
	id, _ := strconv.ParseInt(r.PathValue("id"), 10, 64)
	if err := s.Auth.DeleteAppPassword(r.Context(), userFrom(r).ID, id); err != nil {
		fail(w, err)
		return
	}
	writeJSON(w, 200, map[string]bool{"ok": true})
}

// ---------- файлы ----------

// parentParam: "" или "root" — корень.
func parentParam(v string) *string {
	if v == "" || v == "root" {
		return nil
	}
	return &v
}

func (s *Server) listFiles(w http.ResponseWriter, r *http.Request) {
	u := userFrom(r)
	parent := parentParam(r.URL.Query().Get("parent"))
	items, err := s.Files.List(r.Context(), u.ID, parent)
	if err != nil {
		fail(w, err)
		return
	}
	crumbs := []files.Node{}
	if parent != nil {
		if crumbs, err = s.Files.Ancestors(r.Context(), u.ID, *parent); err != nil {
			fail(w, err)
			return
		}
	}
	writeJSON(w, 200, map[string]any{"items": items, "breadcrumbs": crumbs})
}

func (s *Server) getFile(w http.ResponseWriter, r *http.Request) {
	n, err := s.Files.Get(r.Context(), userFrom(r).ID, r.PathValue("id"))
	if err != nil {
		fail(w, err)
		return
	}
	writeJSON(w, 200, n)
}

func (s *Server) mkdir(w http.ResponseWriter, r *http.Request) {
	var req struct {
		Parent string `json:"parent"`
		Name   string `json:"name"`
	}
	if err := readJSON(r, &req); err != nil {
		writeErr(w, 400, err.Error())
		return
	}
	n, err := s.Files.Mkdir(r.Context(), userFrom(r).ID, parentParam(req.Parent), strings.TrimSpace(req.Name))
	if err != nil {
		fail(w, err)
		return
	}
	writeJSON(w, 201, n)
}

// upload: POST /api/files/upload?parent=ID&name=файл.txt[&path=папка/подпапка][&conflict=rename]
// Тело запроса — сырые байты файла (так проще показывать прогресс и не нужен multipart).
func (s *Server) upload(w http.ResponseWriter, r *http.Request) {
	s.doUpload(w, r, userFrom(r).ID, parentParam(r.URL.Query().Get("parent")))
}

func (s *Server) doUpload(w http.ResponseWriter, r *http.Request, uid int64, parent *string) {
	q := r.URL.Query()
	name := q.Get("name")
	if err := files.ValidateName(name); err != nil {
		fail(w, err)
		return
	}
	if rel := strings.Trim(q.Get("path"), "/"); rel != "" {
		var err error
		if parent, err = s.Files.EnsureDirs(r.Context(), uid, parent, rel); err != nil {
			fail(w, err)
			return
		}
	}
	if q.Get("conflict") == "rename" {
		var err error
		if name, err = s.Files.FreeName(r.Context(), uid, parent, name); err != nil {
			fail(w, err)
			return
		}
	}
	r.Body = http.MaxBytesReader(w, r.Body, (s.Cfg.MaxUploadMB<<20)+1)
	n, err := s.Files.Write(r.Context(), uid, parent, name, r.Body)
	var mbe *http.MaxBytesError
	if errors.As(err, &mbe) {
		err = files.ErrTooLarge
	}
	if err != nil {
		fail(w, err)
		return
	}
	writeJSON(w, 201, n)
}

func (s *Server) content(w http.ResponseWriter, r *http.Request) {
	b, n, err := s.Files.Open(r.Context(), userFrom(r).ID, r.PathValue("id"), r.URL.Query().Get("version"))
	if err != nil {
		fail(w, err)
		return
	}
	serveBlob(w, r, b, n, r.URL.Query().Get("inline") == "1")
}

// inlineSafe — типы, которые можно открыть прямо в браузере без риска XSS.
func inlineSafe(m string) bool {
	switch {
	case strings.HasPrefix(m, "image/") && m != "image/svg+xml":
		return true
	case strings.HasPrefix(m, "video/"), strings.HasPrefix(m, "audio/"), m == "application/pdf":
		return true
	case strings.HasPrefix(m, "text/plain"):
		return true
	}
	return false
}

func contentDisposition(kind, name string) string {
	ascii := strings.Map(func(r rune) rune {
		if r < 0x20 || r > 0x7e || r == '"' || r == '\\' {
			return '_'
		}
		return r
	}, name)
	return fmt.Sprintf(`%s; filename="%s"; filename*=UTF-8''%s`, kind, ascii, url.PathEscape(name))
}

func serveBlob(w http.ResponseWriter, r *http.Request, b storage.Blob, n *files.Node, inline bool) {
	defer b.Close()
	h := w.Header()
	ctype := n.Mime
	if ctype == "" {
		ctype = "application/octet-stream"
	}
	if inline && inlineSafe(ctype) {
		h.Set("Content-Disposition", contentDisposition("inline", n.Name))
	} else {
		h.Set("Content-Disposition", contentDisposition("attachment", n.Name))
		if !inlineSafe(ctype) {
			ctype = "application/octet-stream"
		}
	}
	h.Set("Content-Type", ctype)
	h.Set("Content-Security-Policy", "sandbox; default-src 'none'; img-src 'self'; media-src 'self'; style-src 'unsafe-inline'")
	h.Set("Cache-Control", "private, max-age=0, must-revalidate")
	if n.VersionID != nil {
		h.Set("ETag", `"`+*n.VersionID+`"`)
	}
	http.ServeContent(w, r, n.Name, n.UpdatedAt, b)
}

func (s *Server) zip(w http.ResponseWriter, r *http.Request) {
	u := userFrom(r)
	n, err := s.Files.Get(r.Context(), u.ID, r.PathValue("id"))
	if err != nil {
		fail(w, err)
		return
	}
	s.writeZip(w, r, u.ID, n)
}

func (s *Server) writeZip(w http.ResponseWriter, r *http.Request, uid int64, root *files.Node) {
	w.Header().Set("Content-Type", "application/zip")
	w.Header().Set("Content-Disposition", contentDisposition("attachment", root.Name+".zip"))
	zw := zip.NewWriter(w)
	defer zw.Close()
	var walk func(n *files.Node, prefix string) error
	walk = func(n *files.Node, prefix string) error {
		p := path.Join(prefix, n.Name)
		if !n.IsDir {
			b, _, err := s.Files.Open(r.Context(), uid, n.ID, "")
			if err != nil {
				return err
			}
			defer b.Close()
			method := zip.Deflate
			if strings.HasPrefix(n.Mime, "image/") || strings.HasPrefix(n.Mime, "video/") || strings.HasPrefix(n.Mime, "audio/") ||
				strings.Contains(n.Mime, "zip") || strings.Contains(n.Mime, "compressed") {
				method = zip.Store
			}
			fw, err := zw.CreateHeader(&zip.FileHeader{Name: p, Method: method, Modified: n.UpdatedAt})
			if err != nil {
				return err
			}
			_, err = io.Copy(fw, b)
			return err
		}
		if _, err := zw.CreateHeader(&zip.FileHeader{Name: p + "/", Modified: n.UpdatedAt}); err != nil {
			return err
		}
		children, err := s.Files.List(r.Context(), uid, &n.ID)
		if err != nil {
			return err
		}
		for i := range children {
			if err := walk(&children[i], p); err != nil {
				return err
			}
		}
		return nil
	}
	if err := walk(root, ""); err != nil {
		// заголовки уже отправлены — можем только оборвать архив
		zw.SetComment("ERROR: " + err.Error())
	}
}

// moveFile: PATCH {"name": "...", "parent": null | "id"} — переименование и/или перемещение.
func (s *Server) moveFile(w http.ResponseWriter, r *http.Request) {
	u := userFrom(r)
	n, err := s.Files.Get(r.Context(), u.ID, r.PathValue("id"))
	if err != nil {
		fail(w, err)
		return
	}
	var req map[string]json.RawMessage
	if err := readJSON(r, &req); err != nil {
		writeErr(w, 400, err.Error())
		return
	}
	name, parent := n.Name, n.ParentID
	if raw, ok := req["name"]; ok {
		if err := json.Unmarshal(raw, &name); err != nil {
			writeErr(w, 400, "name должен быть строкой")
			return
		}
		name = strings.TrimSpace(name)
	}
	if raw, ok := req["parent"]; ok {
		var p *string
		if err := json.Unmarshal(raw, &p); err != nil {
			writeErr(w, 400, "parent должен быть строкой или null")
			return
		}
		if p != nil && (*p == "" || *p == "root") {
			p = nil
		}
		parent = p
	}
	res, err := s.Files.Move(r.Context(), u.ID, n.ID, parent, name)
	if err != nil {
		fail(w, err)
		return
	}
	writeJSON(w, 200, res)
}

func (s *Server) copyFile(w http.ResponseWriter, r *http.Request) {
	u := userFrom(r)
	src, err := s.Files.Get(r.Context(), u.ID, r.PathValue("id"))
	if err != nil {
		fail(w, err)
		return
	}
	var req struct {
		Parent *string `json:"parent"`
		Name   string  `json:"name"`
	}
	if err := readJSON(r, &req); err != nil {
		writeErr(w, 400, err.Error())
		return
	}
	parent := req.Parent
	if parent != nil {
		parent = parentParam(*parent)
	}
	name := strings.TrimSpace(req.Name)
	if name == "" {
		if name, err = s.Files.FreeName(r.Context(), u.ID, parent, src.Name); err != nil {
			fail(w, err)
			return
		}
	}
	n, err := s.Files.Copy(r.Context(), u.ID, src.ID, parent, name)
	if err != nil {
		fail(w, err)
		return
	}
	writeJSON(w, 201, n)
}

func (s *Server) trashFile(w http.ResponseWriter, r *http.Request) {
	if err := s.Files.Trash(r.Context(), userFrom(r).ID, r.PathValue("id")); err != nil {
		fail(w, err)
		return
	}
	writeJSON(w, 200, map[string]bool{"ok": true})
}

func (s *Server) versions(w http.ResponseWriter, r *http.Request) {
	v, err := s.Files.Versions(r.Context(), userFrom(r).ID, r.PathValue("id"))
	if err != nil {
		fail(w, err)
		return
	}
	writeJSON(w, 200, v)
}

func (s *Server) restoreVersion(w http.ResponseWriter, r *http.Request) {
	n, err := s.Files.RestoreVersion(r.Context(), userFrom(r).ID, r.PathValue("id"), r.PathValue("vid"))
	if err != nil {
		fail(w, err)
		return
	}
	writeJSON(w, 200, n)
}

func (s *Server) search(w http.ResponseWriter, r *http.Request) {
	u := userFrom(r)
	items, err := s.Files.Search(r.Context(), u.ID, r.URL.Query().Get("q"))
	if err != nil {
		fail(w, err)
		return
	}
	type hit struct {
		files.Node
		Path string `json:"path"`
	}
	out := make([]hit, 0, len(items))
	for _, n := range items {
		p := "/"
		if n.ParentID != nil {
			p, _ = s.Files.PathOf(r.Context(), u.ID, *n.ParentID)
		}
		out = append(out, hit{n, p})
	}
	writeJSON(w, 200, out)
}

func (s *Server) recent(w http.ResponseWriter, r *http.Request) {
	items, err := s.Files.Recent(r.Context(), userFrom(r).ID, 50)
	if err != nil {
		fail(w, err)
		return
	}
	writeJSON(w, 200, items)
}

// ---------- корзина ----------

func (s *Server) listTrash(w http.ResponseWriter, r *http.Request) {
	items, err := s.Files.ListTrash(r.Context(), userFrom(r).ID)
	if err != nil {
		fail(w, err)
		return
	}
	writeJSON(w, 200, map[string]any{"items": items, "retention_days": s.Cfg.TrashDays})
}

func (s *Server) restoreTrash(w http.ResponseWriter, r *http.Request) {
	n, err := s.Files.Restore(r.Context(), userFrom(r).ID, r.PathValue("id"))
	if err != nil {
		fail(w, err)
		return
	}
	writeJSON(w, 200, n)
}

func (s *Server) purge(w http.ResponseWriter, r *http.Request) {
	if err := s.Files.Purge(r.Context(), userFrom(r).ID, r.PathValue("id")); err != nil {
		fail(w, err)
		return
	}
	writeJSON(w, 200, map[string]bool{"ok": true})
}

func (s *Server) emptyTrash(w http.ResponseWriter, r *http.Request) {
	if err := s.Files.EmptyTrash(r.Context(), userFrom(r).ID); err != nil {
		fail(w, err)
		return
	}
	writeJSON(w, 200, map[string]bool{"ok": true})
}

// ---------- ссылки ----------

type shareOut struct {
	files.Share
	URL string `json:"url"`
}

func (s *Server) shareURL(sh files.Share) shareOut {
	return shareOut{sh, s.Cfg.PublicURL + "/s/" + sh.Token}
}

func (s *Server) listShares(w http.ResponseWriter, r *http.Request) {
	list, err := s.Files.ListShares(r.Context(), userFrom(r).ID, r.URL.Query().Get("node"))
	if err != nil {
		fail(w, err)
		return
	}
	out := make([]shareOut, 0, len(list))
	for _, sh := range list {
		out = append(out, s.shareURL(sh))
	}
	writeJSON(w, 200, out)
}

func (s *Server) createShare(w http.ResponseWriter, r *http.Request) {
	var req struct {
		NodeID      string     `json:"node_id"`
		Password    string     `json:"password"`
		ExpiresAt   *time.Time `json:"expires_at"`
		AllowUpload bool       `json:"allow_upload"`
	}
	if err := readJSON(r, &req); err != nil {
		writeErr(w, 400, "некорректный запрос (дата — в формате RFC 3339)")
		return
	}
	if req.ExpiresAt != nil && req.ExpiresAt.Before(time.Now()) {
		writeErr(w, 400, "дата окончания уже прошла")
		return
	}
	sh, err := s.Files.CreateShare(r.Context(), userFrom(r).ID, req.NodeID, req.Password, req.ExpiresAt, req.AllowUpload)
	if err != nil {
		fail(w, err)
		return
	}
	writeJSON(w, 201, s.shareURL(*sh))
}

func (s *Server) deleteShare(w http.ResponseWriter, r *http.Request) {
	if err := s.Files.DeleteShare(r.Context(), userFrom(r).ID, r.PathValue("id")); err != nil {
		fail(w, err)
		return
	}
	writeJSON(w, 200, map[string]bool{"ok": true})
}

// ---------- администрирование ----------

func (s *Server) adminListUsers(w http.ResponseWriter, r *http.Request) {
	list, err := s.Auth.ListUsers(r.Context())
	if err != nil {
		fail(w, err)
		return
	}
	writeJSON(w, 200, list)
}

func (s *Server) adminCreateUser(w http.ResponseWriter, r *http.Request) {
	var req struct {
		Username   string `json:"username"`
		Password   string `json:"password"`
		IsAdmin    bool   `json:"is_admin"`
		QuotaBytes *int64 `json:"quota_bytes"`
	}
	if err := readJSON(r, &req); err != nil {
		writeErr(w, 400, err.Error())
		return
	}
	quota := int64(-1)
	if req.QuotaBytes != nil {
		quota = *req.QuotaBytes
	}
	u, err := s.Auth.CreateUser(r.Context(), req.Username, req.Password, req.IsAdmin, quota)
	if err != nil {
		fail(w, err)
		return
	}
	writeJSON(w, 201, u)
}

func (s *Server) adminUpdateUser(w http.ResponseWriter, r *http.Request) {
	id, _ := strconv.ParseInt(r.PathValue("id"), 10, 64)
	if _, err := s.Auth.GetUser(r.Context(), id); err != nil {
		fail(w, err)
		return
	}
	var req struct {
		QuotaBytes *int64  `json:"quota_bytes"`
		IsAdmin    *bool   `json:"is_admin"`
		Password   *string `json:"password"`
	}
	if err := readJSON(r, &req); err != nil {
		writeErr(w, 400, err.Error())
		return
	}
	if req.IsAdmin != nil && !*req.IsAdmin && id == userFrom(r).ID {
		writeErr(w, 400, "нельзя снять права администратора с самого себя")
		return
	}
	if req.Password != nil {
		if err := s.Auth.SetPassword(r.Context(), id, *req.Password); err != nil {
			fail(w, err)
			return
		}
	}
	if err := s.Auth.UpdateUser(r.Context(), id, req.QuotaBytes, req.IsAdmin); err != nil {
		fail(w, err)
		return
	}
	u, _ := s.Auth.GetUser(r.Context(), id)
	writeJSON(w, 200, u)
}

func (s *Server) adminDeleteUser(w http.ResponseWriter, r *http.Request) {
	id, _ := strconv.ParseInt(r.PathValue("id"), 10, 64)
	if id == userFrom(r).ID {
		writeErr(w, 400, "нельзя удалить самого себя")
		return
	}
	if err := s.Auth.DeleteUser(r.Context(), id); err != nil {
		fail(w, err)
		return
	}
	go s.Files.GC(context.Background())
	writeJSON(w, 200, map[string]bool{"ok": true})
}
