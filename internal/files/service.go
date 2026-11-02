package files

import (
	"context"
	"crypto/sha256"
	"database/sql"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"log"
	"mime"
	"net/http"
	"os"
	"path"
	"strings"
	"time"

	"github.com/andrk774/cloudbox/internal/storage"
	"github.com/lib/pq"
)

type Service struct {
	DB          *sql.DB
	St          storage.Storage
	TmpDir      string
	MaxVersions int
	MaxUpload   int64 // байт
}

type querier interface {
	QueryRowContext(ctx context.Context, q string, args ...any) *sql.Row
	QueryContext(ctx context.Context, q string, args ...any) (*sql.Rows, error)
	ExecContext(ctx context.Context, q string, args ...any) (sql.Result, error)
}

// ---------- чтение дерева ----------

func (s *Service) Get(ctx context.Context, uid int64, id string) (*Node, error) {
	return s.get(ctx, s.DB, uid, id)
}

func (s *Service) get(ctx context.Context, q querier, uid int64, id string) (*Node, error) {
	if !ValidID(id) {
		return nil, ErrNotFound
	}
	return scanNode(q.QueryRowContext(ctx,
		`SELECT `+nodeCols+` FROM nodes WHERE id=$1 AND owner_id=$2 AND deleted_at IS NULL`, id, uid))
}

// checkDir: parent == nil — корень (всегда ок), иначе это должна быть живая папка.
func (s *Service) checkDir(ctx context.Context, q querier, uid int64, parent *string) error {
	if parent == nil {
		return nil
	}
	n, err := s.get(ctx, q, uid, *parent)
	if err != nil {
		return err
	}
	if !n.IsDir {
		return ErrNotDir
	}
	return nil
}

func (s *Service) List(ctx context.Context, uid int64, parent *string) ([]Node, error) {
	if err := s.checkDir(ctx, s.DB, uid, parent); err != nil {
		return nil, err
	}
	rows, err := s.DB.QueryContext(ctx, `SELECT `+nodeCols+` FROM nodes
		WHERE owner_id=$1 AND parent_id IS NOT DISTINCT FROM $2 AND deleted_at IS NULL
		ORDER BY is_dir DESC, lower(name)`, uid, parent)
	if err != nil {
		return nil, err
	}
	return collect(rows)
}

func collect(rows *sql.Rows) ([]Node, error) {
	defer rows.Close()
	out := []Node{}
	for rows.Next() {
		n, err := scanNode(rows)
		if err != nil {
			return nil, err
		}
		out = append(out, *n)
	}
	return out, rows.Err()
}

func (s *Service) Lookup(ctx context.Context, uid int64, parent *string, name string) (*Node, error) {
	return s.lookup(ctx, s.DB, uid, parent, name, false)
}

func (s *Service) lookup(ctx context.Context, q querier, uid int64, parent *string, name string, forUpdate bool) (*Node, error) {
	sqlq := `SELECT ` + nodeCols + ` FROM nodes
		WHERE owner_id=$1 AND parent_id IS NOT DISTINCT FROM $2 AND name=$3 AND deleted_at IS NULL`
	if forUpdate {
		sqlq += ` FOR UPDATE`
	}
	return scanNode(q.QueryRowContext(ctx, sqlq, uid, parent, name))
}

// ResolvePath находит узел по пути "/a/b/c". Для корня возвращает nil, nil.
func (s *Service) ResolvePath(ctx context.Context, uid int64, p string) (*Node, error) {
	var cur *Node
	for _, part := range strings.Split(path.Clean("/"+p), "/") {
		if part == "" {
			continue
		}
		var parent *string
		if cur != nil {
			if !cur.IsDir {
				return nil, ErrNotFound
			}
			parent = &cur.ID
		}
		n, err := s.Lookup(ctx, uid, parent, part)
		if err != nil {
			return nil, err
		}
		cur = n
	}
	return cur, nil
}

