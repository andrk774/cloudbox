// Package dav — WebDAV-доступ к файлам (подключение как сетевой диск в Windows, Linux, macOS,
// Android через Solid Explorer / FolderSync / Total Commander, rclone, Joplin и т.д.).
//
// Чужие папки, к которым у пользователя есть доступ, видны в виртуальной папке «Доступно мне».
package dav

import (
	"context"
	"encoding/xml"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"log"
	"net/http"
	"os"
	"path"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/andrk774/cloudbox/internal/auth"
	"github.com/andrk774/cloudbox/internal/files"
	"github.com/andrk774/cloudbox/internal/netx"
	"github.com/andrk774/cloudbox/internal/storage"
	"golang.org/x/net/webdav"
)

const (
	Prefix    = "/dav"
	SharedDir = "Доступно мне"
)

type Handler struct {
	Auth   *auth.Service
	Files  *files.Service
	TmpDir string

	mu    sync.Mutex
	locks map[int64]webdav.LockSystem
	fails netx.Limiter
}

const (
	maxFails   = 20
	failWindow = 10 * time.Minute
)

func (h *Handler) lockSystem(uid int64) webdav.LockSystem {
	h.mu.Lock()
	defer h.mu.Unlock()
	if h.locks == nil {
		h.locks = map[int64]webdav.LockSystem{}
	}
	ls, ok := h.locks[uid]
	if !ok {
		ls = webdav.NewMemLS()
		h.locks[uid] = ls
	}
	return ls
}

func (h *Handler) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	// Защита от перебора паролей: 20 неудач за 10 минут с одного адреса.
	ip := netx.ClientIP(r)
	if h.fails.Blocked(ip, maxFails, failWindow) {
		http.Error(w, "too many failed logins", http.StatusTooManyRequests)
		return
	}
	user, pass, ok := r.BasicAuth()
	var u *auth.User
	var err error
	if ok {
		u, err = h.Auth.CheckBasic(r.Context(), user, pass)
	}
	if !ok || err != nil {
		if ok {
			h.fails.Fail(ip, failWindow)
			h.Auth.LogLogin(r.Context(), 0, user, ip, r.UserAgent(), false, "webdav")
		}
		w.Header().Set("WWW-Authenticate", `Basic realm="CloudBox", charset="UTF-8"`)
		http.Error(w, "unauthorized", http.StatusUnauthorized)
		return
	}
	// Заявленный размер PUT больше лимита или свободной квоты — отказываем сразу, не принимая тело.
	if r.Method == http.MethodPut && r.ContentLength > 0 {
		if max := h.Files.MaxUpload; max > 0 && r.ContentLength > max {
			http.Error(w, files.ErrTooLarge.Error(), http.StatusRequestEntityTooLarge)
			return
		}
	}
	wd := &webdav.Handler{
		Prefix:     Prefix,
		FileSystem: &userFS{svc: h.Files, uid: u.ID, uname: u.Username, ip: ip, tmp: h.TmpDir},
		LockSystem: h.lockSystem(u.ID),
		Logger: func(r *http.Request, err error) {
			if err != nil && !errors.Is(err, os.ErrNotExist) {
				log.Printf("dav %s %s: %v", r.Method, r.URL.Path, err)
			}
		},
	}
	wd.ServeHTTP(w, r)
}

// ---------- FileSystem ----------

type userFS struct {
	svc   *files.Service
	uid   int64
	uname string
	ip    string
	tmp   string

	sharedOnce sync.Once
	shared     map[string]sharedEntry
	sharedErr  error
}

type sharedEntry struct {
	node  files.Node
	write bool
}

// loc — разобранный путь.
type loc struct {
	owner   int64
	node    *files.Node // nil — корень владельца (или виртуальная папка)
	write   bool
	virtual bool   // «Доступно мне»
	name    string // отображаемое имя (для корней общих папок)
	shareRt bool   // это корень чужой общей папки
}

func mapErr(err error) error {
	switch {
	case err == nil:
		return nil
	case errors.Is(err, files.ErrNotFound), errors.Is(err, files.ErrNotDir):
		return os.ErrNotExist
	case errors.Is(err, files.ErrExists):
		return os.ErrExist
	case errors.Is(err, files.ErrInvalidName), errors.Is(err, files.ErrCycle), errors.Is(err, files.ErrIsDir),
		errors.Is(err, files.ErrForbidden), errors.Is(err, files.ErrLocked), errors.Is(err, files.ErrCrossOwner):
		return os.ErrPermission
	}
	return err
}

