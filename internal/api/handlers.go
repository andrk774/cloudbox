package api

import (
	"archive/zip"
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

	"github.com/andrk774/cloudbox/internal/files"
	"github.com/andrk774/cloudbox/internal/metrics"
	"github.com/andrk774/cloudbox/internal/storage"
)

// ---------- просмотр ----------

func (s *Server) decorate(r *http.Request, items []files.Node) {
	s.Files.Decorate(r.Context(), userFrom(r).ID, items)
}

func (s *Server) listFiles(w http.ResponseWriter, r *http.Request) {
	u := userFrom(r)
	parent := r.URL.Query().Get("parent")
	if parent == "" || parent == "root" {
		items, err := s.Files.List(r.Context(), u.ID, nil)
		if err != nil {
			fail(w, err)
			return
		}
		s.decorate(r, items)
		writeJSON(w, 200, map[string]any{"items": items, "breadcrumbs": []files.Node{}, "can_write": true})
		return
	}
	perm, dir, ok := s.acc(w, r, parent, false)
	if !ok {
		return
	}
	if !dir.IsDir {
		fail(w, files.ErrNotDir)
		return
	}
	items, err := s.Files.List(r.Context(), perm.Owner, &dir.ID)
	if err != nil {
		fail(w, err)
		return
	}
	crumbs, err := s.Files.Ancestors(r.Context(), perm.Owner, dir.ID)
	if err != nil {
		fail(w, err)
		return
	}
	s.decorate(r, items)
	resp := map[string]any{"items": items, "can_write": perm.Write}
	if perm.Owner != u.ID {
		crumbs = files.TrimCrumbs(crumbs, perm.ShareRoot)
		owner, _ := s.Auth.GetUser(r.Context(), perm.Owner)
		if owner != nil {
			resp["shared_by"] = owner.Username
		}
	}
	resp["breadcrumbs"] = crumbs
	writeJSON(w, 200, resp)
}

func (s *Server) getFile(w http.ResponseWriter, r *http.Request) {
	perm, n, ok := s.acc(w, r, r.PathValue("id"), false)
	if !ok {
		return
	}
	list := []files.Node{*n}
	s.decorate(r, list)
	list[0].CanWrite = &perm.Write
	if perm.Owner == userFrom(r).ID {
		list[0].Path, _ = s.Files.PathOf(r.Context(), perm.Owner, n.ID)
	}
	writeJSON(w, 200, list[0])
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
	perm, parent, ok := s.parentAcc(w, r, req.Parent)
	if !ok {
		return
	}
	n, err := s.Files.Mkdir(r.Context(), perm.Owner, parent, strings.TrimSpace(req.Name))
	if err != nil {
		fail(w, err)
		return
	}
	s.logAct(r, perm.Owner, "mkdir", n, "")
	writeJSON(w, 201, n)
}

// upload: POST /api/files/upload?parent=ID&name=файл.txt[&path=папка/подпапка][&conflict=rename]
// Тело запроса — сырые байты файла. Для больших файлов — /api/uploads (докачка).
func (s *Server) upload(w http.ResponseWriter, r *http.Request) {
	perm, parent, ok := s.parentAcc(w, r, r.URL.Query().Get("parent"))
	if !ok {
		return
	}
	n := s.doUpload(w, r, perm.Owner, parent)
	if n != nil {
		s.logAct(r, perm.Owner, "upload", n, "")
		s.Files.CheckQuotaWarning(r.Context(), perm.Owner)
	}
}

func (s *Server) doUpload(w http.ResponseWriter, r *http.Request, owner int64, parent *string) *files.Node {
	q := r.URL.Query()
	name := q.Get("name")
	if err := files.ValidateName(name); err != nil {
		fail(w, err)
		return nil
	}
	if rel := strings.Trim(q.Get("path"), "/"); rel != "" {
		var err error
		if parent, err = s.Files.EnsureDirs(r.Context(), owner, parent, rel); err != nil {
			fail(w, err)
			return nil
		}
	}
	if q.Get("conflict") == "rename" {
		var err error
		if name, err = s.Files.FreeName(r.Context(), owner, parent, name); err != nil {
			fail(w, err)
			return nil
		}
	}
	r.Body = http.MaxBytesReader(w, r.Body, (s.Cfg.MaxUploadMB<<20)+1)
	n, err := s.Files.Write(r.Context(), owner, parent, name, r.Body)
	var mbe *http.MaxBytesError
	if errors.As(err, &mbe) {
		err = files.ErrTooLarge
	}
	if err != nil {
		fail(w, err)
		return nil
	}
	metrics.UploadBytes.Add(n.Size)
	writeJSON(w, 201, n)
	return n
}

