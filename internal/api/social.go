package api

import (
	"errors"
	"net/http"
	"strconv"
	"strings"
	"time"

	"github.com/andrk774/cloudbox/internal/files"
	"github.com/andrk774/cloudbox/internal/pim"
)

// ---------- избранное и теги ----------

func (s *Server) setFavorite(on bool) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		_, n, good := s.acc(w, r, r.PathValue("id"), false)
		if !good {
			return
		}
		if err := s.Files.SetFavorite(r.Context(), userFrom(r).ID, n.ID, on); err != nil {
			fail(w, err)
			return
		}
		okJSON(w)
	}
}

func (s *Server) listTags(w http.ResponseWriter, r *http.Request) {
	list, err := s.Files.ListTags(r.Context(), userFrom(r).ID)
	if err != nil {
		fail(w, err)
		return
	}
	writeJSON(w, 200, list)
}

func (s *Server) createTag(w http.ResponseWriter, r *http.Request) {
	var req struct {
		Name  string `json:"name"`
		Color string `json:"color"`
	}
	if err := readJSON(r, &req); err != nil {
		writeErr(w, 400, err.Error())
		return
	}
	t, err := s.Files.CreateTag(r.Context(), userFrom(r).ID, req.Name, req.Color)
	if err != nil {
		fail(w, err)
		return
	}
	writeJSON(w, 201, t)
}

func (s *Server) deleteTag(w http.ResponseWriter, r *http.Request) {
	id, _ := strconv.ParseInt(r.PathValue("id"), 10, 64)
	if err := s.Files.DeleteTag(r.Context(), userFrom(r).ID, id); err != nil {
		fail(w, err)
		return
	}
	okJSON(w)
}

func (s *Server) setNodeTags(w http.ResponseWriter, r *http.Request) {
	_, n, good := s.acc(w, r, r.PathValue("id"), false)
	if !good {
		return
	}
	var req struct {
		TagIDs []int64 `json:"tag_ids"`
	}
	if err := readJSON(r, &req); err != nil {
		writeErr(w, 400, err.Error())
		return
	}
	if err := s.Files.SetNodeTags(r.Context(), userFrom(r).ID, n.ID, req.TagIDs); err != nil {
		fail(w, err)
		return
	}
	okJSON(w)
}

func (s *Server) taggedFiles(w http.ResponseWriter, r *http.Request) {
	id, _ := strconv.ParseInt(r.PathValue("id"), 10, 64)
	items, err := s.Files.TaggedNodes(r.Context(), userFrom(r).ID, id)
	if err != nil {
		fail(w, err)
		return
	}
	items = s.accessible(r, items)
	s.decorate(r, items)
	writeJSON(w, 200, items)
}

// ---------- комментарии ----------

func (s *Server) listComments(w http.ResponseWriter, r *http.Request) {
	_, n, good := s.acc(w, r, r.PathValue("id"), false)
	if !good {
		return
	}
	list, err := s.Files.Comments(r.Context(), n.ID)
	if err != nil {
		fail(w, err)
		return
	}
	writeJSON(w, 200, list)
}

func (s *Server) addComment(w http.ResponseWriter, r *http.Request) {
	perm, n, good := s.acc(w, r, r.PathValue("id"), false)
	if !good {
		return
	}
	var req struct {
		Body string `json:"body"`
	}
	if err := readJSON(r, &req); err != nil {
		writeErr(w, 400, err.Error())
		return
	}
	u := userFrom(r)
	c, err := s.Files.AddComment(r.Context(), u.ID, n.ID, req.Body)
	if err != nil {
		fail(w, err)
		return
	}
	s.logAct(r, perm.Owner, "comment", n, "")
	// уведомляем владельца и других участников обсуждения
	notified := map[int64]bool{u.ID: true}
	targets := []int64{perm.Owner}
	if list, err := s.Files.Comments(r.Context(), n.ID); err == nil {
		for _, x := range list {
			targets = append(targets, x.UserID)
		}
	}
	preview := c.Body
	if len([]rune(preview)) > 120 {
		preview = string([]rune(preview)[:120]) + "…"
	}
	for _, t := range targets {
		if notified[t] {
			continue
		}
		notified[t] = true
		s.Files.Notify(r.Context(), t, "comment", u.Username+" прокомментировал «"+n.Name+"»", preview, "#/file/"+n.ID, "")
	}
	writeJSON(w, 201, c)
}

func (s *Server) deleteComment(w http.ResponseWriter, r *http.Request) {
	id, _ := strconv.ParseInt(r.PathValue("cid"), 10, 64)
	c, err := s.Files.GetComment(r.Context(), id)
	if err != nil {
		fail(w, err)
		return
	}
	u := userFrom(r)
	if c.UserID != u.ID {
		// владелец файла тоже может удалять комментарии
		perm, _, err := s.Files.Access(r.Context(), u.ID, c.NodeID)
		if err != nil || perm.Owner != u.ID {
			fail(w, files.ErrForbidden)
			return
		}
	}
	if err := s.Files.DeleteComment(r.Context(), id); err != nil {
		fail(w, err)
		return
	}
	okJSON(w)
}

