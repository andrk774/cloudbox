// Package dav — WebDAV-доступ к файлам (подключение как сетевой диск в Windows, Linux, macOS,
// Android через приложения вроде Solid Explorer / Total Commander, rclone, Joplin и т.д.).
package dav

import (
	"context"
	"errors"
	"io"
	"io/fs"
	"log"
	"net/http"
	"os"
	"path"
	"strings"
	"sync"
	"time"

	"github.com/andrk774/cloudbox/internal/auth"
	"github.com/andrk774/cloudbox/internal/files"
	"github.com/andrk774/cloudbox/internal/storage"
	"golang.org/x/net/webdav"
)

const Prefix = "/dav"

type Handler struct {
	Auth   *auth.Service
	Files  *files.Service
	TmpDir string

	mu    sync.Mutex
	locks map[int64]webdav.LockSystem
	fails map[string]*failCounter
}

type failCounter struct {
	n     int
	reset time.Time
}

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

// tooManyFails — защита от перебора паролей: 20 неудач за 10 минут с одного адреса.
func (h *Handler) tooManyFails(ip string, add bool) bool {
	h.mu.Lock()
	defer h.mu.Unlock()
	if h.fails == nil {
		h.fails = map[string]*failCounter{}
	}
	now := time.Now()
	f, ok := h.fails[ip]
	if !ok || now.After(f.reset) {
		f = &failCounter{reset: now.Add(10 * time.Minute)}
		h.fails[ip] = f
	}
	if add {
		f.n++
	}
	return f.n >= 20
}

func (h *Handler) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	ip := r.RemoteAddr
	if i := strings.LastIndex(ip, ":"); i > 0 {
		ip = ip[:i]
	}
	if h.tooManyFails(ip, false) {
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
			h.tooManyFails(ip, true)
		}
		w.Header().Set("WWW-Authenticate", `Basic realm="CloudBox", charset="UTF-8"`)
		http.Error(w, "unauthorized", http.StatusUnauthorized)
		return
	}
	wd := &webdav.Handler{
		Prefix:     Prefix,
		FileSystem: &userFS{svc: h.Files, uid: u.ID, tmp: h.TmpDir},
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
	svc *files.Service
	uid int64
	tmp string
}

func mapErr(err error) error {
	switch {
	case err == nil:
		return nil
	case errors.Is(err, files.ErrNotFound), errors.Is(err, files.ErrNotDir):
		return os.ErrNotExist
	case errors.Is(err, files.ErrExists):
		return os.ErrExist
	case errors.Is(err, files.ErrInvalidName), errors.Is(err, files.ErrCycle), errors.Is(err, files.ErrIsDir):
		return os.ErrPermission
	}
	return err
}

func clean(name string) string { return path.Clean("/" + name) }

// parentOf находит папку-родителя для пути и возвращает её id (nil — корень) и базовое имя.
func (f *userFS) parentOf(ctx context.Context, name string) (*string, string, error) {
	name = clean(name)
	if name == "/" {
		return nil, "", os.ErrPermission
	}
	dir, base := path.Split(name)
	p, err := f.svc.ResolvePath(ctx, f.uid, dir)
	if err != nil {
		return nil, "", mapErr(err)
	}
	if p == nil {
		return nil, base, nil
	}
	if !p.IsDir {
		return nil, "", os.ErrNotExist
	}
	return &p.ID, base, nil
}

func (f *userFS) Mkdir(ctx context.Context, name string, _ os.FileMode) error {
	parent, base, err := f.parentOf(ctx, name)
	if err != nil {
		return err
	}
	_, err = f.svc.Mkdir(ctx, f.uid, parent, base)
	return mapErr(err)
}

func (f *userFS) Stat(ctx context.Context, name string) (os.FileInfo, error) {
	n, err := f.svc.ResolvePath(ctx, f.uid, clean(name))
	if err != nil {
		return nil, mapErr(err)
	}
	return infoOf(n), nil
}

func (f *userFS) RemoveAll(ctx context.Context, name string) error {
	n, err := f.svc.ResolvePath(ctx, f.uid, clean(name))
	if err != nil {
		return mapErr(err)
	}
	if n == nil {
		return os.ErrPermission
	}
	// Удаление через WebDAV — в корзину, как в Nextcloud.
	return mapErr(f.svc.Trash(ctx, f.uid, n.ID))
}

func (f *userFS) Rename(ctx context.Context, oldName, newName string) error {
	n, err := f.svc.ResolvePath(ctx, f.uid, clean(oldName))
	if err != nil {
		return mapErr(err)
	}
	if n == nil {
		return os.ErrPermission
	}
	parent, base, err := f.parentOf(ctx, newName)
	if err != nil {
		return err
	}
	_, err = f.svc.Move(ctx, f.uid, n.ID, parent, base)
	return mapErr(err)
}

