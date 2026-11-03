package api

import (
	"net/http"
	"time"

	"github.com/andrk774/cloudbox/internal/files"
	"github.com/andrk774/cloudbox/internal/secure"
)

// Доступ по публичной ссылке /s/{token}. Если у ссылки есть пароль, после его ввода
// выдаётся cookie с HMAC(token|hash) — она перестаёт работать при смене пароля ссылки или CB_SECRET.

func (s *Server) shareCookieName(token string) string { return "cb_s_" + token }

func (s *Server) shareCookieValue(sh *files.Share) string {
	return secure.HMAC(s.Cfg.Secret, sh.Token+"|"+sh.PasswordHash)
}

func (s *Server) shareAuthorized(r *http.Request, sh *files.Share) bool {
	if !sh.HasPassword {
		return true
	}
	c, err := r.Cookie(s.shareCookieName(sh.Token))
	return err == nil && secure.Equal(c.Value, s.shareCookieValue(sh))
}

// loadShare достаёт ссылку и проверяет пароль. Возвращает nil, если ответ уже отправлен.
func (s *Server) loadShare(w http.ResponseWriter, r *http.Request, needAuth bool) *files.Share {
	sh, err := s.Files.ShareByToken(r.Context(), r.PathValue("token"))
	if err != nil {
		writeErr(w, http.StatusNotFound, "ссылка не найдена или срок её действия истёк")
		return nil
	}
	if needAuth && !s.shareAuthorized(r, sh) {
		writeErr(w, http.StatusUnauthorized, "нужен пароль")
		return nil
	}
	return sh
}

// shareNode — узел внутри расшаренной папки (или сам расшаренный узел, если id пустой).
func (s *Server) shareNode(w http.ResponseWriter, r *http.Request, sh *files.Share, id string) *files.Node {
	if id == "" {
		id = sh.NodeID
	}
	inside, err := s.Files.IsInside(r.Context(), sh.OwnerID, sh.NodeID, id)
	if err != nil {
		fail(w, err)
		return nil
	}
	if !inside {
		writeErr(w, http.StatusNotFound, "не найдено")
		return nil
	}
	n, err := s.Files.Get(r.Context(), sh.OwnerID, id)
	if err != nil {
		fail(w, err)
		return nil
	}
	if n.E2E { // E2E-папка внутри расшаренной по ссылке: гостю там только шифротекст без ключа
		writeErr(w, http.StatusNotFound, "не найдено")
		return nil
	}
	return n
}

func (s *Server) pubInfo(w http.ResponseWriter, r *http.Request) {
	sh := s.loadShare(w, r, false)
	if sh == nil {
		return
	}
	authorized := s.shareAuthorized(r, sh)
	info := map[string]any{
		"name": sh.NodeName, "is_dir": sh.IsDir, "needs_password": sh.HasPassword,
		"authorized": authorized, "allow_upload": sh.AllowUpload, "expires_at": sh.ExpiresAt,
	}
	if authorized {
		if n, err := s.Files.Get(r.Context(), sh.OwnerID, sh.NodeID); err == nil {
			info["node"] = n
		}
	}
	writeJSON(w, 200, info)
}

func (s *Server) pubAuth(w http.ResponseWriter, r *http.Request) {
	if !s.allow(r, "share-pw", 20, 10*time.Minute) {
		writeErr(w, http.StatusTooManyRequests, "слишком много попыток, подождите")
		return
	}
	sh := s.loadShare(w, r, false)
	if sh == nil {
		return
	}
	var req struct {
		Password string `json:"password"`
	}
	if err := readJSON(r, &req); err != nil {
		writeErr(w, 400, err.Error())
		return
	}
	if sh.HasPassword && !secure.VerifyPassword(sh.PasswordHash, req.Password) {
		writeErr(w, http.StatusUnauthorized, "неверный пароль")
		return
	}
	http.SetCookie(w, &http.Cookie{
		Name: s.shareCookieName(sh.Token), Value: s.shareCookieValue(sh), Path: "/",
		HttpOnly: true, SameSite: http.SameSiteLaxMode, MaxAge: 7 * 24 * 3600,
		Secure: len(s.Cfg.PublicURL) > 8 && s.Cfg.PublicURL[:8] == "https://",
	})
	writeJSON(w, 200, map[string]bool{"ok": true})
}

