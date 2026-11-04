// Package syncclient — движок двусторонней синхронизации локальной папки с CloudBox.
// Им пользуются консольный клиент (cmd/cloudbox-sync) и графический (cmd/cloudbox-desktop).
//
//	cloudbox-sync -server https://cloud.example.ru -user andrey -local ~/CloudBox -remote /Документы -watch 30s
//
// Пароль — пароль приложения из настроек CloudBox: флаг -password или переменная CLOUDBOX_PASSWORD.
//
// Правила:
//   - изменилось только с одной стороны — копируем на другую;
//   - изменилось с обеих — локальная копия переименовывается в «имя (конфликт …)», серверная скачивается,
//     конфликтная копия загружается следующим проходом — ничего не теряется;
//   - удалено с одной стороны и не менялось на другой — удаляем (на сервере — в корзину,
//     локально — в папку .cloudbox-trash внутри синхронизируемой папки).
package syncclient

import (
	"bytes"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"net/http"
	"net/url"
	"os"
	"path"
	"path/filepath"
	"sort"
	"strings"
	"time"
)

const (
	stateFile = ".cloudbox-sync.json"
	trashDir  = ".cloudbox-trash"
	tmpPrefix = ".cloudbox-tmp-"
	chunk     = 8 << 20
)

type entry struct {
	Dir   bool   `json:"dir,omitempty"`
	Size  int64  `json:"size,omitempty"`
	MTime int64  `json:"mtime,omitempty"` // локальное время изменения, нс
	SHA   string `json:"sha,omitempty"`
	Ver   string `json:"ver,omitempty"` // версия на сервере
}

type state struct {
	Server     string           `json:"server"`
	RemoteRoot string           `json:"remote_root"`
	Etag       string           `json:"etag"`
	Entries    map[string]entry `json:"entries"`
	// E2ERoots — зашифрованные (E2E) папки на сервере: их содержимое — шифротекст с случайными именами,
	// расшифровать его может только веб-интерфейс. Клиент синхронизации их не трогает ни с одной стороны.
	E2ERoots []string `json:"e2e_roots,omitempty"`
}

type remoteEntry struct {
	ID        string    `json:"id"`
	Path      string    `json:"path"`
	IsDir     bool      `json:"is_dir"`
	Size      int64     `json:"size"`
	SHA256    string    `json:"sha256"`
	VersionID string    `json:"version_id"`
	UpdatedAt time.Time `json:"updated_at"`
	E2E       bool      `json:"e2e"`
}

type client struct {
	base, user, pass string
	http             *http.Client
}

type syncer struct {
	c      *client
	local  string
	remote string
	st     state
	dry    bool
	stats  struct{ up, down, delL, delR, conflicts int }
	log    func(format string, a ...any)
}

// ExpandHome раскрывает ~ в начале пути.
func ExpandHome(p string) string {
	if strings.HasPrefix(p, "~") {
		if h, err := os.UserHomeDir(); err == nil {
			return filepath.Join(h, p[1:])
		}
	}
	return p
}

// ---------- HTTP ----------

type apiError struct {
	Status int
	Msg    string
}

func (e *apiError) Error() string { return fmt.Sprintf("HTTP %d: %s", e.Status, e.Msg) }

func (c *client) do(method, p string, body io.Reader, ctype string, out any) error {
	req, err := http.NewRequest(method, c.base+p, body)
	if err != nil {
		return err
	}
	req.SetBasicAuth(c.user, c.pass)
	if ctype != "" {
		req.Header.Set("Content-Type", ctype)
	}
	resp, err := c.http.Do(req)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	if resp.StatusCode >= 300 {
		var e struct {
			Error string `json:"error"`
		}
		b, _ := io.ReadAll(io.LimitReader(resp.Body, 4096))
		json.Unmarshal(b, &e)
		if e.Error == "" {
			e.Error = strings.TrimSpace(string(b))
		}
		return &apiError{resp.StatusCode, e.Error}
	}
	if out != nil {
		return json.NewDecoder(resp.Body).Decode(out)
	}
	io.Copy(io.Discard, resp.Body)
	return nil
}