// ---------- журнал и уведомления ----------

func (s *Server) activity(w http.ResponseWriter, r *http.Request) {
	node := r.URL.Query().Get("node")
	if node != "" {
		if _, _, good := s.acc(w, r, node, false); !good {
			return
		}
	}
	limit, _ := strconv.Atoi(r.URL.Query().Get("limit"))
	list, err := s.Files.ActivityFor(r.Context(), userFrom(r).ID, node, limit)
	if err != nil {
		fail(w, err)
		return
	}
	writeJSON(w, 200, list)
}

func (s *Server) listNotifications(w http.ResponseWriter, r *http.Request) {
	list, unread, err := s.Files.Notifications(r.Context(), userFrom(r).ID)
	if err != nil {
		fail(w, err)
		return
	}
	writeJSON(w, 200, map[string]any{"items": list, "unread": unread})
}

func (s *Server) readNotifications(w http.ResponseWriter, r *http.Request) {
	var req struct {
		IDs []int64 `json:"ids"`
	}
	readJSON(r, &req)
	if err := s.Files.MarkRead(r.Context(), userFrom(r).ID, req.IDs); err != nil {
		fail(w, err)
		return
	}
	okJSON(w)
}

func (s *Server) clearNotifications(w http.ResponseWriter, r *http.Request) {
	if err := s.Files.ClearNotifications(r.Context(), userFrom(r).ID); err != nil {
		fail(w, err)
		return
	}
	okJSON(w)
}

// ---------- докачка ----------

func (s *Server) createUpload(w http.ResponseWriter, r *http.Request) {
	var req struct {
		Parent   string `json:"parent"`
		Name     string `json:"name"`
		Path     string `json:"path"`
		Size     int64  `json:"size"`
		Conflict string `json:"conflict"`
	}
	if err := readJSON(r, &req); err != nil {
		writeErr(w, 400, err.Error())
		return
	}
	perm, parent, good := s.parentAcc(w, r, req.Parent)
	if !good {
		return
	}
	if req.Conflict != "rename" {
		req.Conflict = ""
	}
	u, err := s.Files.CreateUpload(r.Context(), perm.Owner, userFrom(r).ID, parent, req.Name, strings.Trim(req.Path, "/"), req.Conflict, req.Size)
	if err != nil {
		fail(w, err)
		return
	}
	writeJSON(w, 201, u)
}

func (s *Server) getUpload(w http.ResponseWriter, r *http.Request) {
	u, err := s.Files.GetUpload(r.Context(), userFrom(r).ID, r.PathValue("id"))
	if err != nil {
		fail(w, err)
		return
	}
	writeJSON(w, 200, u)
}

// appendUpload: PUT /api/uploads/{id}?offset=N — тело является следующей частью файла.
func (s *Server) appendUpload(w http.ResponseWriter, r *http.Request) {
	off, err := strconv.ParseInt(r.URL.Query().Get("offset"), 10, 64)
	if err != nil {
		writeErr(w, 400, "нужен параметр offset")
		return
	}
	r.Body = http.MaxBytesReader(w, r.Body, 64<<20)
	u, err := s.Files.AppendUpload(r.Context(), userFrom(r).ID, r.PathValue("id"), off, r.Body)
	if errors.Is(err, files.ErrOffset) {
		writeJSON(w, http.StatusConflict, map[string]any{"error": err.Error(), "received": u.Received})
		return
	}
	if err != nil {
		if u != nil {
			// часть данных могла записаться — сообщаем актуальное смещение
			writeJSON(w, http.StatusBadRequest, map[string]any{"error": "соединение прервалось", "received": u.Received})
			return
		}
		fail(w, err)
		return
	}
	writeJSON(w, 200, map[string]int64{"received": u.Received, "size": u.Size})
}

func (s *Server) completeUpload(w http.ResponseWriter, r *http.Request) {
	u := userFrom(r)
	up, err := s.Files.GetUpload(r.Context(), u.ID, r.PathValue("id"))
	if err != nil {
		fail(w, err)
		return
	}
	// права на папку могли отозвать, пока шла загрузка
	if up.ParentID != nil {
		if _, _, good := s.parentAcc(w, r, *up.ParentID); !good {
			return
		}
	} else if up.OwnerID != u.ID {
		fail(w, files.ErrForbidden)
		return
	}
	n, up, err := s.Files.CompleteUpload(r.Context(), u.ID, up.ID)
	if err != nil {
		fail(w, err)
		return
	}
	s.logAct(r, up.OwnerID, "upload", n, "")
	s.Files.CheckQuotaWarning(r.Context(), up.OwnerID)
	writeJSON(w, 201, n)
}

func (s *Server) deleteUpload(w http.ResponseWriter, r *http.Request) {
	if err := s.Files.DeleteUpload(r.Context(), userFrom(r).ID, r.PathValue("id")); err != nil {
		fail(w, err)
		return
	}
	okJSON(w)
}