func (s *Server) pubList(w http.ResponseWriter, r *http.Request) {
	sh := s.loadShare(w, r, true)
	if sh == nil {
		return
	}
	dir := s.shareNode(w, r, sh, r.URL.Query().Get("dir"))
	if dir == nil {
		return
	}
	if !dir.IsDir {
		fail(w, files.ErrNotDir)
		return
	}
	items, err := s.Files.List(r.Context(), sh.OwnerID, &dir.ID)
	if err != nil {
		fail(w, err)
		return
	}
	visible := items[:0]
	for _, it := range items {
		if !it.E2E {
			visible = append(visible, it)
		}
	}
	items = visible
	// «Хлебные крошки» только в пределах расшаренной папки
	anc, err := s.Files.Ancestors(r.Context(), sh.OwnerID, dir.ID)
	if err != nil {
		fail(w, err)
		return
	}
	crumbs := []files.Node{}
	for i, n := range anc {
		if n.ID == sh.NodeID {
			crumbs = anc[i:]
			break
		}
	}
	writeJSON(w, 200, map[string]any{"items": items, "breadcrumbs": crumbs})
}

func (s *Server) pubContent(w http.ResponseWriter, r *http.Request) {
	sh := s.loadShare(w, r, true)
	if sh == nil {
		return
	}
	n := s.shareNode(w, r, sh, r.URL.Query().Get("node"))
	if n == nil {
		return
	}
	b, n, err := s.Files.Open(r.Context(), sh.OwnerID, n.ID, "")
	if err != nil {
		fail(w, err)
		return
	}
	if r.Header.Get("Range") == "" && r.Method == http.MethodGet && r.URL.Query().Get("inline") != "1" {
		s.Files.CountDownload(r.Context(), sh.ID)
		s.logAct(r, sh.OwnerID, "public_download", n, "по ссылке")
	}
	serveBlob(w, r, b, n, r.URL.Query().Get("inline") == "1")
}

func (s *Server) pubThumb(w http.ResponseWriter, r *http.Request) {
	sh := s.loadShare(w, r, true)
	if sh == nil {
		return
	}
	n := s.shareNode(w, r, sh, r.URL.Query().Get("node"))
	if n == nil {
		return
	}
	s.serveThumb(w, r, n)
}

func (s *Server) pubZip(w http.ResponseWriter, r *http.Request) {
	sh := s.loadShare(w, r, true)
	if sh == nil {
		return
	}
	n := s.shareNode(w, r, sh, r.URL.Query().Get("node"))
	if n == nil {
		return
	}
	s.Files.CountDownload(r.Context(), sh.ID)
	s.logAct(r, sh.OwnerID, "public_download", n, "ZIP по ссылке")
	s.writeZip(w, r, sh.OwnerID, n)
}

func (s *Server) pubUpload(w http.ResponseWriter, r *http.Request) {
	sh := s.loadShare(w, r, true)
	if sh == nil {
		return
	}
	if !sh.AllowUpload {
		writeErr(w, http.StatusForbidden, "загрузка по этой ссылке запрещена")
		return
	}
	dir := s.shareNode(w, r, sh, r.URL.Query().Get("dir"))
	if dir == nil {
		return
	}
	if !dir.IsDir {
		fail(w, files.ErrNotDir)
		return
	}
	// Гости не могут перезаписывать файлы владельца — всегда новое имя.
	q := r.URL.Query()
	q.Set("conflict", "rename")
	q.Del("path")
	r.URL.RawQuery = q.Encode()
	if n := s.doUpload(w, r, sh.OwnerID, &dir.ID); n != nil {
		s.logAct(r, sh.OwnerID, "public_upload", n, "гость по ссылке")
		s.Files.Notify(r.Context(), sh.OwnerID, "guest_upload", "Гость загрузил файл",
			"«"+n.Name+"» в папку «"+dir.Name+"» по публичной ссылке.", "#/files/"+dir.ID, "")
		s.Files.CheckQuotaWarning(r.Context(), sh.OwnerID)
	}
}