func (c *client) json(method, p string, in, out any) error {
	var body io.Reader
	if in != nil {
		b, _ := json.Marshal(in)
		body = bytes.NewReader(b)
	}
	return c.do(method, p, body, "application/json", out)
}

type node struct {
	ID    string `json:"id"`
	Name  string `json:"name"`
	IsDir bool   `json:"is_dir"`
}

// resolveRemote находит (или создаёт) папку на сервере по пути.
func (c *client) resolveRemote(p string) (string, error) {
	cur := ""
	for _, part := range strings.Split(strings.Trim(path.Clean("/"+p), "/"), "/") {
		if part == "" {
			continue
		}
		var list struct {
			Items []node `json:"items"`
		}
		q := ""
		if cur != "" {
			q = "?parent=" + cur
		}
		if err := c.json("GET", "/api/files"+q, nil, &list); err != nil {
			return "", err
		}
		found := ""
		for _, n := range list.Items {
			if n.Name == part && n.IsDir {
				found = n.ID
			}
		}
		if found == "" {
			var n node
			if err := c.json("POST", "/api/files/folder", map[string]string{"parent": cur, "name": part}, &n); err != nil {
				return "", err
			}
			found = n.ID
		}
		cur = found
	}
	return cur, nil
}

// ---------- состояние ----------

func (s *syncer) statePath() string { return filepath.Join(s.local, stateFile) }

func (s *syncer) loadState() {
	b, err := os.ReadFile(s.statePath())
	if err == nil {
		json.Unmarshal(b, &s.st)
	}
	if s.st.Server != s.c.base+"|"+s.remote {
		// другая пара сервер/папка — начинаем заново (ничего не удаляем, только сравниваем)
		s.st = state{Server: s.c.base + "|" + s.remote}
	}
	if s.st.Entries == nil {
		s.st.Entries = map[string]entry{}
	}
}

func (s *syncer) saveState() error {
	if s.dry {
		return nil
	}
	b, _ := json.MarshalIndent(s.st, "", " ")
	tmp := s.statePath() + ".tmp"
	if err := os.WriteFile(tmp, b, 0o600); err != nil {
		return err
	}
	return os.Rename(tmp, s.statePath())
}

// ---------- сканирование ----------

type localEntry struct {
	Dir   bool
	Size  int64
	MTime int64
	SHA   string
}

func skipName(name string) bool {
	return name == stateFile || name == stateFile+".tmp" || name == trashDir || strings.HasPrefix(name, tmpPrefix) ||
		name == ".DS_Store" || name == "Thumbs.db" || name == "desktop.ini" || strings.HasPrefix(name, "~$")
}

func (s *syncer) scanLocal() (map[string]localEntry, error) {
	out := map[string]localEntry{}
	err := filepath.WalkDir(s.local, func(p string, d fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		if p == s.local {
			return nil
		}
		if skipName(d.Name()) {
			if d.IsDir() {
				return filepath.SkipDir
			}
			return nil
		}
		if d.Type()&fs.ModeSymlink != 0 {
			return nil
		}
		rel, _ := filepath.Rel(s.local, p)
		rel = filepath.ToSlash(rel)
		if d.IsDir() {
			out[rel] = localEntry{Dir: true}
			return nil
		}
		info, err := d.Info()
		if err != nil {
			return err
		}
		le := localEntry{Size: info.Size(), MTime: info.ModTime().UnixNano()}
		if prev, ok := s.st.Entries[rel]; ok && !prev.Dir && prev.Size == le.Size && prev.MTime == le.MTime && prev.SHA != "" {
			le.SHA = prev.SHA // не изменился — не пересчитываем хеш
		} else if le.SHA, err = hashFile(p); err != nil {
			return err
		}
		out[rel] = le
		return nil
	})
	return out, err
}