func clean(name string) string { return path.Clean("/" + name) }

func splitPath(p string) []string {
	var out []string
	for _, s := range strings.Split(clean(p), "/") {
		if s != "" {
			out = append(out, s)
		}
	}
	return out
}

// sharedMap — чужие папки и файлы, доступные пользователю, по отображаемому имени.
func (f *userFS) sharedMap(ctx context.Context) (map[string]sharedEntry, error) {
	f.sharedOnce.Do(func() {
		list, err := f.svc.SharedWithMe(ctx, f.uid)
		if err != nil {
			f.sharedErr = err
			return
		}
		f.shared = map[string]sharedEntry{}
		seen := map[string]int{}
		for _, n := range list {
			seen[n.Name]++
		}
		for _, n := range list {
			name := n.Name
			if seen[n.Name] > 1 {
				name = fmt.Sprintf("%s (%s)", n.Name, n.OwnerName)
			}
			f.shared[name] = sharedEntry{node: n, write: n.CanWrite != nil && *n.CanWrite}
		}
	})
	return f.shared, f.sharedErr
}

func (f *userFS) resolve(ctx context.Context, p string) (*loc, error) {
	parts := splitPath(p)
	if len(parts) == 0 {
		return &loc{owner: f.uid, write: true}, nil
	}
	if parts[0] == SharedDir {
		if len(parts) == 1 {
			return &loc{owner: f.uid, virtual: true, name: SharedDir}, nil
		}
		m, err := f.sharedMap(ctx)
		if err != nil {
			return nil, err
		}
		e, ok := m[parts[1]]
		if !ok {
			return nil, os.ErrNotExist
		}
		cur := e.node
		l := &loc{owner: e.node.OwnerID, node: &cur, write: e.write, name: parts[1], shareRt: len(parts) == 2}
		for _, part := range parts[2:] {
			if !l.node.IsDir {
				return nil, os.ErrNotExist
			}
			n, err := f.svc.Lookup(ctx, l.owner, &l.node.ID, part)
			if err != nil {
				return nil, mapErr(err)
			}
			l.node, l.name = n, ""
		}
		return l, nil
	}
	n, err := f.svc.ResolvePath(ctx, f.uid, p)
	if err != nil {
		return nil, mapErr(err)
	}
	return &loc{owner: f.uid, node: n, write: true}, nil
}

// parentOf — папка для создания/переименования: владелец, id папки (nil — корень), имя, права.
func (f *userFS) parentOf(ctx context.Context, name string) (*loc, *string, string, error) {
	name = clean(name)
	if name == "/" {
		return nil, nil, "", os.ErrPermission
	}
	dir, base := path.Split(name)
	l, err := f.resolve(ctx, dir)
	if err != nil {
		return nil, nil, "", err
	}
	if l.virtual {
		return nil, nil, "", os.ErrPermission // в «Доступно мне» создавать нельзя
	}
	if l.node != nil && !l.node.IsDir {
		return nil, nil, "", os.ErrNotExist
	}
	if !l.write {
		return nil, nil, "", os.ErrPermission
	}
	if l.node == nil && base == SharedDir {
		return nil, nil, "", os.ErrPermission
	}
	var id *string
	if l.node != nil {
		id = &l.node.ID
	}
	return l, id, base, nil
}

func (f *userFS) log(ctx context.Context, owner int64, action string, n *files.Node, details string) {
	uid := f.uid
	f.svc.Log(ctx, owner, &uid, f.uname, action, n, details+" (WebDAV)", f.ip)
}

func (f *userFS) Mkdir(ctx context.Context, name string, _ os.FileMode) error {
	l, parent, base, err := f.parentOf(ctx, name)
	if err != nil {
		return err
	}
	n, err := f.svc.Mkdir(ctx, l.owner, parent, base)
	if err == nil {
		f.log(ctx, l.owner, "mkdir", n, "")
	}
	return mapErr(err)
}

func (f *userFS) Stat(ctx context.Context, name string) (os.FileInfo, error) {
	l, err := f.resolve(ctx, name)
	if err != nil {
		return nil, err
	}
	return infoOfLoc(l), nil
}

