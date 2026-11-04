package api

import (
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"strconv"
	"strings"
	"time"

	"github.com/andrk774/cloudbox/internal/files"
	"github.com/andrk774/cloudbox/internal/mail"
)

// ---------- почта: веб-клиент для внешних IMAP/SMTP-ящиков ----------

func (s *Server) mailInfo(w http.ResponseWriter, r *http.Request) {
	info := map[string]any{"enabled": s.Mail.Enabled()}
	if !s.Mail.Enabled() {
		info["reason"] = mail.ErrDisabled.Error()
	}
	writeJSON(w, 200, info)
}

func (s *Server) mailPreset(w http.ResponseWriter, r *http.Request) {
	p, ok := mail.PresetFor(r.URL.Query().Get("email"))
	if !ok {
		writeJSON(w, 200, map[string]any{"known": false})
		return
	}
	writeJSON(w, 200, map[string]any{"known": true, "preset": p})
}

func (s *Server) mailAccounts(w http.ResponseWriter, r *http.Request) {
	list, err := s.Mail.Accounts(r.Context(), userFrom(r).ID)
	if err != nil {
		fail(w, err)
		return
	}
	writeJSON(w, 200, list)
}

func (s *Server) mailAddAccount(w http.ResponseWriter, r *http.Request) {
	// проверка ящика подключается к чужому серверу: без лимита это перебор паролей чужими руками
	if !s.allow(r, "mail-add", 10, 10*time.Minute) {
		writeErr(w, http.StatusTooManyRequests, "слишком много попыток подключить ящик, подождите 10 минут")
		return
	}
	var in mail.AccountInput
	if err := readJSON(r, &in); err != nil {
		writeErr(w, 400, err.Error())
		return
	}
	a, err := s.Mail.AddAccount(r.Context(), userFrom(r).ID, in)
	if err != nil {
		fail(w, err)
		return
	}
	writeJSON(w, 201, a)
}

func (s *Server) mailDeleteAccount(w http.ResponseWriter, r *http.Request) {
	if err := s.Mail.DeleteAccount(r.Context(), userFrom(r).ID, r.PathValue("id")); err != nil {
		fail(w, err)
		return
	}
	okJSON(w)
}

// mailAcc — ящик текущего пользователя (чужой неотличим от несуществующего).
func (s *Server) mailAcc(w http.ResponseWriter, r *http.Request) (*mail.Account, bool) {
	a, err := s.Mail.Account(r.Context(), userFrom(r).ID, r.PathValue("id"))
	if err != nil {
		fail(w, err)
		return nil, false
	}
	return a, true
}

func (s *Server) mailFolders(w http.ResponseWriter, r *http.Request) {
	a, ok := s.mailAcc(w, r)
	if !ok {
		return
	}
	list, err := s.Mail.Folders(r.Context(), a)
	if err != nil {
		fail(w, err)
		return
	}
	writeJSON(w, 200, list)
}

func (s *Server) mailMessages(w http.ResponseWriter, r *http.Request) {
	a, ok := s.mailAcc(w, r)
	if !ok {
		return
	}
	q := r.URL.Query()
	offset, _ := strconv.Atoi(q.Get("offset"))
	page, err := s.Mail.Messages(r.Context(), a, q.Get("folder"), offset, q.Get("q"))
	if err != nil {
		fail(w, err)
		return
	}
	writeJSON(w, 200, page)
}

func uidParam(r *http.Request) (uint32, bool) {
	n, err := strconv.ParseUint(r.URL.Query().Get("uid"), 10, 32)
	return uint32(n), err == nil && n > 0
}

func (s *Server) mailMessage(w http.ResponseWriter, r *http.Request) {
	a, ok := s.mailAcc(w, r)
	if !ok {
		return
	}
	uid, ok := uidParam(r)
	if !ok {
		writeErr(w, 400, "неверный номер письма")
		return
	}
	m, err := s.Mail.Message(r.Context(), a, r.URL.Query().Get("folder"), uid)
	if err != nil {
		fail(w, err)
		return
	}
	writeJSON(w, 200, m)
}