func hashFile(p string) (string, error) {
	f, err := os.Open(p)
	if err != nil {
		return "", err
	}
	defer f.Close()
	h := sha256.New()
	if _, err := io.Copy(h, f); err != nil {
		return "", err
	}
	return hex.EncodeToString(h.Sum(nil)), nil
}

func (s *syncer) scanRemote() (map[string]remoteEntry, string, bool, error) {
	var resp struct {
		Etag      string        `json:"etag"`
		Unchanged bool          `json:"unchanged"`
		Entries   []remoteEntry `json:"entries"`
	}
	q := "/api/sync/tree?etag=" + url.QueryEscape(s.st.Etag)
	if s.st.RemoteRoot != "" {
		q += "&root=" + s.st.RemoteRoot
	}
	if err := s.c.json("GET", q, nil, &resp); err != nil {
		return nil, "", false, err
	}
	out := map[string]remoteEntry{}
	e2e := map[string]bool{}
	for _, e := range resp.Entries {
		if e.E2E {
			e2e[e.Path] = true
			continue
		}
		out[e.Path] = e
	}
	if !resp.Unchanged {
		var roots []string
		for p := range e2e {
			if !e2e[path.Dir(p)] { // родитель — обычная папка: это корень E2E-папки
				roots = append(roots, p)
				if !slicesContains(s.st.E2ERoots, p) {
					s.log("пропускаю зашифрованную папку %s — её открывают в веб-интерфейсе CloudBox", p)
				}
			}
		}
		sort.Strings(roots)
		s.st.E2ERoots = roots
	}
	return out, resp.Etag, resp.Unchanged, nil
}

func slicesContains(list []string, v string) bool {
	for _, x := range list {
		if x == v {
			return true
		}
	}
	return false
}

// dropE2E убирает из локального дерева пути под зашифрованными папками: иначе одноимённая
// локальная папка выглядела бы новой и уехала бы на сервер (или наоборот считалась удалённой).
func (s *syncer) dropE2E(local map[string]localEntry) map[string]localEntry {
	if len(s.st.E2ERoots) == 0 {
		return local
	}
	for p := range local {
		for _, r := range s.st.E2ERoots {
			if p == r || strings.HasPrefix(p, r+"/") {
				delete(local, p)
				break
			}
		}
	}
	return local
}

// ---------- проход синхронизации ----------