func (f *userFS) RemoveAll(ctx context.Context, name string) error {
	l, err := f.resolve(ctx, name)
	if err != nil {
		return err
	}
	if l.node == nil || l.virtual || l.shareRt || !l.write {
		return os.ErrPermission
	}
	// Удаление через WebDAV — в корзину владельца, как в Nextcloud.
	if err := f.svc.Trash(ctx, l.owner, l.node.ID); err != nil {
		return mapErr(err)
	}
	f.log(ctx, l.owner, "trash", l.node, "")
	return nil
}

func (f *userFS) Rename(ctx context.Context, oldName, newName string) error {
	src, err := f.resolve(ctx, oldName)
	if err != nil {
		return err
	}
	if src.node == nil || src.virtual || src.shareRt || !src.write {
		return os.ErrPermission
	}
	dst, parent, base, err := f.parentOf(ctx, newName)
	if err != nil {
		return err
	}
	if dst.owner != src.owner {
		return os.ErrPermission
	}
	n, err := f.svc.Move(ctx, src.owner, src.node.ID, parent, base)
	if err == nil {
		f.log(ctx, src.owner, "move", n, "из "+clean(oldName))
	}
	return mapErr(err)
}

func (f *userFS) OpenFile(ctx context.Context, name string, flag int, _ os.FileMode) (webdav.File, error) {
	name = clean(name)
	if flag&(os.O_WRONLY|os.O_RDWR) != 0 {
		l, parent, base, err := f.parentOf(ctx, name)
		if err != nil {
			return nil, err
		}
		existing, err := f.svc.Lookup(ctx, l.owner, parent, base)
		switch {
		case err == nil && existing.IsDir:
			return nil, os.ErrPermission
		case err == nil && flag&os.O_EXCL != 0:
			return nil, os.ErrExist
		case errors.Is(err, files.ErrNotFound) && flag&os.O_CREATE == 0:
			return nil, os.ErrNotExist
		case err != nil && !errors.Is(err, files.ErrNotFound):
			return nil, err
		}
		limit, err := f.writeLimit(ctx, l.owner)
		if err != nil {
			return nil, err
		}
		tmp, err := os.CreateTemp(f.tmp, "dav-*")
		if err != nil {
			return nil, err
		}
		return &writeFile{fs: f, ctx: ctx, owner: l.owner, parent: parent, name: base, tmp: tmp, limit: limit}, nil
	}

	l, err := f.resolve(ctx, name)
	if err != nil {
		return nil, err
	}
	if l.virtual || l.node == nil || l.node.IsDir {
		return &dirFile{fs: f, ctx: ctx, l: l}, nil
	}
	return &readFile{fs: f, ctx: ctx, l: l}, nil
}

// ---------- FileInfo ----------

type fileInfo struct {
	n       *files.Node
	name    string
	virtual bool
}

func infoOf(n *files.Node) fileInfo { return fileInfo{n: n} }

func infoOfLoc(l *loc) fileInfo {
	return fileInfo{n: l.node, name: l.name, virtual: l.virtual}
}

func (i fileInfo) Name() string {
	if i.name != "" {
		return i.name
	}
	if i.n == nil {
		return "/"
	}
	return i.n.Name
}
func (i fileInfo) Size() int64 {
	if i.n == nil || i.n.IsDir {
		return 0
	}
	return i.n.Size
}
func (i fileInfo) Mode() fs.FileMode {
	if i.IsDir() {
		return fs.ModeDir | 0o755
	}
	return 0o644
}
func (i fileInfo) ModTime() time.Time {
	if i.n == nil {
		return time.Unix(0, 0)
	}
	return i.n.UpdatedAt
}
func (i fileInfo) IsDir() bool { return i.virtual || i.n == nil || i.n.IsDir }
func (i fileInfo) Sys() any    { return nil }

// ContentType и ETag — чтобы библиотека webdav не открывала файл ради определения типа.
func (i fileInfo) ContentType(context.Context) (string, error) {
	if i.IsDir() || i.n.Mime == "" {
		return "application/octet-stream", nil
	}
	return i.n.Mime, nil
}