func (s *Server) content(w http.ResponseWriter, r *http.Request) {
	perm, n, ok := s.acc(w, r, r.PathValue("id"), false)
	if !ok {
		return
	}
	b, n, err := s.Files.Open(r.Context(), perm.Owner, n.ID, r.URL.Query().Get("version"))
	if err != nil {
		fail(w, err)
		return
	}
	serveBlob(w, r, b, n, r.URL.Query().Get("inline") == "1")
}

// saveContent — сохранение из встроенного редактора (заметки, текстовые файлы).
// Заголовок If-Match: <version_id> защищает от перезаписи чужих правок.
func (s *Server) saveContent(w http.ResponseWriter, r *http.Request) {
	perm, n, ok := s.acc(w, r, r.PathValue("id"), true)
	if !ok {
		return
	}
	if n.IsDir {
		fail(w, files.ErrIsDir)
		return
	}
	ifv := strings.Trim(r.Header.Get("If-Match"), `"`)
	r.Body = http.MaxBytesReader(w, r.Body, 50<<20)
	nn, err := s.Files.WriteOpt(r.Context(), perm.Owner, n.ParentID, n.Name, r.Body, files.WriteOpts{IfVersion: ifv})
	if err != nil {
		fail(w, err)
		return
	}
	s.logAct(r, perm.Owner, "edit", nn, "")
	writeJSON(w, 200, nn)
}

func (s *Server) thumb(w http.ResponseWriter, r *http.Request) {
	_, n, ok := s.acc(w, r, r.PathValue("id"), false)
	if !ok {
		return
	}
	s.serveThumb(w, r, n)
}