func (s *syncer) run() error {
	s.stats = struct{ up, down, delL, delR, conflicts int }{}
	if s.st.RemoteRoot == "" && strings.Trim(s.remote, "/") != "" {
		id, err := s.c.resolveRemote(s.remote)
		if err != nil {
			return fmt.Errorf("папка на сервере: %w", err)
		}
		s.st.RemoteRoot = id
	}
	local, err := s.scanLocal()
	if err != nil {
		return err
	}
	remote, etag, unchanged, err := s.scanRemote()
	if err != nil {
		var ae *apiError
		if errors.As(err, &ae) && ae.Status == 404 && s.st.RemoteRoot != "" {
			s.st.RemoteRoot = "" // папку на сервере удалили — пересоздадим
			s.st.Entries = map[string]entry{}
			return s.run()
		}
		return err
	}
	local = s.dropE2E(local)
	if unchanged {
		// на сервере ничего не менялось — берём дерево из состояния
		remote = map[string]remoteEntry{}
		for p, e := range s.st.Entries {
			remote[p] = remoteEntry{Path: p, IsDir: e.Dir, Size: e.Size, SHA256: e.SHA, VersionID: e.Ver, ID: "?"}
		}
		if s.localUnchanged(local) {
			return nil
		}
		// нужны настоящие id — перечитаем дерево целиком
		s.st.Etag = ""
		if remote, etag, _, err = s.scanRemote(); err != nil {
			return err
		}
	}

	paths := map[string]bool{}
	for p := range local {
		paths[p] = true
	}
	for p := range remote {
		paths[p] = true
	}
	for p := range s.st.Entries {
		paths[p] = true
	}
	all := make([]string, 0, len(paths))
	for p := range paths {
		all = append(all, p)
	}
	sort.Strings(all) // родители раньше детей

	dirIDs := map[string]string{"": s.st.RemoteRoot}
	for p, e := range remote {
		if e.IsDir {
			dirIDs[p] = e.ID
		}
	}

	var deleteLocalDirs, deleteRemoteDirs []string
	for _, p := range all {
		l, lok := local[p]
		r, rok := remote[p]
		prev, sok := s.st.Entries[p]

		// --- папки
		if (lok && l.Dir) || (rok && r.IsDir) || (sok && prev.Dir) {
			switch {
			case lok && l.Dir && rok && r.IsDir:
				s.st.Entries[p] = entry{Dir: true}
			case lok && l.Dir && !rok:
				if sok && prev.Dir && s.subtreeUnchangedLocal(p, local) {
					deleteLocalDirs = append(deleteLocalDirs, p) // удалили на сервере
				} else if err := s.mkdirRemote(p, dirIDs); err != nil {
					return err
				}
			case rok && r.IsDir && !lok:
				if sok && prev.Dir && s.subtreeUnchangedRemote(p, remote) {
					deleteRemoteDirs = append(deleteRemoteDirs, p) // удалили локально
				} else {
					s.logf("mkdir local %s", p)
					if !s.dry {
						if err := os.MkdirAll(s.abs(p), 0o755); err != nil {
							return err
						}
					}
					s.st.Entries[p] = entry{Dir: true}
				}
			case !lok && !rok:
				delete(s.st.Entries, p)
			default:
				s.log("пропуск %s: с одной стороны файл, с другой папка", p)
			}
			continue
		}

		// --- файлы
		switch {
		case lok && rok:
			if l.SHA == r.SHA256 {
				s.st.Entries[p] = entry{Size: l.Size, MTime: l.MTime, SHA: l.SHA, Ver: r.VersionID}
				continue
			}
			lChanged := !sok || l.SHA != prev.SHA
			rChanged := !sok || r.VersionID != prev.Ver
			switch {
			case lChanged && !rChanged:
				if err := s.upload(p, dirIDs); err != nil {
					return err
				}
			case !lChanged && rChanged:
				if err := s.download(p, r); err != nil {
					return err
				}
			default:
				if err := s.conflict(p, r); err != nil {
					return err
				}
			}
		case lok && !rok:
			if sok && l.SHA == prev.SHA {
				if err := s.trashLocal(p); err != nil { // удалён на сервере
					return err
				}
				delete(s.st.Entries, p)
			} else if err := s.upload(p, dirIDs); err != nil {
				return err
			}
		case rok && !lok:
			if sok && r.VersionID == prev.Ver {
				if err := s.trashRemote(p, r.ID); err != nil { // удалён локально
					return err
				}
				delete(s.st.Entries, p)
			} else if err := s.download(p, r); err != nil {
				return err
			}
		default:
			delete(s.st.Entries, p)
		}
	}

	// папки удаляем в конце, начиная с самых глубоких
	sort.Sort(sort.Reverse(sort.StringSlice(deleteRemoteDirs)))
	for _, p := range deleteRemoteDirs {
		if parentIn(p, deleteRemoteDirs) {
			delete(s.st.Entries, p)
			continue
		}
		if err := s.trashRemote(p, remote[p].ID); err != nil {
			return err
		}
		s.forget(p)
	}
	sort.Sort(sort.Reverse(sort.StringSlice(deleteLocalDirs)))
	for _, p := range deleteLocalDirs {
		if parentIn(p, deleteLocalDirs) {
			continue
		}
		if err := s.trashLocal(p); err != nil {
			return err
		}
		s.forget(p)
	}

	// После наших изменений etag сервера поменялся — запомним свежий, чтобы следующий проход был дешёвым.
	if s.stats.up+s.stats.delR > 0 || etag == "" {
		var resp struct {
			Etag string `json:"etag"`
		}
		q := "/api/sync/tree?etag=-"
		if s.st.RemoteRoot != "" {
			q += "&root=" + s.st.RemoteRoot
		}
		if err := s.c.json("GET", q, nil, &resp); err == nil {
			etag = resp.Etag
		}
	}
	s.st.Etag = etag
	return s.saveState()
}