func (s *Server) mailAttachmentData(w http.ResponseWriter, r *http.Request, folder string, uid uint32, n int) (*mail.Account, *mail.Attachment, []byte, bool) {
	a, ok := s.mailAcc(w, r)
	if !ok {
		return nil, nil, nil, false
	}
	raw, err := s.Mail.Raw(r.Context(), a, folder, uid)
	if err != nil {
		fail(w, err)
		return nil, nil, nil, false
	}
	att, data, err := mail.AttachmentData(raw, n)
	if err != nil {
		fail(w, err)
		return nil, nil, nil, false
	}
	return a, att, data, true
}

// mailAttachment отдаёт вложение на скачивание. Содержимое пришло из чужого письма, поэтому
// всегда attachment + sandbox: даже HTML-вложение не выполнится в контексте CloudBox.
func (s *Server) mailAttachment(w http.ResponseWriter, r *http.Request) {
	q := r.URL.Query()
	uid, ok := uidParam(r)
	n, err := strconv.Atoi(q.Get("n"))
	if !ok || err != nil || n < 0 {
		writeErr(w, 400, "неверные параметры вложения")
		return
	}
	_, att, data, ok := s.mailAttachmentData(w, r, q.Get("folder"), uid, n)
	if !ok {
		return
	}
	ctype := att.Mime
	if !inlineSafe(ctype) {
		ctype = "application/octet-stream"
	}
	h := w.Header()
	h.Set("Content-Type", ctype)
	h.Set("Content-Disposition", contentDisposition("attachment", att.Filename))
	h.Set("Content-Security-Policy", "sandbox; default-src 'none'")
	h.Set("Cache-Control", "private, no-store")
	h.Set("Content-Length", strconv.Itoa(len(data)))
	w.Write(data)
}

// mailSaveAttachment сохраняет вложение в файлы CloudBox.
func (s *Server) mailSaveAttachment(w http.ResponseWriter, r *http.Request) {
	var req struct {
		Folder string `json:"folder"`
		UID    uint32 `json:"uid"`
		N      int    `json:"n"`
		Parent string `json:"parent"`
	}
	if err := readJSON(r, &req); err != nil {
		writeErr(w, 400, err.Error())
		return
	}
	perm, parent, ok := s.parentAcc(w, r, req.Parent)
	if !ok {
		return
	}
	_, att, data, ok := s.mailAttachmentData(w, r, req.Folder, req.UID, req.N)
	if !ok {
		return
	}
	name := strings.TrimSpace(strings.NewReplacer("/", "_", "\\", "_").Replace(att.Filename))
	if files.ValidateName(name) != nil {
		name = "вложение"
	}
	name, err := s.Files.FreeName(r.Context(), perm.Owner, parent, name)
	if err != nil {
		fail(w, err)
		return
	}
	n, err := s.Files.Write(r.Context(), perm.Owner, parent, name, strings.NewReader(string(data)))
	if err != nil {
		fail(w, err)
		return
	}
	s.logAct(r, perm.Owner, "upload", n, "вложение из почты")
	writeJSON(w, 201, n)
}

type mailUIDs struct {
	Folder string   `json:"folder"`
	UIDs   []uint32 `json:"uids"`
	Flag   string   `json:"flag"`
	On     bool     `json:"on"`
	Dest   string   `json:"dest"`
}

func (s *Server) mailBatch(w http.ResponseWriter, r *http.Request, fn func(a *mail.Account, req mailUIDs) (any, error)) {
	a, ok := s.mailAcc(w, r)
	if !ok {
		return
	}
	var req mailUIDs
	if err := readJSON(r, &req); err != nil {
		writeErr(w, 400, err.Error())
		return
	}
	res, err := fn(a, req)
	if err != nil {
		fail(w, err)
		return
	}
	writeJSON(w, 200, res)
}