// ---------- календари и контакты ----------

func (s *Server) pimInfo(w http.ResponseWriter, r *http.Request) {
	writeJSON(w, 200, pim.URLs(s.Cfg.PublicURL, userFrom(r).Username))
}

func (s *Server) listCalendars(w http.ResponseWriter, r *http.Request) {
	u := userFrom(r)
	cals, err := s.PIM.Collections(r.Context(), u.ID, "cal")
	if err != nil {
		fail(w, err)
		return
	}
	books, err := s.PIM.Collections(r.Context(), u.ID, "card")
	if err != nil {
		fail(w, err)
		return
	}
	for i := range cals {
		cals[i].URL = s.Cfg.PublicURL + pim.CalPrefix + "/" + u.Username + "/calendars/" + cals[i].Slug + "/"
	}
	for i := range books {
		books[i].URL = s.Cfg.PublicURL + pim.CardPrefix + "/" + u.Username + "/contacts/" + books[i].Slug + "/"
	}
	writeJSON(w, 200, map[string]any{"calendars": cals, "addressbooks": books})
}

func (s *Server) createCalendar(w http.ResponseWriter, r *http.Request) {
	var req struct {
		Kind  string `json:"kind"`
		Name  string `json:"name"`
		Color string `json:"color"`
	}
	if err := readJSON(r, &req); err != nil {
		writeErr(w, 400, err.Error())
		return
	}
	if req.Kind != "card" {
		req.Kind = "cal"
	}
	c, err := s.PIM.CreateCollection(r.Context(), userFrom(r).ID, req.Kind, "", req.Name, "", req.Color)
	if err != nil {
		fail(w, err)
		return
	}
	writeJSON(w, 201, c)
}

func (s *Server) deleteCollection(w http.ResponseWriter, r *http.Request) {
	id, _ := strconv.ParseInt(r.PathValue("id"), 10, 64)
	if err := s.PIM.DeleteCollection(r.Context(), userFrom(r).ID, id); err != nil {
		fail(w, err)
		return
	}
	okJSON(w)
}

func (s *Server) listEvents(w http.ResponseWriter, r *http.Request) {
	q := r.URL.Query()
	from, err1 := time.Parse(time.RFC3339, q.Get("from"))
	to, err2 := time.Parse(time.RFC3339, q.Get("to"))
	if err1 != nil || err2 != nil || !to.After(from) || to.Sub(from) > 400*24*time.Hour {
		writeErr(w, 400, "нужны from и to в формате RFC 3339 (интервал до 400 дней)")
		return
	}
	list, err := s.PIM.Events(r.Context(), userFrom(r).ID, from, to)
	if err != nil {
		fail(w, err)
		return
	}
	writeJSON(w, 200, list)
}

func (s *Server) createEvent(w http.ResponseWriter, r *http.Request) {
	var in pim.EventInput
	if err := readJSON(r, &in); err != nil {
		writeErr(w, 400, err.Error())
		return
	}
	id, err := s.PIM.CreateEvent(r.Context(), userFrom(r).ID, in)
	if err != nil {
		fail(w, err)
		return
	}
	writeJSON(w, 201, map[string]int64{"id": id})
}

func (s *Server) deleteObject(w http.ResponseWriter, r *http.Request) {
	id, _ := strconv.ParseInt(r.PathValue("id"), 10, 64)
	if err := s.PIM.DeleteObject(r.Context(), userFrom(r).ID, id); err != nil {
		fail(w, err)
		return
	}
	okJSON(w)
}

func (s *Server) listContacts(w http.ResponseWriter, r *http.Request) {
	list, err := s.PIM.Contacts(r.Context(), userFrom(r).ID)
	if err != nil {
		fail(w, err)
		return
	}
	writeJSON(w, 200, list)
}

func (s *Server) createContact(w http.ResponseWriter, r *http.Request) {
	var in pim.ContactInput
	if err := readJSON(r, &in); err != nil {
		writeErr(w, 400, err.Error())
		return
	}
	id, err := s.PIM.CreateContact(r.Context(), userFrom(r).ID, in)
	if err != nil {
		fail(w, err)
		return
	}
	writeJSON(w, 201, map[string]int64{"id": id})
}

// ---------- онлайн-редактор ----------

func (s *Server) officePage(w http.ResponseWriter, r *http.Request) {
	u := userFrom(r)
	if u == nil {
		http.Redirect(w, r, "/", http.StatusFound)
		return
	}
	if !s.WOPI.Enabled() {
		http.Error(w, "онлайн-редактор не настроен (CB_WOPI_URL)", http.StatusNotFound)
		return
	}
	perm, n, err := s.Files.Access(r.Context(), u.ID, r.PathValue("id"))
	if err != nil || n.IsDir || n.E2E { // онлайн-офис получил бы шифротекст: ключа у сервера нет
		http.NotFound(w, r)
		return
	}
	s.WOPI.OpenPage(w, r, u.ID, u.Username, perm, n)
}