func (f *userFS) OpenFile(ctx context.Context, name string, flag int, _ os.FileMode) (webdav.File, error) {
	name = clean(name)
	if flag&(os.O_WRONLY|os.O_RDWR) != 0 {
		parent, base, err := f.parentOf(ctx, name)
		if err != nil {
			return nil, err
		}
		existing, err := f.svc.Lookup(ctx, f.uid, parent, base)
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
		tmp, err := os.CreateTemp(f.tmp, "dav-*")
		if err != nil {
			return nil, err
		}
		return &writeFile{fs: f, ctx: ctx, parent: parent, name: base, tmp: tmp}, nil
	}

	n, err := f.svc.ResolvePath(ctx, f.uid, name)
	if err != nil {
		return nil, mapErr(err)
	}
	if n == nil || n.IsDir {
		return &dirFile{fs: f, ctx: ctx, node: n}, nil
	}
	return &readFile{fs: f, ctx: ctx, node: n}, nil
}

// ---------- FileInfo ----------

type fileInfo struct {
	n *files.Node
}

func infoOf(n *files.Node) fileInfo { return fileInfo{n} }

func (i fileInfo) Name() string {
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
func (i fileInfo) IsDir() bool { return i.n == nil || i.n.IsDir }
func (i fileInfo) Sys() any    { return nil }

// ContentType и ETag — чтобы библиотека webdav не открывала файл ради определения типа.
func (i fileInfo) ContentType(context.Context) (string, error) {
	if i.IsDir() || i.n.Mime == "" {
		return "application/octet-stream", nil
	}
	return i.n.Mime, nil
}

func (i fileInfo) ETag(context.Context) (string, error) {
	if i.n == nil {
		return `"root"`, nil
	}
	if i.n.VersionID != nil {
		return `"` + *i.n.VersionID + `"`, nil
	}
	return `"` + i.n.ID + i.n.UpdatedAt.Format("150405.000000") + `"`, nil
}

// ---------- папка ----------

type dirFile struct {
	fs   *userFS
	ctx  context.Context
	node *files.Node // nil — корень
	list []files.Node
	pos  int
	read bool
}

func (d *dirFile) Close() error                   { return nil }
func (d *dirFile) Read([]byte) (int, error)       { return 0, os.ErrInvalid }
func (d *dirFile) Write([]byte) (int, error)      { return 0, os.ErrInvalid }
func (d *dirFile) Seek(int64, int) (int64, error) { return 0, nil }
func (d *dirFile) Stat() (os.FileInfo, error)     { return infoOf(d.node), nil }
func (d *dirFile) Readdir(count int) ([]fs.FileInfo, error) {
	if !d.read {
		var parent *string
		if d.node != nil {
			parent = &d.node.ID
		}
		l, err := d.fs.svc.List(d.ctx, d.fs.uid, parent)
		if err != nil {
			return nil, mapErr(err)
		}
		d.list, d.read = l, true
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
	for i := range rest {
		out[i] = infoOf(&rest[i])
	}
	return out, nil
}

// ---------- чтение файла (блоб открывается лениво) ----------

type readFile struct {
	fs   *userFS
	ctx  context.Context
	node *files.Node
	blob storage.Blob
}

func (f *readFile) open() error {
	if f.blob != nil {
		return nil
	}
	b, _, err := f.fs.svc.Open(f.ctx, f.fs.uid, f.node.ID, "")
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
func (f *readFile) Stat() (os.FileInfo, error)         { return infoOf(f.node), nil }

// ---------- запись файла: буферизуем во временный файл, коммитим при Close ----------

type writeFile struct {
	fs     *userFS
	ctx    context.Context
	parent *string
	name   string
	tmp    *os.File
	size   int64
	closed bool
}

func (f *writeFile) Write(p []byte) (int, error) {
	n, err := f.tmp.Write(p)
	f.size += int64(n)
	return n, err
}

func (f *writeFile) Read([]byte) (int, error)                  { return 0, os.ErrInvalid }
func (f *writeFile) Seek(off int64, whence int) (int64, error) { return f.tmp.Seek(off, whence) }
func (f *writeFile) Readdir(int) ([]fs.FileInfo, error)        { return nil, os.ErrInvalid }

func (f *writeFile) Stat() (os.FileInfo, error) {
	now := time.Now()
	return fileInfo{&files.Node{Name: f.name, Size: f.size, UpdatedAt: now, ID: "pending"}}, nil
}

func (f *writeFile) Close() error {
	if f.closed {
		return nil
	}
	f.closed = true
	defer func() { f.tmp.Close(); os.Remove(f.tmp.Name()) }()
	if _, err := f.tmp.Seek(0, io.SeekStart); err != nil {
		return err
	}
	_, err := f.fs.svc.Write(f.ctx, f.fs.uid, f.parent, f.name, f.tmp)
	return mapErr(err)
}