// Ancestors — цепочка от корня до узла включительно (для «хлебных крошек»).
func (s *Service) Ancestors(ctx context.Context, uid int64, id string) ([]Node, error) {
	if !ValidID(id) {
		return nil, ErrNotFound
	}
	rows, err := s.DB.QueryContext(ctx, `
		WITH RECURSIVE a AS (
			SELECT `+nodeCols+`, 0 AS depth FROM nodes WHERE id=$1 AND owner_id=$2
			UNION ALL
			SELECT n.id, n.owner_id, n.parent_id, n.name, n.is_dir, n.size, n.mime, n.current_version_id,
			       n.created_at, n.updated_at, n.deleted_at, a.depth+1
			FROM nodes n JOIN a ON n.id = a.parent_id
		) SELECT `+nodeCols+` FROM a ORDER BY depth DESC`, id, uid)
	if err != nil {
		return nil, err
	}
	out, err := collect(rows)
	if err == nil && len(out) == 0 {
		err = ErrNotFound
	}
	return out, err
}

// PathOf — человекочитаемый путь "/Документы/отчёт.pdf".
func (s *Service) PathOf(ctx context.Context, uid int64, id string) (string, error) {
	anc, err := s.Ancestors(ctx, uid, id)
	if err != nil {
		return "", err
	}
	parts := make([]string, len(anc))
	for i, n := range anc {
		parts[i] = n.Name
	}
	return "/" + strings.Join(parts, "/"), nil
}