// ETag папки меняется при любом изменении внутри (поддерживается триггером в БД),
// поэтому клиенты синхронизации видят, что внутри что-то поменялось.
func (i fileInfo) ETag(context.Context) (string, error) {
	switch {
	case i.virtual:
		return `"shared-` + strconv.FormatInt(time.Now().Unix()/60, 10) + `"`, nil
	case i.n == nil:
		return `"root"`, nil
	case i.n.Etag != "":
		return `"` + i.n.Etag + `"`, nil
	case i.n.VersionID != nil:
		return `"` + *i.n.VersionID + `"`, nil
	}
	return `"` + i.n.ID + `"`, nil
}

// ---------- папка ----------

type dirFile struct {
	fs   *userFS
	ctx  context.Context
	l    *loc
	list []fs.FileInfo
	pos  int
	read bool
}

func (d *dirFile) Close() error                   { return nil }
func (d *dirFile) Read([]byte) (int, error)       { return 0, os.ErrInvalid }
func (d *dirFile) Write([]byte) (int, error)      { return 0, os.ErrInvalid }
func (d *dirFile) Seek(int64, int) (int64, error) { return 0, nil }
func (d *dirFile) Stat() (os.FileInfo, error) {
	fi := infoOfLoc(d.l)
	if d.l.node == nil && !d.l.virtual {
		// ETag корня берём из users.root_etag
		return rootInfo{fi, d.fs.svc.RootEtag(d.ctx, d.l.owner)}, nil
	}
	return fi, nil
}

type rootInfo struct {
	fileInfo
	etag string
}

func (r rootInfo) ETag(context.Context) (string, error) { return `"` + r.etag + `"`, nil }

func (d *dirFile) load() error {
	if d.read {
		return nil
	}
	d.read = true
	if d.l.virtual {
		m, err := d.fs.sharedMap(d.ctx)
		if err != nil {
			return err
		}
		for name, e := range m {
			n := e.node
			d.list = append(d.list, fileInfo{n: &n, name: name})
		}
		return nil
	}
	var parent *string
	if d.l.node != nil {
		parent = &d.l.node.ID
	}
	l, err := d.fs.svc.List(d.ctx, d.l.owner, parent)
	if err != nil {
		return mapErr(err)
	}
	for i := range l {
		if d.l.node == nil && l[i].Name == SharedDir {
			continue // перекрыто виртуальной папкой
		}
		d.list = append(d.list, infoOf(&l[i]))
	}
	if d.l.node == nil && d.l.owner == d.fs.uid {
		if m, err := d.fs.sharedMap(d.ctx); err == nil && len(m) > 0 {
			d.list = append(d.list, fileInfo{name: SharedDir, virtual: true})
		}
	}
	return nil
}

func (d *dirFile) Readdir(count int) ([]fs.FileInfo, error) {
	if err := d.load(); err != nil {
		return nil, err
	}
	rest := d.list[d.pos:]
	if count > 0 {
		if len(rest) == 0 {
			return nil, io.EOF
		}
		if len(rest) > count {
			rest = rest[:count]
		}
	}
	d.pos += len(rest)
	out := make([]fs.FileInfo, len(rest))
	copy(out, rest)
	return out, nil
}

// DeadProps отдаёт квоту (RFC 4331): Windows, rclone и файловые менеджеры показывают свободное место.
func (d *dirFile) DeadProps() (map[xml.Name]webdav.Property, error) {
	used, quota, err := d.fs.svc.Usage(d.ctx, d.l.owner)
	if err != nil {
		return nil, nil
	}
	avail := int64(1) << 50 // «без ограничения»
	if quota > 0 {
		avail = max(0, quota-used)
	}
	mk := func(local string, v int64) webdav.Property {
		return webdav.Property{XMLName: xml.Name{Space: "DAV:", Local: local}, InnerXML: []byte(strconv.FormatInt(v, 10))}
	}
	return map[xml.Name]webdav.Property{
		{Space: "DAV:", Local: "quota-used-bytes"}:      mk("quota-used-bytes", used),
		{Space: "DAV:", Local: "quota-available-bytes"}: mk("quota-available-bytes", avail),
	}, nil
}

func (d *dirFile) Patch(p []webdav.Proppatch) ([]webdav.Propstat, error) {
	var names []webdav.Property
	for _, pp := range p {
		names = append(names, pp.Props...)
	}
	return []webdav.Propstat{{Props: names, Status: http.StatusForbidden}}, nil
}

