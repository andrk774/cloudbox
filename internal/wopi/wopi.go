// Package wopi — онлайн-редактирование документов через Collabora Online или OnlyOffice
// по протоколу WOPI. CloudBox выступает WOPI-хостом: отдаёт редактору файл и принимает сохранения.
package wopi

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"encoding/xml"
	"errors"
	"fmt"
	"html/template"
	"io"
	"log"
	"net/http"
	"net/url"
	"path"
	"regexp"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/andrk774/cloudbox/internal/files"
	"github.com/andrk774/cloudbox/internal/secure"
)

type Config struct {
	EditorURL    string // как браузер видит редактор: https://office.example.ru
	DiscoveryURL string // откуда CloudBox берёт discovery (может быть внутренний адрес контейнера)
	HostURL      string // как редактор видит CloudBox (WOPISrc): обычно публичный адрес
	Secret       string
	TokenTTL     time.Duration
}

type Server struct {
	Cfg   Config
	Files *files.Service

	mu      sync.Mutex
	actions map[string]map[string]string // ext -> action(edit/view) -> urlsrc
	loaded  time.Time
}

func (s *Server) Enabled() bool { return s != nil && s.Cfg.EditorURL != "" }

// ---------- discovery ----------

type discovery struct {
	Zones []struct {
		Name string `xml:"name,attr"`
		Apps []struct {
			Name    string `xml:"name,attr"`
			Actions []struct {
				Name   string `xml:"name,attr"`
				Ext    string `xml:"ext,attr"`
				URLSrc string `xml:"urlsrc,attr"`
			} `xml:"action"`
		} `xml:"app"`
	} `xml:"net-zone"`
}

var placeholder = regexp.MustCompile(`<[^>]*>`)

func (s *Server) load(ctx context.Context) (map[string]map[string]string, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.actions != nil && time.Since(s.loaded) < time.Hour {
		return s.actions, nil
	}
	u := s.Cfg.DiscoveryURL
	if u == "" {
		u = strings.TrimRight(s.Cfg.EditorURL, "/") + "/hosting/discovery"
	}
	req, _ := http.NewRequestWithContext(ctx, http.MethodGet, u, nil)
	resp, err := (&http.Client{Timeout: 10 * time.Second}).Do(req)
	if err != nil {
		if s.actions != nil {
			return s.actions, nil
		}
		return nil, fmt.Errorf("редактор недоступен: %w", err)
	}
	defer resp.Body.Close()
	var d discovery
	if err := xml.NewDecoder(io.LimitReader(resp.Body, 4<<20)).Decode(&d); err != nil {
		return nil, fmt.Errorf("discovery: %w", err)
	}
	acts := map[string]map[string]string{}
	for _, z := range d.Zones {
		for _, a := range z.Apps {
			for _, ac := range a.Actions {
				if ac.Ext == "" || ac.URLSrc == "" {
					continue
				}
				if acts[ac.Ext] == nil {
					acts[ac.Ext] = map[string]string{}
				}
				src := placeholder.ReplaceAllString(ac.URLSrc, "")
				if _, ok := acts[ac.Ext][ac.Name]; !ok {
					acts[ac.Ext][ac.Name] = src
				}
			}
		}
	}
	s.actions, s.loaded = acts, time.Now()
	return acts, nil
}

// Extensions — что умеет открывать редактор (для веб-интерфейса).
func (s *Server) Extensions(ctx context.Context) []string {
	if !s.Enabled() {
		return nil
	}
	acts, err := s.load(ctx)
	if err != nil {
		log.Println("wopi:", err)
		return nil
	}
	var out []string
	for ext, a := range acts {
		if a["edit"] != "" || a["view"] != "" {
			out = append(out, ext)
		}
	}
	return out
}

// ---------- токены доступа ----------

type token struct {
	UID      int64  `json:"u"`
	Owner    int64  `json:"o"`
	Node     string `json:"n"`
	Write    bool   `json:"w"`
	Exp      int64  `json:"e"`
	Username string `json:"un"`
}

func (s *Server) sign(t token) string {
	b, _ := json.Marshal(t)
	p := base64.RawURLEncoding.EncodeToString(b)
	return p + "." + secure.HMAC(s.Cfg.Secret, "wopi|"+p)
}

func (s *Server) parse(raw string) (*token, error) {
	i := strings.LastIndex(raw, ".")
	if i < 0 || !secure.Equal(raw[i+1:], secure.HMAC(s.Cfg.Secret, "wopi|"+raw[:i])) {
		return nil, errors.New("bad token")
	}
	b, err := base64.RawURLEncoding.DecodeString(raw[:i])
	if err != nil {
		return nil, err
	}
	var t token
	if err := json.Unmarshal(b, &t); err != nil {
		return nil, err
	}
	if time.Now().Unix() > t.Exp {
		return nil, errors.New("token expired")
	}
	return &t, nil
}

