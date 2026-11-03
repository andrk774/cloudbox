package files

import (
	"bytes"
	"context"
	"database/sql"
	"errors"
	"io"
	"sync"
	"time"

	"github.com/andrk774/cloudbox/internal/media"
	"github.com/andrk774/cloudbox/internal/storage"
)

var ErrNoThumb = errors.New("миниатюры нет")

func exifTaken(r io.Reader, loc *time.Location) *time.Time {
	ex, err := media.ReadExif(r, loc)
	if err != nil {
		return nil
	}
	return ex.Taken
}

// thumbLocks не даёт генерировать одну и ту же миниатюру параллельно (лента галереи шлёт много запросов).
var thumbLocks sync.Map

// Thumbnail возвращает JPEG-миниатюру текущей версии файла, создавая и кэшируя её при первом запросе.
func (s *Service) Thumbnail(ctx context.Context, n *Node, size int) ([]byte, error) {
	if n.IsDir || n.VersionID == nil || !media.Sizes[size] || !media.CanThumb(n.Mime) {
		return nil, ErrNoThumb
	}
	vid := *n.VersionID
	if b, err := s.cachedThumb(ctx, vid, size); !errors.Is(err, sql.ErrNoRows) {
		return b, err
	}

	mu, _ := thumbLocks.LoadOrStore(vid, &sync.Mutex{})
	mu.(*sync.Mutex).Lock()
	defer func() { mu.(*sync.Mutex).Unlock(); thumbLocks.Delete(vid) }()
	if b, err := s.cachedThumb(ctx, vid, size); !errors.Is(err, sql.ErrNoRows) {
		return b, err
	}

	blob, _, err := s.Open(ctx, n.OwnerID, n.ID, vid)
	if err != nil {
		return nil, err
	}
	data, err := media.Thumbnail(ctx, blob, blob.Size(), n.Mime, size, s.TmpDir)
	blob.Close()
	if err != nil {
		if ctx.Err() != nil {
			return nil, ctx.Err()
		}
		// Запоминаем неудачу, чтобы не пытаться снова на каждом показе.
		s.DB.ExecContext(ctx, `INSERT INTO thumbs (version_id, size, blob_key) VALUES ($1,$2,'') ON CONFLICT DO NOTHING`, vid, size)
		return nil, ErrNoThumb
	}
	key := "th" + newBlobKey()
	if err := s.St.Put(ctx, key, bytes.NewReader(data), int64(len(data))); err != nil {
		return data, nil // отдадим без кэша
	}
	res, err := s.DB.ExecContext(ctx, `INSERT INTO thumbs (version_id, size, blob_key) VALUES ($1,$2,$3) ON CONFLICT DO NOTHING`, vid, size, key)
	if n, _ := res.RowsAffected(); err != nil || n == 0 {
		s.queueGC(ctx, s.DB, key)
	}
	return data, nil
}

func (s *Service) cachedThumb(ctx context.Context, vid string, size int) ([]byte, error) {
	var key string
	err := s.DB.QueryRowContext(ctx, `SELECT blob_key FROM thumbs WHERE version_id=$1 AND size=$2`, vid, size).Scan(&key)
	if err != nil {
		return nil, err
	}
	if key == "" {
		return nil, ErrNoThumb
	}
	b, err := s.St.Get(ctx, key)
	if errors.Is(err, storage.ErrNotFound) {
		// блоб пропал (например, после восстановления из бэкапа без миниатюр) — сгенерируем заново
		s.DB.ExecContext(ctx, `DELETE FROM thumbs WHERE version_id=$1 AND size=$2`, vid, size)
		return nil, sql.ErrNoRows
	}
	if err != nil {
		return nil, err
	}
	defer b.Close()
	return io.ReadAll(io.LimitReader(b, 8<<20))
}
