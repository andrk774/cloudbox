package files

import (
	"context"
	"database/sql"
	"errors"
	"time"

	"github.com/andrk774/cloudbox/internal/secure"
)

type Share struct {
	ID           string     `json:"id"`
	Token        string     `json:"token"`
	NodeID       string     `json:"node_id"`
	OwnerID      int64      `json:"-"`
	PasswordHash string     `json:"-"`
	HasPassword  bool       `json:"has_password"`
	ExpiresAt    *time.Time `json:"expires_at"`
	AllowUpload  bool       `json:"allow_upload"`
	Downloads    int64      `json:"downloads"`
	CreatedAt    time.Time  `json:"created_at"`
	NodeName     string     `json:"node_name"`
	IsDir        bool       `json:"is_dir"`
}

const shareCols = `s.id, s.token, s.node_id, s.owner_id, COALESCE(s.password_hash,''), s.expires_at, s.allow_upload, s.downloads, s.created_at, n.name, n.is_dir`

func scanShare(sc scanner) (*Share, error) {
	var sh Share
	var exp sql.NullTime
	if err := sc.Scan(&sh.ID, &sh.Token, &sh.NodeID, &sh.OwnerID, &sh.PasswordHash, &exp, &sh.AllowUpload, &sh.Downloads, &sh.CreatedAt, &sh.NodeName, &sh.IsDir); err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			return nil, ErrNotFound
		}
		return nil, err
	}
	if exp.Valid {
		sh.ExpiresAt = &exp.Time
	}
	sh.HasPassword = sh.PasswordHash != ""
	return &sh, nil
}

func (s *Service) CreateShare(ctx context.Context, uid int64, nodeID, password string, expires *time.Time, allowUpload bool) (*Share, error) {
	n, err := s.Get(ctx, uid, nodeID)
	if err != nil {
		return nil, err
	}
	if n.E2E {
		return nil, ErrE2E // по ссылке гость получил бы шифротекст без ключа
	}
	var ph *string
	if password != "" {
		h, err := secure.HashPassword(password)
		if err != nil {
			return nil, err
		}
		ph = &h
	}
	if !n.IsDir {
		allowUpload = false
	}
	id := NewID()
	if _, err := s.DB.ExecContext(ctx, `INSERT INTO shares (id, token, node_id, owner_id, password_hash, expires_at, allow_upload)
		VALUES ($1,$2,$3,$4,$5,$6,$7)`, id, secure.Token(12), n.ID, uid, ph, expires, allowUpload); err != nil {
		return nil, err
	}
	return scanShare(s.DB.QueryRowContext(ctx, `SELECT `+shareCols+` FROM shares s JOIN nodes n ON n.id=s.node_id WHERE s.id=$1`, id))
}

// ListShares — ссылки пользователя (nodeID == "" — все).
func (s *Service) ListShares(ctx context.Context, uid int64, nodeID string) ([]Share, error) {
	q := `SELECT ` + shareCols + ` FROM shares s JOIN nodes n ON n.id=s.node_id
		WHERE s.owner_id=$1 AND n.deleted_at IS NULL`
	args := []any{uid}
	if nodeID != "" {
		if !ValidID(nodeID) {
			return []Share{}, nil
		}
		q += ` AND s.node_id=$2`
		args = append(args, nodeID)
	}
	rows, err := s.DB.QueryContext(ctx, q+` ORDER BY s.created_at DESC`, args...)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := []Share{}
	for rows.Next() {
		sh, err := scanShare(rows)
		if err != nil {
			return nil, err
		}
		out = append(out, *sh)
	}
	return out, rows.Err()
}

func (s *Service) DeleteShare(ctx context.Context, uid int64, id string) error {
	if !ValidID(id) {
		return ErrNotFound
	}
	res, err := s.DB.ExecContext(ctx, `DELETE FROM shares WHERE id=$1 AND owner_id=$2`, id, uid)
	if err != nil {
		return err
	}
	if n, _ := res.RowsAffected(); n == 0 {
		return ErrNotFound
	}
	return nil
}

// ShareByToken — действующая ссылка (не истекла, объект не в корзине).
func (s *Service) ShareByToken(ctx context.Context, token string) (*Share, error) {
	if len(token) < 8 || len(token) > 64 {
		return nil, ErrNotFound
	}
	sh, err := scanShare(s.DB.QueryRowContext(ctx, `SELECT `+shareCols+` FROM shares s JOIN nodes n ON n.id=s.node_id
		WHERE s.token=$1 AND n.deleted_at IS NULL`, token))
	if err != nil {
		return nil, err
	}
	if sh.ExpiresAt != nil && time.Now().After(*sh.ExpiresAt) {
		return nil, ErrNotFound
	}
	return sh, nil
}

func (s *Service) CountDownload(ctx context.Context, shareID string) {
	s.DB.ExecContext(ctx, `UPDATE shares SET downloads = downloads + 1 WHERE id=$1`, shareID)
}
