// Package storage хранит содержимое файлов (блобы) на диске или в S3.
// Метаданные (имена, папки, версии) живут в PostgreSQL, здесь — только байты.
package storage

import (
	"context"
	"errors"
	"io"
)

var ErrNotFound = errors.New("blob not found")

// Blob — открытый на чтение блоб с поддержкой Seek (нужно для Range-запросов и видео).
type Blob interface {
	io.ReadSeekCloser
	Size() int64
}

type Storage interface {
	// Put сохраняет блоб. r читается до конца, size — точный размер.
	Put(ctx context.Context, key string, r io.Reader, size int64) error
	Get(ctx context.Context, key string) (Blob, error)
	Delete(ctx context.Context, key string) error
}