func parentIn(p string, list []string) bool {
	for _, q := range list {
		if q != p && strings.HasPrefix(p, q+"/") {
			return true
		}
	}
	return false
}

func (s *syncer) forget(p string) {
	for k := range s.st.Entries {
		if k == p || strings.HasPrefix(k, p+"/") {
			delete(s.st.Entries, k)
		}
	}
}

func (s *syncer) localUnchanged(local map[string]localEntry) bool {
	if len(local) != len(s.st.Entries) {
		return false
	}
	for p, l := range local {
		e, ok := s.st.Entries[p]
		if !ok || e.Dir != l.Dir || (!l.Dir && e.SHA != l.SHA) {
			return false
		}
	}
	return true
}

func (s *syncer) subtreeUnchangedLocal(dir string, local map[string]localEntry) bool {
	for p, l := range local {
		if strings.HasPrefix(p, dir+"/") {
			e, ok := s.st.Entries[p]
			if !ok || (!l.Dir && e.SHA != l.SHA) {
				return false
			}
		}
	}
	return true
}

func (s *syncer) subtreeUnchangedRemote(dir string, remote map[string]remoteEntry) bool {
	for p, r := range remote {
		if strings.HasPrefix(p, dir+"/") {
			e, ok := s.st.Entries[p]
			if !ok || (!r.IsDir && e.Ver != r.VersionID) {
				return false
			}
		}
	}
	return true
}

func (s *syncer) abs(rel string) string { return filepath.Join(s.local, filepath.FromSlash(rel)) }

func (s *syncer) logf(f string, a ...any) {
	if s.dry {
		f = "[dry-run] " + f
	}
	s.log(f, a...)
}

// ---------- действия ----------

func (s *syncer) mkdirRemote(p string, dirIDs map[string]string) error {
	if _, ok := dirIDs[p]; ok {
		return nil
	}
	parent, name := path.Split(p)
	parent = strings.TrimSuffix(parent, "/")
	if _, ok := dirIDs[parent]; !ok {
		if err := s.mkdirRemote(parent, dirIDs); err != nil {
			return err
		}
	}
	s.logf("mkdir remote %s", p)
	if s.dry {
		dirIDs[p] = "dry"
		return nil
	}
	var n node
	err := s.c.json("POST", "/api/files/folder", map[string]string{"parent": dirIDs[parent], "name": name}, &n)
	var ae *apiError
	if errors.As(err, &ae) && ae.Status == 409 {
		return nil // уже создана
	}
	if err != nil {
		return err
	}
	dirIDs[p] = n.ID
	s.st.Entries[p] = entry{Dir: true}
	return nil
}

func (s *syncer) upload(p string, dirIDs map[string]string) error {
	parentRel, name := path.Split(p)
	parentRel = strings.TrimSuffix(parentRel, "/")
	if err := s.mkdirRemote(parentRel, dirIDs); err != nil && parentRel != "" {
		return err
	}
	s.logf("↑ %s", p)
	s.stats.up++
	if s.dry {
		return nil
	}
	f, err := os.Open(s.abs(p))
	if err != nil {
		return err
	}
	defer f.Close()
	info, err := f.Stat()
	if err != nil {
		return err
	}
	sha, err := hashFile(s.abs(p))
	if err != nil {
		return err
	}
	parent := dirIDs[parentRel]
	var n struct {
		VersionID string `json:"version_id"`
	}
	if info.Size() <= 2*chunk {
		q := url.Values{"name": {name}}
		if parent != "" {
			q.Set("parent", parent)
		}
		err = s.c.do("POST", "/api/files/upload?"+q.Encode(), f, "application/octet-stream", &n)
	} else {
		err = s.chunkedUpload(f, info.Size(), parent, name, &n)
	}
	if err != nil {
		return fmt.Errorf("загрузка %s: %w", p, err)
	}
	s.st.Entries[p] = entry{Size: info.Size(), MTime: info.ModTime().UnixNano(), SHA: sha, Ver: n.VersionID}
	return nil
}