func (s *Server) mailFlag(w http.ResponseWriter, r *http.Request) {
	s.mailBatch(w, r, func(a *mail.Account, q mailUIDs) (any, error) {
		return map[string]bool{"ok": true}, s.Mail.SetFlag(r.Context(), a, q.Folder, q.UIDs, q.Flag, q.On)
	})
}

func (s *Server) mailMove(w http.ResponseWriter, r *http.Request) {
	s.mailBatch(w, r, func(a *mail.Account, q mailUIDs) (any, error) {
		return map[string]bool{"ok": true}, s.Mail.Move(r.Context(), a, q.Folder, q.UIDs, q.Dest)
	})
}

func (s *Server) mailDelete(w http.ResponseWriter, r *http.Request) {
	s.mailBatch(w, r, func(a *mail.Account, q mailUIDs) (any, error) {
		perm, err := s.Mail.Delete(r.Context(), a, q.Folder, q.UIDs)
		return map[string]bool{"ok": err == nil, "permanent": perm}, err
	})
}

// mailSend: multipart/form-data — поле message (JSON письма с cloud_files — id файлов CloudBox)
// и поля file — вложения с компьютера.
func (s *Server) mailSend(w http.ResponseWriter, r *http.Request) {
	u := userFrom(r)
	// защита от рассылки спама через сервер: не больше 100 писем в час на пользователя
	if !s.lim.Allow("mail-send|"+strconv.FormatInt(u.ID, 10), 100, time.Hour) {
		writeErr(w, http.StatusTooManyRequests, "слишком много писем за час")
		return
	}
	a, ok := s.mailAcc(w, r)
	if !ok {
		return
	}
	r.Body = http.MaxBytesReader(w, r.Body, mail.MaxSendBytes+(1<<20))
	if err := r.ParseMultipartForm(8 << 20); err != nil {
		var mbe *http.MaxBytesError
		if errors.As(err, &mbe) {
			fail(w, mail.ErrSendTooBig)
			return
		}
		writeErr(w, 400, "ожидается multipart/form-data с полем message")
		return
	}
	defer r.MultipartForm.RemoveAll()
	var req struct {
		mail.Outgoing
		CloudFiles []string `json:"cloud_files"`
	}
	if err := json.Unmarshal([]byte(r.FormValue("message")), &req); err != nil {
		writeErr(w, 400, "некорректное поле message")
		return
	}
	out := req.Outgoing
	var total int64
	addFile := func(name, mt string, rd io.Reader) error {
		b, err := io.ReadAll(io.LimitReader(rd, mail.MaxSendBytes-total+1))
		if err != nil {
			return err
		}
		total += int64(len(b))
		if total > mail.MaxSendBytes {
			return mail.ErrSendTooBig
		}
		out.Files = append(out.Files, mail.OutFile{Name: name, Mime: mt, Data: b})
		return nil
	}
	for _, fh := range r.MultipartForm.File["file"] {
		f, err := fh.Open()
		if err != nil {
			writeErr(w, 400, "не удалось прочитать вложение")
			return
		}
		err = addFile(fh.Filename, fh.Header.Get("Content-Type"), f)
		f.Close()
		if err != nil {
			fail(w, err)
			return
		}
	}
	if len(req.CloudFiles) > 20 {
		writeErr(w, 400, "не больше 20 файлов из CloudBox")
		return
	}
	for _, id := range req.CloudFiles {
		perm, n, ok := s.acc(w, r, id, false) // только файлы, к которым у отправителя есть доступ
		if !ok {
			return
		}
		b, nn, err := s.Files.Open(r.Context(), perm.Owner, n.ID, "")
		if err != nil {
			fail(w, err)
			return
		}
		err = addFile(nn.Name, nn.Mime, b)
		b.Close()
		if err != nil {
			fail(w, err)
			return
		}
	}
	if err := s.Mail.Send(r.Context(), a, &out); err != nil {
		fail(w, err)
		return
	}
	okJSON(w)
}
