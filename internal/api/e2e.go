package api

import (
	"encoding/json"
	"net/http"
	"strings"

	"github.com/andrk774/cloudbox/internal/files"
)

// ---------- сквозное шифрование: сервер хранит только обёрнутые ключи и шифротекст ----------

func (s *Server) e2eKeys(w http.ResponseWriter, r *http.Request) {
	k, err := s.Files.E2EKeys(r.Context(), userFrom(r).ID)
	if err != nil {
		fail(w, err)
		return
	}
	writeJSON(w, 200, map[string]any{"keys": k})
}

func (s *Server) e2ePutKeys(w http.ResponseWriter, r *http.Request) {
	var req struct {
		Data    json.RawMessage `json:"data"`
		Version int             `json:"version"` // 0 — создать; текущая версия — заменить (смена кодовой фразы)
	}
	if err := readJSON(r, &req); err != nil {
		writeErr(w, 400, err.Error())
		return
	}
	v, err := s.Files.PutE2EKeys(r.Context(), userFrom(r).ID, req.Data, req.Version)
	if err != nil {
		fail(w, err)
		return
	}
	writeJSON(w, 200, map[string]int{"version": v})
}

func (s *Server) e2eDeleteKeys(w http.ResponseWriter, r *http.Request) {
	if err := s.Files.DeleteE2EKeys(r.Context(), userFrom(r).ID); err != nil {
		fail(w, err)
		return
	}
	okJSON(w)
}

func (s *Server) e2eCreateFolder(w http.ResponseWriter, r *http.Request) {
	var req struct {
		Parent string `json:"parent"`
		Name   string `json:"name"`
	}
	if err := readJSON(r, &req); err != nil {
		writeErr(w, 400, err.Error())
		return
	}
	// E2E-папки — только в своём дереве: в чужую общую папку ключ владельца не подходит
	u := userFrom(r)
	perm, parent, ok := s.parentAcc(w, r, req.Parent)
	if !ok {
		return
	}
	if perm.Owner != u.ID {
		fail(w, files.ErrE2E)
		return
	}
	n, err := s.Files.CreateE2EFolder(r.Context(), u.ID, parent, strings.TrimSpace(req.Name))
	if err != nil {
		fail(w, err)
		return
	}
	s.logAct(r, u.ID, "mkdir", n, "зашифрованная папка")
	writeJSON(w, 201, n)
}

func (s *Server) e2eMeta(w http.ResponseWriter, r *http.Request) {
	data, v, err := s.Files.E2EMeta(r.Context(), userFrom(r).ID, r.PathValue("id"))
	if err != nil {
		fail(w, err)
		return
	}
	writeJSON(w, 200, map[string]any{"data": data, "version": v})
}

func (s *Server) e2ePutMeta(w http.ResponseWriter, r *http.Request) {
	r.Body = http.MaxBytesReader(w, r.Body, 9<<20)
	var req struct {
		Data    string `json:"data"`
		Version int    `json:"version"`
	}
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		writeErr(w, 400, "некорректный JSON")
		return
	}
	v, err := s.Files.PutE2EMeta(r.Context(), userFrom(r).ID, r.PathValue("id"), req.Data, req.Version)
	if err != nil {
		fail(w, err)
		return
	}
	writeJSON(w, 200, map[string]int{"version": v})
}
