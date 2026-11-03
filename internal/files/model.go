// Package files — ядро: дерево папок, версии файлов, корзина, квоты, публичные ссылки.
package files

import (
	"crypto/rand"
	"database/sql"
	"encoding/hex"
	"errors"
	"fmt"
	"strings"
	"time"
	"unicode"
	"unicode/utf8"

	"github.com/lib/pq"
)

var (
	ErrNotFound    = errors.New("не найдено")
	ErrExists      = errors.New("объект с таким именем уже существует")
	ErrNotDir      = errors.New("это не папка")
	ErrIsDir       = errors.New("это папка")
	ErrQuota       = errors.New("превышена квота")
	ErrInvalidName = errors.New("недопустимое имя")
	ErrCycle       = errors.New("нельзя переместить папку внутрь самой себя")
	ErrTooLarge    = errors.New("файл слишком большой")
	ErrForbidden   = errors.New("недостаточно прав")
	ErrCrossOwner  = errors.New("нельзя переместить между хранилищами разных владельцев — используйте копирование")
	ErrConflict    = errors.New("файл изменён кем-то другим — обновите страницу")
	ErrLocked      = errors.New("файл заблокирован онлайн-редактором")
	ErrShared      = errors.New("доступ этому пользователю или группе уже выдан")
	ErrTagExists   = errors.New("такой тег уже есть")
)

type Node struct {
	ID        string     `json:"id"`
	OwnerID   int64      `json:"-"`
	ParentID  *string    `json:"parent_id"`
	Name      string     `json:"name"`
	IsDir     bool       `json:"is_dir"`
	Size      int64      `json:"size"`
	Mime      string     `json:"mime"`
	VersionID *string    `json:"version_id,omitempty"`
	CreatedAt time.Time  `json:"created_at"`
	UpdatedAt time.Time  `json:"updated_at"`
	DeletedAt *time.Time `json:"deleted_at,omitempty"`
	Etag      string     `json:"etag"`
	TakenAt   *time.Time `json:"taken_at,omitempty"`
	// Для корзины: исходное расположение
	OrigPath string `json:"orig_path,omitempty"`

	// Заполняются Decorate для текущего пользователя
	Favorite  bool   `json:"favorite,omitempty"`
	Tags      []Tag  `json:"tags,omitempty"`
	Shared    bool   `json:"shared,omitempty"`
	OwnerName string `json:"owner,omitempty"`
	CanWrite  *bool  `json:"can_write,omitempty"`
	Path      string `json:"path,omitempty"`
	Snippet   string `json:"snippet,omitempty"`
}

type Tag struct {
	ID    int64  `json:"id"`
	Name  string `json:"name"`
	Color string `json:"color"`
	Count int    `json:"count,omitempty"`
}

type Version struct {
	ID        string    `json:"id"`
	NodeID    string    `json:"node_id"`
	BlobKey   string    `json:"-"`
	Size      int64     `json:"size"`
	SHA256    string    `json:"sha256"`
	CreatedAt time.Time `json:"created_at"`
	Current   bool      `json:"current"`
}

const nodeCols = `id, owner_id, parent_id, name, is_dir, size, mime, current_version_id, created_at, updated_at, deleted_at, etag, taken_at`

// nodeColsP — те же колонки с префиксом таблицы (для JOIN и рекурсивных запросов).
func nodeColsP(p string) string {
	cols := strings.Split(nodeCols, ", ")
	for i, c := range cols {
		cols[i] = p + "." + c
	}
	return strings.Join(cols, ", ")
}

type scanner interface{ Scan(...any) error }

func scanNode(s scanner) (*Node, error) {
	var n Node
	var parent, ver sql.NullString
	var del, taken sql.NullTime
	if err := s.Scan(&n.ID, &n.OwnerID, &parent, &n.Name, &n.IsDir, &n.Size, &n.Mime, &ver, &n.CreatedAt, &n.UpdatedAt, &del, &n.Etag, &taken); err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			return nil, ErrNotFound
		}
		return nil, err
	}
	if parent.Valid {
		n.ParentID = &parent.String
	}
	if ver.Valid {
		n.VersionID = &ver.String
	}
	if del.Valid {
		n.DeletedAt = &del.Time
	}
	if taken.Valid {
		n.TakenAt = &taken.Time
	}
	return &n, nil
}

// NewID — UUIDv4.
func NewID() string {
	var b [16]byte
	_, _ = rand.Read(b[:])
	b[6] = (b[6] & 0x0f) | 0x40
	b[8] = (b[8] & 0x3f) | 0x80
	h := hex.EncodeToString(b[:])
	return fmt.Sprintf("%s-%s-%s-%s-%s", h[0:8], h[8:12], h[12:16], h[16:20], h[20:32])
}

// ValidID проверяет, что строка похожа на UUID (защита от мусора в запросах).
func ValidID(s string) bool {
	if len(s) != 36 {
		return false
	}
	for i, c := range s {
		switch i {
		case 8, 13, 18, 23:
			if c != '-' {
				return false
			}
		default:
			if !strings.ContainsRune("0123456789abcdefABCDEF", c) {
				return false
			}
		}
	}
	return true
}

func ValidateName(name string) error {
	if name == "" || name == "." || name == ".." || len(name) > 255 || !utf8.ValidString(name) {
		return ErrInvalidName
	}
	if strings.ContainsAny(name, `/\`) {
		return ErrInvalidName
	}
	for _, r := range name {
		if unicode.IsControl(r) {
			return ErrInvalidName
		}
	}
	if strings.TrimSpace(name) != name {
		return ErrInvalidName
	}
	return nil
}

// isUniqueViolation — ошибка уникального индекса PostgreSQL (код 23505).
func isUniqueViolation(err error) bool {
	var pe *pq.Error
	return errors.As(err, &pe) && pe.Code == "23505"
}