// ---------- страница редактора ----------

var page = template.Must(template.New("office").Parse(`<!doctype html>
<html lang="ru"><head><meta charset="utf-8"><meta name="viewport" content="width=device-width, initial-scale=1">
<title>{{.Name}} — CloudBox</title>
<style>html,body{margin:0;height:100%;overflow:hidden;font-family:system-ui,sans-serif}
#bar{height:40px;display:flex;align-items:center;gap:12px;padding:0 14px;background:#1c2330;color:#fff;font-size:14px}
#bar a{color:#9fc0ff;text-decoration:none} iframe{border:0;width:100%;height:calc(100% - 40px)}</style></head>
<body><div id="bar"><a href="/#/files/{{.Parent}}">← К файлам</a><span>{{.Name}}</span></div>
<form id="f" action="{{.Action}}" method="post" target="ed">
<input type="hidden" name="access_token" value="{{.Token}}"><input type="hidden" name="access_token_ttl" value="{{.TTL}}"></form>
<iframe name="ed" allow="clipboard-read *; clipboard-write *; fullscreen"></iframe>
<script>document.getElementById('f').submit()</script></body></html>`))

// OpenPage отдаёт страницу с редактором. Права проверены вызывающим (perm).
func (s *Server) OpenPage(w http.ResponseWriter, r *http.Request, uid int64, username string, perm *files.Perm, n *files.Node) {
	acts, err := s.load(r.Context())
	if err != nil {
		http.Error(w, err.Error(), http.StatusBadGateway)
		return
	}
	ext := strings.TrimPrefix(strings.ToLower(path.Ext(n.Name)), ".")
	a := acts[ext]
	src := a["edit"]
	write := perm.Write
	if src == "" || !write {
		if a["view"] != "" {
			src, write = a["view"], false
		}
	}
	if src == "" {
		http.Error(w, "редактор не поддерживает этот формат", http.StatusBadRequest)
		return
	}
	ttl := s.Cfg.TokenTTL
	if ttl == 0 {
		ttl = 10 * time.Hour
	}
	exp := time.Now().Add(ttl)
	tok := s.sign(token{UID: uid, Owner: perm.Owner, Node: n.ID, Write: write, Exp: exp.Unix(), Username: username})
	host := strings.TrimRight(s.Cfg.HostURL, "/")
	wopiSrc := host + "/wopi/files/" + n.ID
	sep := "?"
	if strings.Contains(src, "?") {
		sep = "&"
		if strings.HasSuffix(src, "?") || strings.HasSuffix(src, "&") {
			sep = ""
		}
	}
	action := src + sep + "WOPISrc=" + url.QueryEscape(wopiSrc) + "&lang=ru"
	parent := ""
	if n.ParentID != nil && perm.Owner == uid {
		parent = *n.ParentID
	}
	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	w.Header().Set("Cache-Control", "no-store")
	w.Header().Del("X-Frame-Options")
	page.Execute(w, map[string]any{
		"Name": n.Name, "Parent": parent, "Action": template.URL(action), "Token": tok, "TTL": exp.UnixMilli(),
	})
}

// ---------- WOPI-эндпоинты (их вызывает сервер редактора) ----------

func (s *Server) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	rest := strings.TrimPrefix(r.URL.Path, "/wopi/files/")
	id, sub, _ := strings.Cut(rest, "/")
	t, err := s.parse(r.URL.Query().Get("access_token"))
	if err != nil || t.Node != id {
		http.Error(w, "unauthorized", http.StatusUnauthorized)
		return
	}
	ctx := r.Context()
	n, err := s.Files.Get(ctx, t.Owner, id)
	if err != nil {
		http.Error(w, "not found", http.StatusNotFound)
		return
	}
	override := r.Header.Get("X-WOPI-Override")
	switch {
	case sub == "" && r.Method == http.MethodGet:
		s.checkFileInfo(w, t, n)
	case sub == "contents" && r.Method == http.MethodGet:
		b, n, err := s.Files.Open(ctx, t.Owner, id, "")
		if err != nil {
			http.Error(w, err.Error(), http.StatusNotFound)
			return
		}
		defer b.Close()
		w.Header().Set("Content-Type", "application/octet-stream")
		w.Header().Set("Content-Length", strconv.FormatInt(b.Size(), 10))
		if n.VersionID != nil {
			w.Header().Set("X-WOPI-ItemVersion", *n.VersionID)
		}
		io.Copy(w, b)
	case sub == "contents" && r.Method == http.MethodPost && (override == "PUT" || override == ""):
		s.putFile(w, r, t, n)
	case sub == "" && r.Method == http.MethodPost:
		s.lockOps(w, r, t, n, override)
	default:
		http.Error(w, "not implemented", http.StatusNotImplemented)
	}
}

