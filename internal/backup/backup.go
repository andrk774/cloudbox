// Package backup — резервное копирование и восстановление (БД + файлы одним архивом),
// а также перенос блобов между хранилищами (local ↔ s3).
package backup

import (
	"archive/tar"
	"compress/gzip"
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log"
	"os"
	"os/exec"
	"path/filepath"
	"sort"
	"strings"
	"time"

	"github.com/andrk774/cloudbox/internal/storage"
)

type Manifest struct {
	Format    int       `json:"format"`
	CreatedAt time.Time `json:"created_at"`
	Blobs     int       `json:"blobs"`
	Bytes     int64     `json:"bytes"`
}

// blobKeys — все ключи версий файлов (миниатюры не сохраняем — они пересоздаются).
func blobKeys(ctx context.Context, db *sql.DB) ([]string, error) {
	rows, err := db.QueryContext(ctx, `SELECT DISTINCT blob_key FROM versions ORDER BY blob_key`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []string
	for rows.Next() {
		var k string
		if rows.Scan(&k) == nil {
			out = append(out, k)
		}
	}
	return out, rows.Err()
}

// Create пишет архив tar.gz: manifest.json, db.sql (pg_dump), blobs/<ключ>.
func Create(ctx context.Context, dbURL string, db *sql.DB, st storage.Storage, w io.Writer, tmpDir string) (*Manifest, error) {
	dump, err := os.CreateTemp(tmpDir, "pgdump-*.sql")
	if err != nil {
		return nil, err
	}
	defer os.Remove(dump.Name())
	cmd := exec.CommandContext(ctx, "pg_dump", "--no-owner", "--no-privileges",
		"--exclude-table-data=thumbs", "--exclude-table-data=uploads", "--exclude-table-data=index_queue", dbURL)
	cmd.Stdout = dump
	var stderr strings.Builder
	cmd.Stderr = &stderr
	if err := cmd.Run(); err != nil {
		dump.Close()
		return nil, fmt.Errorf("pg_dump: %v %s", err, stderr.String())
	}
	if _, err := dump.Seek(0, io.SeekStart); err != nil {
		return nil, err
	}
	keys, err := blobKeys(ctx, db)
	if err != nil {
		return nil, err
	}

	gz := gzip.NewWriter(w)
	tw := tar.NewWriter(gz)
	m := &Manifest{Format: 1, CreatedAt: time.Now().UTC(), Blobs: len(keys)}

	st0, _ := dump.Stat()
	if err := tw.WriteHeader(&tar.Header{Name: "db.sql", Mode: 0o600, Size: st0.Size(), ModTime: m.CreatedAt}); err != nil {
		return nil, err
	}
	if _, err := io.Copy(tw, dump); err != nil {
		return nil, err
	}
	dump.Close()

	for _, k := range keys {
		b, err := st.Get(ctx, k)
		if errors.Is(err, storage.ErrNotFound) {
			log.Printf("backup: блоб %s отсутствует в хранилище — пропущен", k)
			continue
		}
		if err != nil {
			return nil, err
		}
		if err := tw.WriteHeader(&tar.Header{Name: "blobs/" + k, Mode: 0o600, Size: b.Size(), ModTime: m.CreatedAt}); err != nil {
			b.Close()
			return nil, err
		}
		n, err := io.Copy(tw, b)
		b.Close()
		if err != nil {
			return nil, err
		}
		m.Bytes += n
	}
	mb, _ := json.MarshalIndent(m, "", "  ")
	if err := tw.WriteHeader(&tar.Header{Name: "manifest.json", Mode: 0o600, Size: int64(len(mb)), ModTime: m.CreatedAt}); err != nil {
		return nil, err
	}
	tw.Write(mb)
	if err := tw.Close(); err != nil {
		return nil, err
	}
	return m, gz.Close()
}

// Restore разворачивает архив в ПУСТУЮ базу и текущее хранилище.
// Блобы записываются через текущее хранилище — так можно сменить local на s3 или включить шифрование.
func Restore(ctx context.Context, dbURL string, db *sql.DB, st storage.Storage, r io.Reader, tmpDir string) (int, error) {
	var exists sql.NullString
	if err := db.QueryRowContext(ctx, `SELECT to_regclass('public.users')::text`).Scan(&exists); err != nil {
		return 0, err
	}
	if exists.Valid {
		return 0, errors.New("база данных не пустая — восстановление выполняется только в новую пустую базу")
	}
	gz, err := gzip.NewReader(r)
	if err != nil {
		return 0, err
	}
	tr := tar.NewReader(gz)
	blobs, dbDone := 0, false
	for {
		h, err := tr.Next()
		if err == io.EOF {
			break
		}
		if err != nil {
			return blobs, err
		}
		switch {
		case h.Name == "db.sql":
			f, err := os.CreateTemp(tmpDir, "restore-*.sql")
			if err != nil {
				return blobs, err
			}
			if _, err := io.Copy(f, tr); err != nil {
				f.Close()
				os.Remove(f.Name())
				return blobs, err
			}
			f.Close()
			cmd := exec.CommandContext(ctx, "psql", "-v", "ON_ERROR_STOP=1", "-q", "-f", f.Name(), dbURL)
			out, err := cmd.CombinedOutput()
			os.Remove(f.Name())
			if err != nil {
				return blobs, fmt.Errorf("psql: %v %s", err, out)
			}
			dbDone = true
		case strings.HasPrefix(h.Name, "blobs/"):
			key := strings.TrimPrefix(h.Name, "blobs/")
			if key == "" || strings.ContainsAny(key, "/\\.") {
				continue
			}
			if err := st.Put(ctx, key, tr, h.Size); err != nil {
				return blobs, fmt.Errorf("блоб %s: %w", key, err)
			}
			blobs++
		}
	}
	if !dbDone {
		return blobs, errors.New("в архиве нет db.sql")
	}
	return blobs, nil
}

// CopyBlobs переносит все блобы (версии и миниатюры) из src в dst. Уже существующие пропускаются.
func CopyBlobs(ctx context.Context, db *sql.DB, src, dst storage.Storage, progress func(done, total int)) error {
	rows, err := db.QueryContext(ctx, `SELECT blob_key FROM versions UNION SELECT blob_key FROM thumbs WHERE blob_key<>''`)
	if err != nil {
		return err
	}
	var keys []string
	for rows.Next() {
		var k string
		if rows.Scan(&k) == nil {
			keys = append(keys, k)
		}
	}
	rows.Close()
	for i, k := range keys {
		if b, err := dst.Get(ctx, k); err == nil {
			b.Close()
		} else {
			b, err := src.Get(ctx, k)
			if errors.Is(err, storage.ErrNotFound) {
				log.Printf("migrate: блоб %s не найден в исходном хранилище", k)
				continue
			}
			if err != nil {
				return err
			}
			err = dst.Put(ctx, k, b, b.Size())
			b.Close()
			if err != nil {
				return fmt.Errorf("блоб %s: %w", k, err)
			}
		}
		if progress != nil {
			progress(i+1, len(keys))
		}
	}
	return nil
}

// Scheduled — ежедневный бэкап в каталог dir в указанный час, хранится keep последних архивов.
func Scheduled(ctx context.Context, dbURL string, db *sql.DB, st storage.Storage, dir string, hour, keep int, tmpDir string) {
	if dir == "" {
		return
	}
	if err := os.MkdirAll(dir, 0o750); err != nil {
		log.Println("backup:", err)
		return
	}
	t := time.NewTicker(10 * time.Minute)
	defer t.Stop()
	for {
		now := time.Now()
		name := filepath.Join(dir, "cloudbox-"+now.Format("2006-01-02")+".tar.gz")
		if now.Hour() == hour {
			if _, err := os.Stat(name); errors.Is(err, os.ErrNotExist) {
				runOne(ctx, dbURL, db, st, name, tmpDir)
				prune(dir, keep)
			}
		}
		select {
		case <-ctx.Done():
			return
		case <-t.C:
		}
	}
}

func runOne(ctx context.Context, dbURL string, db *sql.DB, st storage.Storage, name, tmpDir string) {
	tmp := name + ".part"
	f, err := os.Create(tmp)
	if err != nil {
		log.Println("backup:", err)
		return
	}
	start := time.Now()
	m, err := Create(ctx, dbURL, db, st, f, tmpDir)
	if cerr := f.Close(); err == nil {
		err = cerr
	}
	if err != nil {
		os.Remove(tmp)
		log.Println("backup: ошибка:", err)
		return
	}
	if err := os.Rename(tmp, name); err != nil {
		log.Println("backup:", err)
		return
	}
	log.Printf("backup: %s — %d файлов, %d МБ, %s", name, m.Blobs, m.Bytes>>20, time.Since(start).Round(time.Second))
}

func prune(dir string, keep int) {
	if keep <= 0 {
		return
	}
	list, _ := filepath.Glob(filepath.Join(dir, "cloudbox-*.tar.gz"))
	sort.Strings(list)
	for len(list) > keep {
		os.Remove(list[0])
		list = list[1:]
	}
}