// chunkedUpload — загрузка частями с продолжением после обрыва.
func (s *syncer) chunkedUpload(f *os.File, size int64, parent, name string, out any) error {
	var up struct {
		ID       string `json:"id"`
		Received int64  `json:"received"`
	}
	if err := s.c.json("POST", "/api/uploads", map[string]any{"parent": parent, "name": name, "size": size}, &up); err != nil {
		return err
	}
	off := int64(0)
	retries := 0
	for off < size {
		n := min(int64(chunk), size-off)
		sec := io.NewSectionReader(f, off, n)
		var res struct {
			Received int64 `json:"received"`
		}
		err := s.c.do("PUT", fmt.Sprintf("/api/uploads/%s?offset=%d", up.ID, off), sec, "application/octet-stream", &res)
		if err != nil {
			retries++
			if retries > 10 {
				return err
			}
			time.Sleep(time.Duration(retries) * 2 * time.Second)
			var cur struct {
				Received int64 `json:"received"`
			}
			if e := s.c.json("GET", "/api/uploads/"+up.ID, nil, &cur); e == nil {
				off = cur.Received
			}
			continue
		}
		retries = 0
		off = res.Received
	}
	return s.c.json("POST", "/api/uploads/"+up.ID+"/complete", nil, out)
}

func (s *syncer) download(p string, r remoteEntry) error {
	s.logf("↓ %s", p)
	s.stats.down++
	if s.dry {
		return nil
	}
	dst := s.abs(p)
	if err := os.MkdirAll(filepath.Dir(dst), 0o755); err != nil {
		return err
	}
	req, _ := http.NewRequest("GET", s.c.base+"/api/files/"+r.ID+"/content", nil)
	req.SetBasicAuth(s.c.user, s.c.pass)
	resp, err := s.c.http.Do(req)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	if resp.StatusCode != 200 {
		return fmt.Errorf("скачивание %s: HTTP %d", p, resp.StatusCode)
	}
	tmp, err := os.CreateTemp(filepath.Dir(dst), tmpPrefix+"*")
	if err != nil {
		return err
	}
	h := sha256.New()
	_, err = io.Copy(io.MultiWriter(tmp, h), resp.Body)
	if cerr := tmp.Close(); err == nil {
		err = cerr
	}
	if err != nil {
		os.Remove(tmp.Name())
		return err
	}
	sha := hex.EncodeToString(h.Sum(nil))
	if r.SHA256 != "" && sha != r.SHA256 {
		os.Remove(tmp.Name())
		return fmt.Errorf("скачивание %s: контрольная сумма не совпала", p)
	}
	os.Chtimes(tmp.Name(), r.UpdatedAt, r.UpdatedAt)
	if err := os.Rename(tmp.Name(), dst); err != nil {
		os.Remove(tmp.Name())
		return err
	}
	info, err := os.Stat(dst)
	if err != nil {
		return err
	}
	s.st.Entries[p] = entry{Size: info.Size(), MTime: info.ModTime().UnixNano(), SHA: sha, Ver: r.VersionID}
	return nil
}

func (s *syncer) conflict(p string, r remoteEntry) error {
	host, _ := os.Hostname()
	ext := path.Ext(p)
	base := strings.TrimSuffix(p, ext)
	cp := fmt.Sprintf("%s (конфликт %s %s)%s", base, host, time.Now().Format("2006-01-02 1504"), ext)
	s.logf("конфликт %s → локальная копия сохранена как %s", p, path.Base(cp))
	s.stats.conflicts++
	if s.dry {
		return nil
	}
	if err := os.Rename(s.abs(p), s.abs(cp)); err != nil {
		return err
	}
	delete(s.st.Entries, p)
	return s.download(p, r) // конфликтная копия загрузится следующим проходом как новый файл
}

