package files

import (
	"context"
	"database/sql"
	"errors"
	"time"
)

// Блокировки WOPI: пока документ открыт в онлайн-редакторе, другие клиенты не могут его перезаписать.

const LockTTL = 30 * time.Minute

func (s *Service) checkLock(ctx context.Context, q querier, nodeID, lockID string) error {
	var cur string
	err := q.QueryRowContext(ctx, `SELECT lock_id FROM wopi_locks WHERE node_id=$1 AND expires_at > now()`, nodeID).Scan(&cur)
	if errors.Is(err, sql.ErrNoRows) {
		return nil
	}
	if err != nil {
		return err
	}
	if cur != lockID {
		return ErrLocked
	}
	return nil
}

// GetLock — текущая блокировка ("" — нет).
func (s *Service) GetLock(ctx context.Context, nodeID string) string {
	var cur string
	s.DB.QueryRowContext(ctx, `SELECT lock_id FROM wopi_locks WHERE node_id=$1 AND expires_at > now()`, nodeID).Scan(&cur)
	return cur
}

// Lock ставит или продлевает блокировку. Возвращает текущую чужую блокировку при конфликте.
func (s *Service) Lock(ctx context.Context, nodeID, lockID, oldLock string) (string, error) {
	cur := s.GetLock(ctx, nodeID)
	if cur != "" && cur != lockID && cur != oldLock {
		return cur, ErrLocked
	}
	if oldLock != "" && cur != oldLock {
		return cur, ErrLocked
	}
	_, err := s.DB.ExecContext(ctx, `INSERT INTO wopi_locks (node_id, lock_id, expires_at) VALUES ($1,$2,$3)
		ON CONFLICT (node_id) DO UPDATE SET lock_id=EXCLUDED.lock_id, expires_at=EXCLUDED.expires_at`,
		nodeID, lockID, time.Now().Add(LockTTL))
	return "", err
}

func (s *Service) Unlock(ctx context.Context, nodeID, lockID string) (string, error) {
	cur := s.GetLock(ctx, nodeID)
	if cur != lockID {
		return cur, ErrLocked
	}
	_, err := s.DB.ExecContext(ctx, `DELETE FROM wopi_locks WHERE node_id=$1`, nodeID)
	return "", err
}

// ---------- дерево для клиента синхронизации ----------

type TreeEntry struct {
	ID        string    `json:"id"`
	Path      string    `json:"path"` // относительно корня синхронизации, через "/"
	IsDir     bool      `json:"is_dir"`
	Size      int64     `json:"size"`
	SHA256    string    `json:"sha256,omitempty"`
	VersionID string    `json:"version_id,omitempty"`
	Etag      string    `json:"etag"`
	E2E       bool      `json:"e2e,omitempty"` // шифротекст E2E-папки: клиенты синхронизации его пропускают
	UpdatedAt time.Time `json:"updated_at"`
}

// Tree — всё поддерево (root == nil — весь диск пользователя) одним запросом.
func (s *Service) Tree(ctx context.Context, owner int64, root *string) ([]TreeEntry, error) {
	rows, err := s.DB.QueryContext(ctx, `
		WITH RECURSIVE t AS (
			SELECT id, name::text AS path, is_dir, size, current_version_id, etag, updated_at, e2e
			FROM nodes WHERE owner_id=$1 AND parent_id IS NOT DISTINCT FROM $2 AND deleted_at IS NULL
			UNION ALL
			SELECT n.id, t.path || '/' || n.name, n.is_dir, n.size, n.current_version_id, n.etag, n.updated_at, n.e2e
			FROM nodes n JOIN t ON n.parent_id=t.id WHERE n.deleted_at IS NULL
		)
		SELECT t.id, t.path, t.is_dir, t.size, COALESCE(v.sha256,''), COALESCE(t.current_version_id::text,''), t.etag, t.updated_at, t.e2e
		FROM t LEFT JOIN versions v ON v.id=t.current_version_id ORDER BY t.path`, owner, root)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := []TreeEntry{}
	for rows.Next() {
		var e TreeEntry
		if err := rows.Scan(&e.ID, &e.Path, &e.IsDir, &e.Size, &e.SHA256, &e.VersionID, &e.Etag, &e.UpdatedAt, &e.E2E); err != nil {
			return nil, err
		}
		out = append(out, e)
	}
	return out, rows.Err()
}

// RootEtag — меняется при любом изменении в дереве пользователя.
func (s *Service) RootEtag(ctx context.Context, owner int64) string {
	var e string
	s.DB.QueryRowContext(ctx, `SELECT root_etag FROM users WHERE id=$1`, owner).Scan(&e)
	return e
}