func (s *Service) Search(ctx context.Context, uid int64, q string) ([]Node, error) {
	q = strings.NewReplacer(`\`, `\\`, `%`, `\%`, `_`, `\_`).Replace(strings.TrimSpace(q))
	if q == "" {
		return []Node{}, nil
	}
	rows, err := s.DB.QueryContext(ctx, `SELECT `+nodeCols+` FROM nodes
		WHERE owner_id=$1 AND deleted_at IS NULL AND name ILIKE '%' || $2 || '%'
		ORDER BY is_dir DESC, updated_at DESC LIMIT 200`, uid, q)
	if err != nil {
		return nil, err
	}
	return collect(rows)
}

func (s *Service) Recent(ctx context.Context, uid int64, limit int) ([]Node, error) {
	rows, err := s.DB.QueryContext(ctx, `SELECT `+nodeCols+` FROM nodes
		WHERE owner_id=$1 AND deleted_at IS NULL AND NOT is_dir
		ORDER BY updated_at DESC LIMIT $2`, uid, limit)
	if err != nil {
		return nil, err
	}
	return collect(rows)
}

// IsInside — лежит ли node внутри ancestor (или совпадает с ним). Нужно для публичных ссылок на папки.
func (s *Service) IsInside(ctx context.Context, uid int64, ancestor, node string) (bool, error) {
	if !ValidID(node) {
		return false, nil
	}
	var ok bool
	err := s.DB.QueryRowContext(ctx, `
		WITH RECURSIVE a AS (
			SELECT id, parent_id FROM nodes WHERE id=$1 AND owner_id=$3 AND deleted_at IS NULL
			UNION ALL
			SELECT n.id, n.parent_id FROM nodes n JOIN a ON n.id=a.parent_id
		) SELECT EXISTS(SELECT 1 FROM a WHERE id=$2)`, node, ancestor, uid).Scan(&ok)
	return ok, err
}

// ---------- квоты ----------

func (s *Service) Usage(ctx context.Context, uid int64) (used, quota int64, err error) {
	err = s.DB.QueryRowContext(ctx, `SELECT COALESCE((SELECT SUM(size) FROM versions WHERE owner_id=$1),0), quota_bytes FROM users WHERE id=$1`, uid).Scan(&used, &quota)
	return
}

func (s *Service) checkQuota(ctx context.Context, uid, extra int64) error {
	used, quota, err := s.Usage(ctx, uid)
	if err != nil {
		return err
	}
	if quota > 0 && used+extra > quota {
		return ErrQuota
	}
	return nil
}

// ---------- запись ----------

func (s *Service) Mkdir(ctx context.Context, uid int64, parent *string, name string) (*Node, error) {
	if err := ValidateName(name); err != nil {
		return nil, err
	}
	if err := s.checkDir(ctx, s.DB, uid, parent); err != nil {
		return nil, err
	}
	n, err := scanNode(s.DB.QueryRowContext(ctx, `INSERT INTO nodes (id, owner_id, parent_id, name, is_dir)
		VALUES ($1,$2,$3,$4,TRUE) RETURNING `+nodeCols, NewID(), uid, parent, name))
	if isUniqueViolation(err) {
		return nil, ErrExists
	}
	return n, err
}

// EnsureDirs создаёт цепочку папок rel ("a/b/c") внутри parent, возвращает id последней.
func (s *Service) EnsureDirs(ctx context.Context, uid int64, parent *string, rel string) (*string, error) {
	cur := parent
	for _, part := range strings.Split(rel, "/") {
		if part == "" || part == "." {
			continue
		}
		n, err := s.Lookup(ctx, uid, cur, part)
		if errors.Is(err, ErrNotFound) {
			n, err = s.Mkdir(ctx, uid, cur, part)
			if errors.Is(err, ErrExists) { // гонка параллельных загрузок
				n, err = s.Lookup(ctx, uid, cur, part)
			}
		}
		if err != nil {
			return nil, err
		}
		if !n.IsDir {
			return nil, ErrNotDir
		}
		id := n.ID
		cur = &id
	}
	return cur, nil
}

type spooled struct {
	f    *os.File
	size int64
	sum  string
	mime string
}

func (s *Service) spool(r io.Reader, name string) (*spooled, error) {
	f, err := os.CreateTemp(s.TmpDir, "cloudbox-*")
	if err != nil {
		return nil, err
	}
	h := sha256.New()
	limit := s.MaxUpload
	if limit <= 0 {
		limit = 1 << 62
	}
	n, err := io.Copy(io.MultiWriter(f, h), io.LimitReader(r, limit+1))
	if err == nil && n > limit {
		err = ErrTooLarge
	}
	if err != nil {
		f.Close()
		os.Remove(f.Name())
		return nil, err
	}
	m := mime.TypeByExtension(strings.ToLower(path.Ext(name)))
	if m == "" {
		buf := make([]byte, 512)
		k, _ := f.ReadAt(buf, 0)
		m = http.DetectContentType(buf[:k])
	}
	if i := strings.Index(m, ";"); i > 0 && !strings.HasPrefix(m, "text/") {
		m = m[:i]
	}
	return &spooled{f: f, size: n, sum: hex.EncodeToString(h.Sum(nil)), mime: m}, nil
}

func (sp *spooled) close() { sp.f.Close(); os.Remove(sp.f.Name()) }

func newBlobKey() string { return strings.ReplaceAll(NewID(), "-", "") }

// Write создаёт файл или добавляет новую версию существующего.
func (s *Service) Write(ctx context.Context, uid int64, parent *string, name string, r io.Reader) (*Node, error) {
	if err := ValidateName(name); err != nil {
		return nil, err
	}
	if err := s.checkDir(ctx, s.DB, uid, parent); err != nil {
		return nil, err
	}
	sp, err := s.spool(r, name)
	if err != nil {
		return nil, err
	}
	defer sp.close()
	if err := s.checkQuota(ctx, uid, sp.size); err != nil {
		return nil, err
	}
	key := newBlobKey()
	if _, err := sp.f.Seek(0, io.SeekStart); err != nil {
		return nil, err
	}
	if err := s.St.Put(ctx, key, sp.f, sp.size); err != nil {
		return nil, fmt.Errorf("storage: %w", err)
	}
	var node *Node
	for attempt := 0; attempt < 3; attempt++ {
		node, err = s.commitWrite(ctx, uid, parent, name, key, sp)
		if !errors.Is(err, ErrExists) { // ErrExists = гонка с параллельной загрузкой, пробуем как перезапись
			break
		}
	}
	if err != nil {
		s.queueGC(context.Background(), s.DB, key)
		return nil, err
	}
	return node, nil
}

func (s *Service) commitWrite(ctx context.Context, uid int64, parent *string, name, key string, sp *spooled) (*Node, error) {
	tx, err := s.DB.BeginTx(ctx, nil)
	if err != nil {
		return nil, err
	}
	defer tx.Rollback()
	if err := s.checkDir(ctx, tx, uid, parent); err != nil {
		return nil, err
	}
	vid := NewID()
	existing, err := s.lookup(ctx, tx, uid, parent, name, true)
	var nodeID string
	switch {
	case err == nil:
		if existing.IsDir {
			return nil, ErrIsDir
		}
		nodeID = existing.ID
		if _, err := tx.ExecContext(ctx, `UPDATE nodes SET current_version_id=$1, size=$2, mime=$3, updated_at=now() WHERE id=$4`,
			vid, sp.size, sp.mime, nodeID); err != nil {
			return nil, err
		}
	case errors.Is(err, ErrNotFound):
		nodeID = NewID()
		if _, err := tx.ExecContext(ctx, `INSERT INTO nodes (id, owner_id, parent_id, name, is_dir, size, mime, current_version_id)
			VALUES ($1,$2,$3,$4,FALSE,$5,$6,$7)`, nodeID, uid, parent, name, sp.size, sp.mime, vid); err != nil {
			if isUniqueViolation(err) {
				return nil, ErrExists
			}
			return nil, err
		}
	default:
		return nil, err
	}
	if _, err := tx.ExecContext(ctx, `INSERT INTO versions (id, node_id, owner_id, blob_key, size, sha256) VALUES ($1,$2,$3,$4,$5,$6)`,
		vid, nodeID, uid, key, sp.size, sp.sum); err != nil {
		return nil, err
	}
	if err := s.pruneVersions(ctx, tx, nodeID); err != nil {
		return nil, err
	}
	n, err := s.get(ctx, tx, uid, nodeID)
	if err != nil {
		return nil, err
	}
	return n, tx.Commit()
}

// pruneVersions оставляет текущую версию + MaxVersions предыдущих.
func (s *Service) pruneVersions(ctx context.Context, q querier, nodeID string) error {
	if s.MaxVersions < 0 {
		return nil
	}
	rows, err := q.QueryContext(ctx, `SELECT v.id, v.blob_key FROM versions v JOIN nodes n ON n.id=v.node_id
		WHERE v.node_id=$1 AND v.id IS DISTINCT FROM n.current_version_id
		ORDER BY v.created_at DESC OFFSET $2`, nodeID, s.MaxVersions)
	if err != nil {
		return err
	}
	var ids, keys []string
	for rows.Next() {
		var id, k string
		if err := rows.Scan(&id, &k); err != nil {
			rows.Close()
			return err
		}
		ids, keys = append(ids, id), append(keys, k)
	}
	rows.Close()
	if len(ids) == 0 {
		return nil
	}
	if _, err := q.ExecContext(ctx, `DELETE FROM versions WHERE id = ANY($1::uuid[])`, pq.Array(ids)); err != nil {
		return err
	}
	return s.queueGC(ctx, q, keys...)
}

// ---------- чтение содержимого ----------

// Open открывает текущую (vid == "") или указанную версию файла.
func (s *Service) Open(ctx context.Context, uid int64, id, vid string) (storage.Blob, *Node, error) {
	n, err := s.Get(ctx, uid, id)
	if err != nil {
		return nil, nil, err
	}
	if n.IsDir {
		return nil, n, ErrIsDir
	}
	if vid == "" {
		if n.VersionID == nil {
			return nil, n, ErrNotFound
		}
		vid = *n.VersionID
	}
	if !ValidID(vid) {
		return nil, n, ErrNotFound
	}
	var key string
	err = s.DB.QueryRowContext(ctx, `SELECT blob_key FROM versions WHERE id=$1 AND node_id=$2`, vid, n.ID).Scan(&key)
	if errors.Is(err, sql.ErrNoRows) {
		return nil, n, ErrNotFound
	}
	if err != nil {
		return nil, n, err
	}
	b, err := s.St.Get(ctx, key)
	if errors.Is(err, storage.ErrNotFound) {
		return nil, n, ErrNotFound
	}
	return b, n, err
}

// ---------- перемещение и копирование ----------

func (s *Service) Move(ctx context.Context, uid int64, id string, newParent *string, newName string) (*Node, error) {
	if err := ValidateName(newName); err != nil {
		return nil, err
	}
	n, err := s.Get(ctx, uid, id)
	if err != nil {
		return nil, err
	}
	if err := s.checkDir(ctx, s.DB, uid, newParent); err != nil {
		return nil, err
	}
	if n.IsDir && newParent != nil {
		inside, err := s.IsInside(ctx, uid, n.ID, *newParent)
		if err != nil {
			return nil, err
		}
		if inside {
			return nil, ErrCycle
		}
	}
	res, err := scanNode(s.DB.QueryRowContext(ctx, `UPDATE nodes SET parent_id=$1, name=$2, updated_at=now()
		WHERE id=$3 AND owner_id=$4 AND deleted_at IS NULL RETURNING `+nodeCols, newParent, newName, id, uid))
	if isUniqueViolation(err) {
		return nil, ErrExists
	}
	return res, err
}

// subtreeSize — суммарный размер текущих версий поддерева.
func (s *Service) subtreeSize(ctx context.Context, q querier, id string) (int64, error) {
	var size int64
	err := q.QueryRowContext(ctx, `
		WITH RECURSIVE t AS (
			SELECT id, size, is_dir FROM nodes WHERE id=$1
			UNION ALL SELECT n.id, n.size, n.is_dir FROM nodes n JOIN t ON n.parent_id=t.id WHERE n.deleted_at IS NULL
		) SELECT COALESCE(SUM(size) FILTER (WHERE NOT is_dir),0) FROM t`, id).Scan(&size)
	return size, err
}

// Copy копирует файл или папку целиком. Блобы не дублируются: новые версии ссылаются на те же ключи.
func (s *Service) Copy(ctx context.Context, uid int64, id string, newParent *string, newName string) (*Node, error) {
	if err := ValidateName(newName); err != nil {
		return nil, err
	}
	src, err := s.Get(ctx, uid, id)
	if err != nil {
		return nil, err
	}
	if src.IsDir && newParent != nil {
		if inside, err := s.IsInside(ctx, uid, src.ID, *newParent); err != nil {
			return nil, err
		} else if inside {
			return nil, ErrCycle
		}
	}
	size, err := s.subtreeSize(ctx, s.DB, src.ID)
	if err != nil {
		return nil, err
	}
	if err := s.checkQuota(ctx, uid, size); err != nil {
		return nil, err
	}
	tx, err := s.DB.BeginTx(ctx, nil)
	if err != nil {
		return nil, err
	}
	defer tx.Rollback()
	if err := s.checkDir(ctx, tx, uid, newParent); err != nil {
		return nil, err
	}
	newID, err := s.copyRec(ctx, tx, uid, src, newParent, newName)
	if err != nil {
		return nil, err
	}
	n, err := s.get(ctx, tx, uid, newID)
	if err != nil {
		return nil, err
	}
	return n, tx.Commit()
}

func (s *Service) copyRec(ctx context.Context, tx *sql.Tx, uid int64, src *Node, parent *string, name string) (string, error) {
	id := NewID()
	var vid *string
	if !src.IsDir && src.VersionID != nil {
		v := NewID()
		vid = &v
	}
	if _, err := tx.ExecContext(ctx, `INSERT INTO nodes (id, owner_id, parent_id, name, is_dir, size, mime, current_version_id)
		VALUES ($1,$2,$3,$4,$5,$6,$7,$8)`, id, uid, parent, name, src.IsDir, src.Size, src.Mime, vid); err != nil {
		if isUniqueViolation(err) {
			return "", ErrExists
		}
		return "", err
	}
	if vid != nil {
		if _, err := tx.ExecContext(ctx, `INSERT INTO versions (id, node_id, owner_id, blob_key, size, sha256)
			SELECT $1, $2, owner_id, blob_key, size, sha256 FROM versions WHERE id=$3`, *vid, id, *src.VersionID); err != nil {
			return "", err
		}
	}
	if src.IsDir {
		rows, err := tx.QueryContext(ctx, `SELECT `+nodeCols+` FROM nodes WHERE parent_id=$1 AND deleted_at IS NULL`, src.ID)
		if err != nil {
			return "", err
		}
		children, err := collect(rows)
		if err != nil {
			return "", err
		}
		for i := range children {
			if _, err := s.copyRec(ctx, tx, uid, &children[i], &id, children[i].Name); err != nil {
				return "", err
			}
		}
	}
	return id, nil
}

// ---------- корзина ----------

func (s *Service) Trash(ctx context.Context, uid int64, id string) error {
	n, err := s.Get(ctx, uid, id)
	if err != nil {
		return err
	}
	now := time.Now().UTC().Truncate(time.Microsecond)
	tx, err := s.DB.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer tx.Rollback()
	if _, err := tx.ExecContext(ctx, `
		WITH RECURSIVE t AS (
			SELECT id FROM nodes WHERE id=$1
			UNION ALL SELECT n.id FROM nodes n JOIN t ON n.parent_id=t.id WHERE n.deleted_at IS NULL
		) UPDATE nodes SET deleted_at=$2 WHERE id IN (SELECT id FROM t)`, n.ID, now); err != nil {
		return err
	}
	if _, err := tx.ExecContext(ctx, `UPDATE nodes SET trash_root=TRUE, trash_parent_id=parent_id WHERE id=$1`, n.ID); err != nil {
		return err
	}
	return tx.Commit()
}

func (s *Service) ListTrash(ctx context.Context, uid int64) ([]Node, error) {
	rows, err := s.DB.QueryContext(ctx, `SELECT `+nodeCols+`, trash_parent_id FROM nodes
		WHERE owner_id=$1 AND trash_root AND deleted_at IS NOT NULL ORDER BY deleted_at DESC`, uid)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := []Node{}
	var parents []sql.NullString
	for rows.Next() {
		var n Node
		var parent, ver, tp sql.NullString
		var del sql.NullTime
		if err := rows.Scan(&n.ID, &n.OwnerID, &parent, &n.Name, &n.IsDir, &n.Size, &n.Mime, &ver, &n.CreatedAt, &n.UpdatedAt, &del, &tp); err != nil {
			return nil, err
		}
		if del.Valid {
			n.DeletedAt = &del.Time
		}
		out = append(out, n)
		parents = append(parents, tp)
	}
	if err := rows.Err(); err != nil {
		return nil, err
	}
	rows.Close()
	for i := range out {
		out[i].OrigPath = "/"
		if parents[i].Valid {
			if p, err := s.PathOf(ctx, uid, parents[i].String); err == nil {
				out[i].OrigPath = p
			}
		}
	}
	return out, nil
}

type trashRoot struct {
	id        string
	name      string
	deletedAt time.Time
	parent    *string
}

func (s *Service) getTrashRoot(ctx context.Context, q querier, uid int64, id string) (*trashRoot, error) {
	if !ValidID(id) {
		return nil, ErrNotFound
	}
	var tr trashRoot
	var tp sql.NullString
	err := q.QueryRowContext(ctx, `SELECT id, name, deleted_at, trash_parent_id FROM nodes
		WHERE id=$1 AND owner_id=$2 AND trash_root AND deleted_at IS NOT NULL FOR UPDATE`, id, uid).
		Scan(&tr.id, &tr.name, &tr.deletedAt, &tp)
	if errors.Is(err, sql.ErrNoRows) {
		return nil, ErrNotFound
	}
	if tp.Valid {
		tr.parent = &tp.String
	}
	return &tr, err
}

// Restore возвращает узел из корзины на прежнее место (или в корень, если папки уже нет).
func (s *Service) Restore(ctx context.Context, uid int64, id string) (*Node, error) {
	tx, err := s.DB.BeginTx(ctx, nil)
	if err != nil {
		return nil, err
	}
	defer tx.Rollback()
	tr, err := s.getTrashRoot(ctx, tx, uid, id)
	if err != nil {
		return nil, err
	}
	target := tr.parent
	if target != nil {
		if err := s.checkDir(ctx, tx, uid, target); err != nil {
			target = nil
		}
	}
	name, err := s.freeName(ctx, tx, uid, target, tr.name)
	if err != nil {
		return nil, err
	}
	if _, err := tx.ExecContext(ctx, `UPDATE nodes SET deleted_at=NULL, trash_root=FALSE, trash_parent_id=NULL,
		parent_id=$1, name=$2 WHERE id=$3`, target, name, tr.id); err != nil {
		return nil, err
	}
	if _, err := tx.ExecContext(ctx, `
		WITH RECURSIVE t AS (
			SELECT id FROM nodes WHERE parent_id=$1 AND deleted_at=$2
			UNION ALL SELECT n.id FROM nodes n JOIN t ON n.parent_id=t.id WHERE n.deleted_at=$2
		) UPDATE nodes SET deleted_at=NULL WHERE id IN (SELECT id FROM t)`, tr.id, tr.deletedAt); err != nil {
		return nil, err
	}
	n, err := s.get(ctx, tx, uid, tr.id)
	if err != nil {
		return nil, err
	}
	return n, tx.Commit()
}

// freeName подбирает свободное имя: "файл.txt" → "файл (2).txt".
func (s *Service) freeName(ctx context.Context, q querier, uid int64, parent *string, name string) (string, error) {
	ext := path.Ext(name)
	base := strings.TrimSuffix(name, ext)
	if base == "" {
		base, ext = name, ""
	}
	cand := name
	for i := 2; i < 1000; i++ {
		_, err := s.lookup(ctx, q, uid, parent, cand, false)
		if errors.Is(err, ErrNotFound) {
			return cand, nil
		}
		if err != nil {
			return "", err
		}
		cand = fmt.Sprintf("%s (%d)%s", base, i, ext)
	}
	return "", ErrExists
}

// FreeName — публичная обёртка (для загрузки без перезаписи).
func (s *Service) FreeName(ctx context.Context, uid int64, parent *string, name string) (string, error) {
	return s.freeName(ctx, s.DB, uid, parent, name)
}

// Purge удаляет узел из корзины навсегда.
func (s *Service) Purge(ctx context.Context, uid int64, id string) error {
	tx, err := s.DB.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer tx.Rollback()
	tr, err := s.getTrashRoot(ctx, tx, uid, id)
	if err != nil {
		return err
	}
	// Вложенные объекты, удалённые в корзину раньше отдельно, не должны исчезнуть вместе с папкой.
	if _, err := tx.ExecContext(ctx, `
		WITH RECURSIVE t AS (
			SELECT id FROM nodes WHERE parent_id=$1
			UNION ALL SELECT n.id FROM nodes n JOIN t ON n.parent_id=t.id
		) UPDATE nodes SET parent_id=NULL, trash_parent_id=NULL
		  WHERE id IN (SELECT id FROM t) AND trash_root AND deleted_at <> $2`, tr.id, tr.deletedAt); err != nil {
		return err
	}
	if _, err := tx.ExecContext(ctx, `
		WITH RECURSIVE t AS (
			SELECT id FROM nodes WHERE id=$1
			UNION ALL SELECT n.id FROM nodes n JOIN t ON n.parent_id=t.id
		) INSERT INTO blob_gc (blob_key) SELECT blob_key FROM versions WHERE node_id IN (SELECT id FROM t)
		  ON CONFLICT DO NOTHING`, tr.id); err != nil {
		return err
	}
	if _, err := tx.ExecContext(ctx, `DELETE FROM nodes WHERE id=$1`, tr.id); err != nil {
		return err
	}
	if err := tx.Commit(); err != nil {
		return err
	}
	go s.GC(context.Background())
	return nil
}

func (s *Service) EmptyTrash(ctx context.Context, uid int64) error {
	items, err := s.ListTrash(ctx, uid)
	if err != nil {
		return err
	}
	for _, it := range items {
		if err := s.Purge(ctx, uid, it.ID); err != nil && !errors.Is(err, ErrNotFound) {
			return err
		}
	}
	return nil
}

// PurgeExpired чистит корзины всех пользователей от объектов старше days дней.
func (s *Service) PurgeExpired(ctx context.Context, days int) {
	if days <= 0 {
		return
	}
	rows, err := s.DB.QueryContext(ctx, `SELECT id, owner_id FROM nodes WHERE trash_root AND deleted_at < now() - make_interval(days => $1)`, days)
	if err != nil {
		log.Println("trash cleanup:", err)
		return
	}
	type item struct {
		id  string
		uid int64
	}
	var items []item
	for rows.Next() {
		var it item
		if rows.Scan(&it.id, &it.uid) == nil {
			items = append(items, it)
		}
	}
	rows.Close()
	for _, it := range items {
		if err := s.Purge(ctx, it.uid, it.id); err != nil && !errors.Is(err, ErrNotFound) {
			log.Println("trash cleanup:", err)
		}
	}
}

// ---------- версии ----------

func (s *Service) Versions(ctx context.Context, uid int64, id string) ([]Version, error) {
	n, err := s.Get(ctx, uid, id)
	if err != nil {
		return nil, err
	}
	rows, err := s.DB.QueryContext(ctx, `SELECT id, node_id, blob_key, size, sha256, created_at FROM versions
		WHERE node_id=$1 ORDER BY created_at DESC`, n.ID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := []Version{}
	for rows.Next() {
		var v Version
		if err := rows.Scan(&v.ID, &v.NodeID, &v.BlobKey, &v.Size, &v.SHA256, &v.CreatedAt); err != nil {
			return nil, err
		}
		v.Current = n.VersionID != nil && *n.VersionID == v.ID
		out = append(out, v)
	}
	return out, rows.Err()
}

// RestoreVersion делает старую версию текущей (создаётся новая версия с тем же содержимым).
func (s *Service) RestoreVersion(ctx context.Context, uid int64, id, vid string) (*Node, error) {
	if !ValidID(vid) {
		return nil, ErrNotFound
	}
	tx, err := s.DB.BeginTx(ctx, nil)
	if err != nil {
		return nil, err
	}
	defer tx.Rollback()
	n, err := s.get(ctx, tx, uid, id)
	if err != nil {
		return nil, err
	}
	var size int64
	if err := tx.QueryRowContext(ctx, `SELECT size FROM versions WHERE id=$1 AND node_id=$2`, vid, n.ID).Scan(&size); err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			return nil, ErrNotFound
		}
		return nil, err
	}
	nv := NewID()
	if _, err := tx.ExecContext(ctx, `INSERT INTO versions (id, node_id, owner_id, blob_key, size, sha256)
		SELECT $1, node_id, owner_id, blob_key, size, sha256 FROM versions WHERE id=$2`, nv, vid); err != nil {
		return nil, err
	}
	if _, err := tx.ExecContext(ctx, `UPDATE nodes SET current_version_id=$1, size=$2, updated_at=now() WHERE id=$3`, nv, size, n.ID); err != nil {
		return nil, err
	}
	if err := s.pruneVersions(ctx, tx, n.ID); err != nil {
		return nil, err
	}
	res, err := s.get(ctx, tx, uid, n.ID)
	if err != nil {
		return nil, err
	}
	return res, tx.Commit()
}

// ---------- сборка мусора ----------

func (s *Service) queueGC(ctx context.Context, q querier, keys ...string) error {
	for _, k := range keys {
		if _, err := q.ExecContext(ctx, `INSERT INTO blob_gc (blob_key) VALUES ($1) ON CONFLICT DO NOTHING`, k); err != nil {
			return err
		}
	}
	return nil
}

// GC удаляет из хранилища блобы, на которые больше не ссылается ни одна версия.
func (s *Service) GC(ctx context.Context) {
	for {
		rows, err := s.DB.QueryContext(ctx, `SELECT blob_key FROM blob_gc ORDER BY queued_at LIMIT 200`)
		if err != nil {
			log.Println("gc:", err)
			return
		}
		var keys []string
		for rows.Next() {
			var k string
			if rows.Scan(&k) == nil {
				keys = append(keys, k)
			}
		}
		rows.Close()
		if len(keys) == 0 {
			return
		}
		failed := false
		for _, k := range keys {
			var used bool
			if err := s.DB.QueryRowContext(ctx, `SELECT EXISTS(SELECT 1 FROM versions WHERE blob_key=$1)`, k).Scan(&used); err != nil {
				log.Println("gc:", err)
				return
			}
			if !used {
				if err := s.St.Delete(ctx, k); err != nil {
					log.Println("gc delete:", err)
					failed = true
					continue // попробуем при следующем запуске
				}
			}
			s.DB.ExecContext(ctx, `DELETE FROM blob_gc WHERE blob_key=$1`, k)
		}
		if failed || len(keys) < 200 {
			return
		}
	}
}