func (s *syncer) trashLocal(p string) error {
	s.logf("✕ local %s (в %s)", p, trashDir)
	s.stats.delL++
	if s.dry {
		return nil
	}
	dst := filepath.Join(s.local, trashDir, time.Now().Format("2006-01-02_150405"), filepath.FromSlash(p))
	if err := os.MkdirAll(filepath.Dir(dst), 0o755); err != nil {
		return err
	}
	return os.Rename(s.abs(p), dst)
}

func (s *syncer) trashRemote(p, id string) error {
	s.logf("✕ remote %s (в корзину)", p)
	s.stats.delR++
	if s.dry {
		return nil
	}
	err := s.c.json("DELETE", "/api/files/"+id, nil, nil)
	var ae *apiError
	if errors.As(err, &ae) && ae.Status == 404 {
		return nil
	}
	return err
}

// ---------- публичный API ----------

// Config — что и с чем синхронизировать.
type Config struct {
	Server   string // https://cloud.example.ru
	User     string
	Password string // пароль приложения из настроек CloudBox
	Local    string // локальная папка (~ раскрывается)
	Remote   string // папка на сервере, например /Документы
	DryRun   bool   // только показать, что будет сделано
}

// Stats — итог одного прохода.
type Stats struct {
	Up, Down, DelLocal, DelRemote, Conflicts int
}

// Changed — сделал ли проход хоть что-нибудь.
func (s Stats) Changed() bool { return s.Up+s.Down+s.DelLocal+s.DelRemote+s.Conflicts > 0 }

// Syncer — синхронизация одной пары папок. Состояние хранится в .cloudbox-sync.json внутри локальной папки.
type Syncer struct{ s *syncer }

// New готовит синхронизацию. logf получает строки журнала (nil — молча).
func New(cfg Config, logf func(format string, a ...any)) (*Syncer, error) {
	if cfg.Server == "" || cfg.User == "" || cfg.Password == "" || cfg.Local == "" {
		return nil, errors.New("укажите сервер, пользователя, пароль приложения и локальную папку")
	}
	if logf == nil {
		logf = func(string, ...any) {}
	}
	abs, err := filepath.Abs(ExpandHome(cfg.Local))
	if err != nil {
		return nil, err
	}
	if err := os.MkdirAll(abs, 0o755); err != nil {
		return nil, err
	}
	remote := cfg.Remote
	if remote == "" {
		remote = "/"
	}
	s := &syncer{
		c:     &client{base: strings.TrimRight(cfg.Server, "/"), user: cfg.User, pass: cfg.Password, http: &http.Client{Timeout: 0}},
		local: abs, remote: remote, dry: cfg.DryRun, log: logf,
	}
	s.loadState()
	return &Syncer{s: s}, nil
}

// Local — абсолютный путь синхронизируемой папки.
func (x *Syncer) Local() string { return x.s.local }

// Run — один проход синхронизации.
func (x *Syncer) Run() (Stats, error) {
	err := x.s.run()
	st := x.s.stats
	return Stats{Up: st.up, Down: st.down, DelLocal: st.delL, DelRemote: st.delR, Conflicts: st.conflicts}, err
}

// Check проверяет адрес сервера и пароль приложения (для мастера настройки).
func Check(server, user, password string) error {
	c := &client{base: strings.TrimRight(server, "/"), user: user, pass: password, http: &http.Client{Timeout: 15 * time.Second}}
	var me struct {
		User struct {
			Username string `json:"username"`
		} `json:"user"`
	}
	if err := c.json("GET", "/api/me", nil, &me); err != nil {
		var ae *apiError
		if errors.As(err, &ae) && ae.Status == 401 {
			return errors.New("неверное имя пользователя или пароль приложения")
		}
		return fmt.Errorf("сервер недоступен: %v", err)
	}
	return nil
}