func (s *Server) checkFileInfo(w http.ResponseWriter, t *token, n *files.Node) {
	ver := ""
	if n.VersionID != nil {
		ver = *n.VersionID
	}
	info := map[string]any{
		"BaseFileName":            n.Name,
		"OwnerId":                 strconv.FormatInt(t.Owner, 10),
		"Size":                    n.Size,
		"UserId":                  strconv.FormatInt(t.UID, 10),
		"UserFriendlyName":        t.Username,
		"Version":                 ver,
		"LastModifiedTime":        n.UpdatedAt.UTC().Format(time.RFC3339),
		"UserCanWrite":            t.Write,
		"ReadOnly":                !t.Write,
		"SupportsUpdate":          true,
		"SupportsLocks":           true,
		"SupportsGetLock":         true,
		"UserCanNotWriteRelative": true,
		"DisablePrint":            false,
		"HidePrintOption":         false,
	}
	w.Header().Set("Content-Type", "application/json")
	json.NewEncoder(w).Encode(info)
}

func (s *Server) putFile(w http.ResponseWriter, r *http.Request, t *token, n *files.Node) {
	if !t.Write {
		http.Error(w, "read only", http.StatusUnauthorized)
		return
	}
	lock := r.Header.Get("X-WOPI-Lock")
	cur := s.Files.GetLock(r.Context(), n.ID)
	if cur != "" && cur != lock {
		w.Header().Set("X-WOPI-Lock", cur)
		http.Error(w, "locked", http.StatusConflict)
		return
	}
	// По спецификации запись без блокировки допустима только для пустого файла, но Collabora
	// иногда сохраняет без LOCK — разрешаем, как делает Nextcloud richdocuments.
	nn, err := s.Files.WriteOpt(r.Context(), t.Owner, n.ParentID, n.Name, r.Body, files.WriteOpts{LockID: cur})
	if errors.Is(err, files.ErrLocked) {
		w.Header().Set("X-WOPI-Lock", s.Files.GetLock(r.Context(), n.ID))
		http.Error(w, "locked", http.StatusConflict)
		return
	}
	if err != nil {
		log.Println("wopi put:", err)
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}
	uid := t.UID
	s.Files.Log(r.Context(), t.Owner, &uid, t.Username, "edit", nn, "онлайн-редактор", "")
	if nn.VersionID != nil {
		w.Header().Set("X-WOPI-ItemVersion", *nn.VersionID)
	}
	w.Header().Set("Content-Type", "application/json")
	json.NewEncoder(w).Encode(map[string]string{"LastModifiedTime": nn.UpdatedAt.UTC().Format(time.RFC3339)})
}

func (s *Server) lockOps(w http.ResponseWriter, r *http.Request, t *token, n *files.Node, op string) {
	ctx := r.Context()
	lock := r.Header.Get("X-WOPI-Lock")
	var cur string
	var err error
	switch op {
	case "LOCK":
		if old := r.Header.Get("X-WOPI-OldLock"); old != "" {
			cur, err = s.Files.Lock(ctx, n.ID, lock, old) // UNLOCK_AND_RELOCK
		} else {
			cur, err = s.Files.Lock(ctx, n.ID, lock, "")
		}
	case "REFRESH_LOCK":
		cur, err = s.Files.Lock(ctx, n.ID, lock, lock)
	case "UNLOCK":
		cur, err = s.Files.Unlock(ctx, n.ID, lock)
	case "GET_LOCK":
		w.Header().Set("X-WOPI-Lock", s.Files.GetLock(ctx, n.ID))
		return
	case "PUT_RELATIVE", "RENAME_FILE":
		http.Error(w, "not supported", http.StatusNotImplemented)
		return
	default:
		http.Error(w, "unknown override", http.StatusBadRequest)
		return
	}
	if errors.Is(err, files.ErrLocked) {
		w.Header().Set("X-WOPI-Lock", cur)
		http.Error(w, "lock mismatch", http.StatusConflict)
		return
	}
	if err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}
	if n.VersionID != nil {
		w.Header().Set("X-WOPI-ItemVersion", *n.VersionID)
	}
	w.WriteHeader(http.StatusOK)
}