// ---------- чтение файла (блоб открывается лениво) ----------

type readFile struct {
	fs   *userFS
	ctx  context.Context
	l    *loc
	blob storage.Blob
}

func (f *readFile) open() error {
	if f.blob != nil {
		return nil
	}
	b, _, err := f.fs.svc.Open(f.ctx, f.l.owner, f.l.node.ID, "")
	if err != nil {
		return mapErr(err)
	}
	f.blob = b
	return nil
}

func (f *readFile) Read(p []byte) (int, error) {
	if err := f.open(); err != nil {
		return 0, err
	}
	return f.blob.Read(p)
}

func (f *readFile) Seek(off int64, whence int) (int64, error) {
	if err := f.open(); err != nil {
		return 0, err
	}
	return f.blob.Seek(off, whence)
}

func (f *readFile) Close() error {
	if f.blob != nil {
		return f.blob.Close()
	}
	return nil
}
func (f *readFile) Write([]byte) (int, error)          { return 0, os.ErrPermission }
func (f *readFile) Readdir(int) ([]fs.FileInfo, error) { return nil, os.ErrInvalid }
func (f *readFile) Stat() (os.FileInfo, error)         { return infoOfLoc(f.l), nil }

// ---------- запись файла: буферизуем во временный файл, коммитим при Close ----------

type writeFile struct {
	fs     *userFS
	ctx    context.Context
	owner  int64
	parent *string
	name   string
	tmp    *os.File
	size   int64
	limit  writeLimit
	failed error // запись прервана — при Close ничего не сохраняем
	closed bool
}

// writeLimit — сколько байт можно принять и какую ошибку вернуть при превышении.
type writeLimit struct {
	max int64 // < 0 — без ограничения
	err error
}

// writeLimit — минимум из CB_MAX_UPLOAD_MB и свободного места в квоте владельца.
// Без этого временный файл рос бы без ограничений, а квота проверялась бы только после приёма всего тела.
func (f *userFS) writeLimit(ctx context.Context, owner int64) (writeLimit, error) {
	l := writeLimit{max: -1}
	if f.svc.MaxUpload > 0 {
		l = writeLimit{max: f.svc.MaxUpload, err: files.ErrTooLarge}
	}
	used, quota, err := f.svc.Usage(ctx, owner)
	if err != nil {
		return l, err
	}
	if free := max(0, quota-used); quota > 0 && (l.max < 0 || free < l.max) {
		l = writeLimit{max: free, err: files.ErrQuota}
	}
	return l, nil
}

func (f *writeFile) Write(p []byte) (int, error) {
	if f.failed != nil {
		return 0, f.failed
	}
	if f.limit.max >= 0 && f.size+int64(len(p)) > f.limit.max {
		f.failed = f.limit.err
		return 0, f.failed
	}
	n, err := f.tmp.Write(p)
	f.size += int64(n)
	if err != nil {
		f.failed = err
	}
	return n, err
}

func (f *writeFile) Read([]byte) (int, error)                  { return 0, os.ErrInvalid }
func (f *writeFile) Seek(off int64, whence int) (int64, error) { return f.tmp.Seek(off, whence) }
func (f *writeFile) Readdir(int) ([]fs.FileInfo, error)        { return nil, os.ErrInvalid }

func (f *writeFile) Stat() (os.FileInfo, error) {
	now := time.Now()
	return fileInfo{n: &files.Node{Name: f.name, Size: f.size, UpdatedAt: now, ID: "pending"}}, nil
}

func (f *writeFile) Close() error {
	if f.closed {
		return nil
	}
	f.closed = true
	defer func() { f.tmp.Close(); os.Remove(f.tmp.Name()) }()
	if f.failed != nil {
		// Тело принято не полностью: сохранять обрезанный файл нельзя.
		return mapErr(f.failed)
	}
	// Файл уже на диске — передаём его ядру напрямую, без повторного копирования.
	n, err := f.fs.svc.WriteFile(f.ctx, f.owner, f.parent, f.name, f.tmp)
	if err == nil {
		f.fs.log(f.ctx, f.owner, "upload", n, "")
		f.fs.svc.CheckQuotaWarning(f.ctx, f.owner)
	}
	return mapErr(err)
}