func (s *Server) serveThumb(w http.ResponseWriter, r *http.Request, n *files.Node) {
	size, _ := strconv.Atoi(r.URL.Query().Get("s"))
	if size == 0 {
		size = 256
	}
	if n.VersionID == nil {
		fail(w, files.ErrNoThumb)
		return
	}
	etag := `"` + *n.VersionID + "-" + strconv.Itoa(size) + `"`
	if r.Header.Get("If-None-Match") == etag {
		w.WriteHeader(http.StatusNotModified)
		return
	}
	data, err := s.Files.Thumbnail(r.Context(), n, size)
	if err != nil {
		w.Header().Set("Cache-Control", "private, max-age=3600")
		fail(w, err)
		return
	}
	w.Header().Set("Content-Type", "image/jpeg")
	w.Header().Set("Cache-Control", "private, max-age=2592000")
	w.Header().Set("ETag", etag)
	w.Write(data)
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

type countingWriter struct {
	http.ResponseWriter
	n int64
}

func (c *countingWriter) Write(b []byte) (int, error) {
	n, err := c.ResponseWriter.Write(b)
	c.n += int64(n)
	return n, err
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
	cw := &countingWriter{ResponseWriter: w}
	http.ServeContent(cw, r, n.Name, n.UpdatedAt, b)
	metrics.DownloadBytes.Add(cw.n)
}

func (s *Server) zip(w http.ResponseWriter, r *http.Request) {
	perm, n, ok := s.acc(w, r, r.PathValue("id"), false)
	if !ok {
		return
	}
	s.writeZip(w, r, perm.Owner, n)
}

func (s *Server) writeZip(w http.ResponseWriter, r *http.Request, owner int64, root *files.Node) {
	w.Header().Set("Content-Type", "application/zip")
	w.Header().Set("Content-Disposition", contentDisposition("attachment", root.Name+".zip"))
	cw := &countingWriter{ResponseWriter: w}
	zw := zip.NewWriter(cw)
	defer func() { zw.Close(); metrics.DownloadBytes.Add(cw.n) }()
	var walk func(n *files.Node, prefix string) error
	walk = func(n *files.Node, prefix string) error {
		p := path.Join(prefix, n.Name)
		if !n.IsDir {
			b, _, err := s.Files.Open(r.Context(), owner, n.ID, "")
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
		children, err := s.Files.List(r.Context(), owner, &n.ID)
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
		zw.SetComment("ERROR: " + err.Error())
	}
}

// ---------- изменение ----------

// moveFile: PATCH {"name": "...", "parent": null | "id"} — переименование и/или перемещение.
func (s *Server) moveFile(w http.ResponseWriter, r *http.Request) {
	u := userFrom(r)
	perm, n, ok := s.acc(w, r, r.PathValue("id"), true)
	if !ok {
		return
	}
	if perm.Owner != u.ID && perm.ShareRoot == n.ID {
		writeErr(w, http.StatusForbidden, "расшаренную вам папку может переименовать или переместить только владелец")
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
		pid := ""
		if p != nil {
			pid = *p
		}
		dperm, dparent, ok := s.parentAcc(w, r, pid)
		if !ok {
			return
		}
		if dperm.Owner != perm.Owner {
			fail(w, files.ErrCrossOwner)
			return
		}
		parent = dparent
	}
	oldName := n.Name
	res, err := s.Files.Move(r.Context(), perm.Owner, n.ID, parent, name)
	if err != nil {
		fail(w, err)
		return
	}
	if oldName != res.Name {
		s.logAct(r, perm.Owner, "rename", res, "было «"+oldName+"»")
	} else {
		s.logAct(r, perm.Owner, "move", res, "")
	}
	writeJSON(w, 200, res)
}

func (s *Server) copyFile(w http.ResponseWriter, r *http.Request) {
	sperm, src, ok := s.acc(w, r, r.PathValue("id"), false)
	if !ok {
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
	pid := ""
	if req.Parent != nil {
		pid = *req.Parent
	}
	dperm, parent, ok := s.parentAcc(w, r, pid)
	if !ok {
		return
	}
	name := strings.TrimSpace(req.Name)
	var err error
	if name == "" {
		if name, err = s.Files.FreeName(r.Context(), dperm.Owner, parent, src.Name); err != nil {
			fail(w, err)
			return
		}
	}
	n, err := s.Files.CopyTo(r.Context(), sperm.Owner, src.ID, dperm.Owner, parent, name)
	if err != nil {
		fail(w, err)
		return
	}
	s.logAct(r, dperm.Owner, "copy", n, "из «"+src.Name+"»")
	writeJSON(w, 201, n)
}

func (s *Server) trashFile(w http.ResponseWriter, r *http.Request) {
	u := userFrom(r)
	perm, n, ok := s.acc(w, r, r.PathValue("id"), true)
	if !ok {
		return
	}
	if perm.Owner != u.ID && perm.ShareRoot == n.ID {
		writeErr(w, http.StatusForbidden, "расшаренную вам папку удалить нельзя — попросите владельца закрыть доступ")
		return
	}
	if err := s.Files.Trash(r.Context(), perm.Owner, n.ID); err != nil {
		fail(w, err)
		return
	}
	s.logAct(r, perm.Owner, "trash", n, "")
	okJSON(w)
}

func (s *Server) versions(w http.ResponseWriter, r *http.Request) {
	perm, n, ok := s.acc(w, r, r.PathValue("id"), false)
	if !ok {
		return
	}
	v, err := s.Files.Versions(r.Context(), perm.Owner, n.ID)
	if err != nil {
		fail(w, err)
		return
	}
	writeJSON(w, 200, v)
}

func (s *Server) restoreVersion(w http.ResponseWriter, r *http.Request) {
	perm, n, ok := s.acc(w, r, r.PathValue("id"), true)
	if !ok {
		return
	}
	nn, err := s.Files.RestoreVersion(r.Context(), perm.Owner, n.ID, r.PathValue("vid"))
	if err != nil {
		fail(w, err)
		return
	}
	s.logAct(r, perm.Owner, "restore_version", nn, "")
	writeJSON(w, 200, nn)
}

// ---------- списки ----------

func (s *Server) withPaths(r *http.Request, owner int64, items []files.Node) {
	for i := range items {
		items[i].Path = "/"
		if items[i].ParentID != nil {
			items[i].Path, _ = s.Files.PathOf(r.Context(), owner, *items[i].ParentID)
		}
	}
}

func (s *Server) search(w http.ResponseWriter, r *http.Request) {
	u := userFrom(r)
	items, err := s.Files.Search(r.Context(), u.ID, r.URL.Query().Get("q"))
	if err != nil {
		fail(w, err)
		return
	}
	s.withPaths(r, u.ID, items)
	s.decorate(r, items)
	writeJSON(w, 200, items)
}

func (s *Server) recent(w http.ResponseWriter, r *http.Request) {
	u := userFrom(r)
	items, err := s.Files.Recent(r.Context(), u.ID, 50)
	if err != nil {
		fail(w, err)
		return
	}
	s.withPaths(r, u.ID, items)
	s.decorate(r, items)
	writeJSON(w, 200, items)
}

// accessible оставляет узлы, к которым у пользователя всё ещё есть доступ (избранное, теги на чужих файлах).
func (s *Server) accessible(r *http.Request, items []files.Node) []files.Node {
	u := userFrom(r)
	out := items[:0]
	for _, n := range items {
		if n.OwnerID == u.ID {
			out = append(out, n)
			continue
		}
		if perm, _, err := s.Files.Access(r.Context(), u.ID, n.ID); err == nil {
			n.CanWrite = &perm.Write
			out = append(out, n)
		}
	}
	return out
}

func (s *Server) favorites(w http.ResponseWriter, r *http.Request) {
	items, err := s.Files.Favorites(r.Context(), userFrom(r).ID)
	if err != nil {
		fail(w, err)
		return
	}
	items = s.accessible(r, items)
	s.decorate(r, items)
	writeJSON(w, 200, items)
}

func (s *Server) gallery(w http.ResponseWriter, r *http.Request) {
	var before *time.Time
	if b := r.URL.Query().Get("before"); b != "" {
		if t, err := time.Parse(time.RFC3339Nano, b); err == nil {
			before = &t
		}
	}
	items, err := s.Files.Gallery(r.Context(), userFrom(r).ID, before, 300)
	if err != nil {
		fail(w, err)
		return
	}
	writeJSON(w, 200, items)
}

// notes — папка «Заметки» (создаётся при первом обращении) и её Markdown-файлы.
func (s *Server) notes(w http.ResponseWriter, r *http.Request) {
	u := userFrom(r)
	dir, err := s.Files.EnsureDirs(r.Context(), u.ID, nil, "Заметки")
	if err != nil {
		fail(w, err)
		return
	}
	items, err := s.Files.List(r.Context(), u.ID, dir)
	if err != nil {
		fail(w, err)
		return
	}
	notes := []files.Node{}
	for _, n := range items {
		ext := strings.ToLower(path.Ext(n.Name))
		if !n.IsDir && (ext == ".md" || ext == ".txt" || ext == ".markdown") {
			notes = append(notes, n)
		}
	}
	writeJSON(w, 200, map[string]any{"folder_id": *dir, "items": notes})
}

// syncTree — всё поддерево одним запросом (для клиента синхронизации).
func (s *Server) syncTree(w http.ResponseWriter, r *http.Request) {
	u := userFrom(r)
	owner := u.ID
	var root *string
	if id := r.URL.Query().Get("root"); id != "" && id != "root" {
		perm, n, ok := s.acc(w, r, id, false)
		if !ok {
			return
		}
		if !n.IsDir {
			fail(w, files.ErrNotDir)
			return
		}
		owner, root = perm.Owner, &n.ID
	}
	etag := s.Files.RootEtag(r.Context(), owner)
	if root != nil {
		if n, err := s.Files.Get(r.Context(), owner, *root); err == nil {
			etag = n.Etag
		}
	}
	if r.URL.Query().Get("etag") == etag {
		writeJSON(w, 200, map[string]any{"etag": etag, "unchanged": true})
		return
	}
	entries, err := s.Files.Tree(r.Context(), owner, root)
	if err != nil {
		fail(w, err)
		return
	}
	writeJSON(w, 200, map[string]any{"etag": etag, "entries": entries})
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
	u := userFrom(r)
	n, err := s.Files.Restore(r.Context(), u.ID, r.PathValue("id"))
	if err != nil {
		fail(w, err)
		return
	}
	s.logAct(r, u.ID, "restore", n, "из корзины")
	writeJSON(w, 200, n)
}

func (s *Server) purge(w http.ResponseWriter, r *http.Request) {
	u := userFrom(r)
	if err := s.Files.Purge(r.Context(), u.ID, r.PathValue("id")); err != nil {
		fail(w, err)
		return
	}
	s.logAct(r, u.ID, "purge", nil, "")
	okJSON(w)
}

func (s *Server) emptyTrash(w http.ResponseWriter, r *http.Request) {
	u := userFrom(r)
	if err := s.Files.EmptyTrash(r.Context(), u.ID); err != nil {
		fail(w, err)
		return
	}
	s.logAct(r, u.ID, "purge", nil, "очистка корзины")
	okJSON(w)
}

// ---------- публичные ссылки ----------

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
	u := userFrom(r)
	sh, err := s.Files.CreateShare(r.Context(), u.ID, req.NodeID, req.Password, req.ExpiresAt, req.AllowUpload)
	if err != nil {
		fail(w, err)
		return
	}
	n, _ := s.Files.Get(r.Context(), u.ID, sh.NodeID)
	s.logAct(r, u.ID, "share_link", n, "")
	writeJSON(w, 201, s.shareURL(*sh))
}

func (s *Server) deleteShare(w http.ResponseWriter, r *http.Request) {
	if err := s.Files.DeleteShare(r.Context(), userFrom(r).ID, r.PathValue("id")); err != nil {
		fail(w, err)
		return
	}
	okJSON(w)
}

// ---------- общий доступ пользователям и группам ----------

func (s *Server) fileAccess(w http.ResponseWriter, r *http.Request) {
	u := userFrom(r)
	perm, n, ok := s.acc(w, r, r.PathValue("id"), false)
	if !ok {
		return
	}
	resp := map[string]any{"owner": perm.Owner == u.ID, "can_write": perm.Write}
	if perm.Owner == u.ID {
		us, err := s.Files.ListUserShares(r.Context(), n.ID)
		if err != nil {
			fail(w, err)
			return
		}
		links, err := s.Files.ListShares(r.Context(), u.ID, n.ID)
		if err != nil {
			fail(w, err)
			return
		}
		out := make([]shareOut, 0, len(links))
		for _, sh := range links {
			out = append(out, s.shareURL(sh))
		}
		resp["user_shares"], resp["links"] = us, out
	} else if owner, err := s.Auth.GetUser(r.Context(), perm.Owner); err == nil {
		resp["owner_name"] = owner.Username
	}
	writeJSON(w, 200, resp)
}

func (s *Server) createUserShare(w http.ResponseWriter, r *http.Request) {
	u := userFrom(r)
	var req struct {
		NodeID   string `json:"node_id"`
		UserID   *int64 `json:"user_id"`
		GroupID  *int64 `json:"group_id"`
		CanWrite bool   `json:"can_write"`
	}
	if err := readJSON(r, &req); err != nil {
		writeErr(w, 400, err.Error())
		return
	}
	sh, err := s.Files.ShareWith(r.Context(), u.ID, req.NodeID, req.UserID, req.GroupID, req.CanWrite)
	if err != nil {
		fail(w, err)
		return
	}
	n, _ := s.Files.Get(r.Context(), u.ID, sh.NodeID)
	s.logAct(r, u.ID, "share_user", n, "для "+sh.TargetName)
	if n != nil {
		what := "файлом"
		if n.IsDir {
			what = "папкой"
		}
		rights := "только просмотр"
		if sh.CanWrite {
			rights = "можно изменять"
		}
		for _, t := range s.Files.ShareTargets(r.Context(), sh) {
			s.Files.Notify(r.Context(), t, "share", u.Username+" поделился с вами "+what,
				"«"+n.Name+"» — "+rights+".", "#/shared", "share-"+sh.ID+"-"+strconv.FormatInt(t, 10))
		}
	}
	writeJSON(w, 201, sh)
}

// shareOwnerOrTarget: владелец может всё, получатель — только отказаться (удалить свою шару).
func (s *Server) loadUserShare(w http.ResponseWriter, r *http.Request) (*files.UserShare, bool, bool) {
	sh, err := s.Files.GetUserShare(r.Context(), r.PathValue("id"))
	if err != nil {
		fail(w, err)
		return nil, false, false
	}
	u := userFrom(r)
	isOwner := sh.OwnerID == u.ID
	isTarget := sh.UserID != nil && *sh.UserID == u.ID
	if !isOwner && !isTarget {
		fail(w, files.ErrNotFound)
		return nil, false, false
	}
	return sh, isOwner, true
}

func (s *Server) updateUserShare(w http.ResponseWriter, r *http.Request) {
	sh, isOwner, ok := s.loadUserShare(w, r)
	if !ok {
		return
	}
	if !isOwner {
		fail(w, files.ErrForbidden)
		return
	}
	var req struct {
		CanWrite bool `json:"can_write"`
	}
	if err := readJSON(r, &req); err != nil {
		writeErr(w, 400, err.Error())
		return
	}
	if err := s.Files.UpdateUserShare(r.Context(), sh.ID, req.CanWrite); err != nil {
		fail(w, err)
		return
	}
	okJSON(w)
}

func (s *Server) deleteUserShare(w http.ResponseWriter, r *http.Request) {
	sh, _, ok := s.loadUserShare(w, r)
	if !ok {
		return
	}
	if err := s.Files.DeleteUserShare(r.Context(), sh.ID); err != nil {
		fail(w, err)
		return
	}
	okJSON(w)
}

func (s *Server) sharedWithMe(w http.ResponseWriter, r *http.Request) {
	items, err := s.Files.SharedWithMe(r.Context(), userFrom(r).ID)
	if err != nil {
		fail(w, err)
		return
	}
	s.decorate(r, items)
	writeJSON(w, 200, items)
}

func (s *Server) directory(w http.ResponseWriter, r *http.Request) {
	list, err := s.Files.Directory(r.Context(), userFrom(r).ID, r.URL.Query().Get("q"))
	if err != nil {
		fail(w, err)
		return
	}
	writeJSON(w, 200, list)
}
