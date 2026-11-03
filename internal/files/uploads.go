package files

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"io"
	"log"
	"os"
	"path/filepath"
	"time"
)

// Возобновляемая загрузка: файл приходит частями, сервер запоминает, сколько получено.
// Обрыв связи не страшен — клиент спрашивает смещение и продолжает с него.

var ErrOffset = errors.New("неверное смещение — запросите текущее и продолжите с него")

const ChunkSize = 8 << 20 // рекомендуемый размер части

type Upload struct {
	ID        string    `json:"id"`
	OwnerID   int64     `json:"-"`
	ActorID   int64     `json:"-"`
	ParentID  *string   `json:"parent_id"`
	Name      string    `json:"name"`
	RelPath   string    `json:"path"`
	Conflict  string    `json:"conflict"`
	Size      int64     `json:"size"`
	Received  int64     `json:"received"`
	ChunkSize int       `json:"chunk_size"`
	CreatedAt time.Time `json:"created_at"`
}

func (s *Service) uploadPath(id string) string { return filepath.Join(s.TmpDir, "uploads", id) }

func (s *Service) CreateUpload(ctx context.Context, owner, actor int64, parent *string, name, rel, conflict string, size int64) (*Upload, error) {
	if err := ValidateName(name); err != nil {
		return nil, err
	}
	if size < 0 {
		return nil, errors.New("неверный размер")
	}
	if s.MaxUpload > 0 && size > s.MaxUpload {
		return nil, ErrTooLarge
	}
	if err := s.checkDir(ctx, s.DB, owner, parent); err != nil {
		return nil, err
	}
	if err := s.checkQuota(ctx, owner, size); err != nil {
		return nil, err
	}
	if err := os.MkdirAll(filepath.Join(s.TmpDir, "uploads"), 0o750); err != nil {
		return nil, err
	}
	u := &Upload{ID: NewID(), OwnerID: owner, ActorID: actor, ParentID: parent, Name: name, RelPath: rel, Conflict: conflict, Size: size, ChunkSize: ChunkSize}
	f, err := os.Create(s.uploadPath(u.ID))
	if err != nil {
		return nil, err
	}
	f.Close()
	err = s.DB.QueryRowContext(ctx, `INSERT INTO uploads (id, owner_id, actor_id, parent_id, name, rel_path, conflict, size)
		VALUES ($1,$2,$3,$4,$5,$6,$7,$8) RETURNING created_at`, u.ID, owner, actor, parent, name, rel, conflict, size).Scan(&u.CreatedAt)
	if err != nil {
		os.Remove(s.uploadPath(u.ID))
		return nil, err
	}
	return u, nil
}

func (s *Service) GetUpload(ctx context.Context, actor int64, id string) (*Upload, error) {
	if !ValidID(id) {
		return nil, ErrNotFound
	}
	var u Upload
	var parent sql.NullString
	err := s.DB.QueryRowContext(ctx, `SELECT id, owner_id, actor_id, parent_id, name, rel_path, conflict, size, received, created_at
		FROM uploads WHERE id=$1 AND actor_id=$2`, id, actor).
		Scan(&u.ID, &u.OwnerID, &u.ActorID, &parent, &u.Name, &u.RelPath, &u.Conflict, &u.Size, &u.Received, &u.CreatedAt)
	if errors.Is(err, sql.ErrNoRows) {
		return nil, ErrNotFound
	}
	if err != nil {
		return nil, err
	}
	if parent.Valid {
		u.ParentID = &parent.String
	}
	u.ChunkSize = ChunkSize
	return &u, nil
}

// AppendUpload дописывает часть по смещению offset. Повтор уже принятой части безопасен.
func (s *Service) AppendUpload(ctx context.Context, actor int64, id string, offset int64, r io.Reader) (*Upload, error) {
	u, err := s.GetUpload(ctx, actor, id)
	if err != nil {
		return nil, err
	}
	if offset != u.Received {
		return u, ErrOffset
	}
	f, err := os.OpenFile(s.uploadPath(id), os.O_WRONLY, 0)
	if err != nil {
		return nil, err
	}
	defer f.Close()
	if _, err := f.Seek(offset, io.SeekStart); err != nil {
		return nil, err
	}
	n, err := io.Copy(f, io.LimitReader(r, u.Size-offset+1))
	if offset+n > u.Size {
		f.Truncate(offset)
		return u, fmt.Errorf("данных больше, чем заявлено (%d байт)", u.Size)
	}
	if err != nil {
		// Сохраняем то, что успело прийти: клиент продолжит с нового смещения.
		f.Sync()
	}
	newRecv := offset + n
	if _, uerr := s.DB.ExecContext(ctx, `UPDATE uploads SET received=$1, updated_at=now() WHERE id=$2 AND received=$3`, newRecv, id, offset); uerr != nil {
		return nil, uerr
	}
	u.Received = newRecv
	return u, err
}

// CompleteUpload превращает собранный файл в версию. Права проверяются вызывающим повторно.
func (s *Service) CompleteUpload(ctx context.Context, actor int64, id string) (*Node, *Upload, error) {
	u, err := s.GetUpload(ctx, actor, id)
	if err != nil {
		return nil, nil, err
	}
	if u.Received != u.Size {
		return nil, u, fmt.Errorf("получено %d из %d байт", u.Received, u.Size)
	}
	parent := u.ParentID
	if u.RelPath != "" {
		if parent, err = s.EnsureDirs(ctx, u.OwnerID, parent, u.RelPath); err != nil {
			return nil, u, err
		}
	}
	name := u.Name
	if u.Conflict == "rename" {
		if name, err = s.FreeName(ctx, u.OwnerID, parent, name); err != nil {
			return nil, u, err
		}
	}
	f, err := os.Open(s.uploadPath(id))
	if err != nil {
		return nil, u, err
	}
	n, err := s.WriteFile(ctx, u.OwnerID, parent, name, f)
	f.Close()
	if err != nil {
		return nil, u, err
	}
	s.DeleteUpload(ctx, actor, id)
	return n, u, nil
}

func (s *Service) DeleteUpload(ctx context.Context, actor int64, id string) error {
	if !ValidID(id) {
		return ErrNotFound
	}
	res, err := s.DB.ExecContext(ctx, `DELETE FROM uploads WHERE id=$1 AND actor_id=$2`, id, actor)
	if err != nil {
		return err
	}
	os.Remove(s.uploadPath(id))
	if n, _ := res.RowsAffected(); n == 0 {
		return ErrNotFound
	}
	return nil
}

// CleanupUploads удаляет брошенные загрузки старше 48 часов.
func (s *Service) CleanupUploads(ctx context.Context) {
	rows, err := s.DB.QueryContext(ctx, `DELETE FROM uploads WHERE updated_at < now() - interval '48 hours' RETURNING id`)
	if err != nil {
		log.Println("uploads cleanup:", err)
		return
	}
	defer rows.Close()
	for rows.Next() {
		var id string
		if rows.Scan(&id) == nil {
			os.Remove(s.uploadPath(id))
		}
	}
}
