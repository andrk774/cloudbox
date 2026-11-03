package files

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
)

// Сквозное шифрование (E2E). Шифрует и расшифровывает только клиент (веб-интерфейс, WebCrypto).
// Сервер хранит:
//   - ключ хранилища пользователя, обёрнутый ключом из кодовой фразы и ключом восстановления;
//   - зашифрованные метаданные каждой E2E-папки (настоящие имена, ключи файлов);
//   - шифротекст файлов под случайными именами.
// Ни ключей, ни кодовой фразы, ни настоящих имён сервер не видит.

var (
	ErrE2ENoKeys    = errors.New("сначала создайте ключ шифрования (кодовую фразу)")
	ErrE2EKeysExist = errors.New("ключ шифрования уже создан")
	ErrE2EInUse     = errors.New("ключ нельзя удалить, пока есть зашифрованные папки: без него их не расшифровать")
	ErrE2EBadData   = errors.New("некорректные данные шифрования")
)

const (
	maxKeyData  = 16 << 10
	maxMetaData = 8 << 20
)

// E2EKeys — обёрнутый ключ пользователя (как его прислал клиент) и версия для безопасной замены.
type E2EKeys struct {
	Data    json.RawMessage `json:"data"`
	Version int             `json:"version"`
}

func (s *Service) E2EKeys(ctx context.Context, uid int64) (*E2EKeys, error) {
	var k E2EKeys
	var data []byte
	err := s.DB.QueryRowContext(ctx, `SELECT data, version FROM e2e_keys WHERE user_id=$1`, uid).Scan(&data, &k.Version)
	if errors.Is(err, sql.ErrNoRows) {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	k.Data = data
	return &k, nil
}

// PutE2EKeys создаёт (expect == 0) или заменяет ключ (смена кодовой фразы: expect — текущая версия).
func (s *Service) PutE2EKeys(ctx context.Context, uid int64, data json.RawMessage, expect int) (int, error) {
	var obj map[string]any
	if len(data) == 0 || len(data) > maxKeyData || json.Unmarshal(data, &obj) != nil || obj == nil {
		return 0, ErrE2EBadData
	}
	if expect == 0 {
		var v int
		err := s.DB.QueryRowContext(ctx, `INSERT INTO e2e_keys (user_id, data) VALUES ($1,$2)
			ON CONFLICT (user_id) DO NOTHING RETURNING version`, uid, []byte(data)).Scan(&v)
		if errors.Is(err, sql.ErrNoRows) {
			return 0, ErrE2EKeysExist
		}
		return v, err
	}
	var v int
	err := s.DB.QueryRowContext(ctx, `UPDATE e2e_keys SET data=$1, version=version+1, updated_at=now()
		WHERE user_id=$2 AND version=$3 RETURNING version`, []byte(data), uid, expect).Scan(&v)
	if errors.Is(err, sql.ErrNoRows) {
		return 0, ErrConflict
	}
	return v, err
}

// DeleteE2EKeys — только если зашифрованных папок нет (в том числе в корзине): иначе данные пропадут.
func (s *Service) DeleteE2EKeys(ctx context.Context, uid int64) error {
	var n int
	if err := s.DB.QueryRowContext(ctx, `SELECT count(*) FROM nodes WHERE owner_id=$1 AND e2e`, uid).Scan(&n); err != nil {
		return err
	}
	if n > 0 {
		return ErrE2EInUse
	}
	_, err := s.DB.ExecContext(ctx, `DELETE FROM e2e_keys WHERE user_id=$1`, uid)
	return err
}

// CreateE2EFolder создаёт E2E-папку в обычной папке. Её собственное имя видно (как у любой папки),
// всё внутри — шифротекст. Вложенные папки создаются обычным Mkdir: флаг e2e наследуется.
func (s *Service) CreateE2EFolder(ctx context.Context, uid int64, parent *string, name string) (*Node, error) {
	if k, err := s.E2EKeys(ctx, uid); err != nil {
		return nil, err
	} else if k == nil {
		return nil, ErrE2ENoKeys
	}
	if e, err := dirE2E(ctx, s.DB, parent); err != nil {
		return nil, err
	} else if e {
		return nil, errors.New("внутри зашифрованной папки создайте обычную вложенную папку — она тоже будет зашифрована")
	}
	n, err := s.Mkdir(ctx, uid, parent, name)
	if err != nil {
		return nil, err
	}
	if _, err := s.DB.ExecContext(ctx, `UPDATE nodes SET e2e=TRUE WHERE id=$1`, n.ID); err != nil {
		return nil, err
	}
	n.E2E = true
	return n, nil
}

// e2eDir — E2E-папка пользователя uid (сам владелец: делиться E2E-папками пока нельзя).
func (s *Service) e2eDir(ctx context.Context, uid int64, id string) (*Node, error) {
	n, err := s.Get(ctx, uid, id)
	if err != nil {
		return nil, err
	}
	if !n.IsDir || !n.E2E {
		return nil, ErrNotFound
	}
	return n, nil
}

// E2EMeta — зашифрованные метаданные папки; version 0 и пустые данные — метаданных ещё нет.
func (s *Service) E2EMeta(ctx context.Context, uid int64, id string) (string, int, error) {
	n, err := s.e2eDir(ctx, uid, id)
	if err != nil {
		return "", 0, err
	}
	var data string
	var v int
	err = s.DB.QueryRowContext(ctx, `SELECT data, version FROM e2e_meta WHERE node_id=$1`, n.ID).Scan(&data, &v)
	if errors.Is(err, sql.ErrNoRows) {
		return "", 0, nil
	}
	return data, v, err
}

// PutE2EMeta записывает метаданные, только если версия на сервере равна expect.
// Иначе ErrConflict: клиент перечитывает метаданные, применяет своё изменение заново и повторяет.
func (s *Service) PutE2EMeta(ctx context.Context, uid int64, id, data string, expect int) (int, error) {
	n, err := s.e2eDir(ctx, uid, id)
	if err != nil {
		return 0, err
	}
	if data == "" || len(data) > maxMetaData {
		return 0, ErrE2EBadData
	}
	var v int
	if expect == 0 {
		err = s.DB.QueryRowContext(ctx, `INSERT INTO e2e_meta (node_id, version, data) VALUES ($1, 1, $2)
			ON CONFLICT (node_id) DO NOTHING RETURNING version`, n.ID, data).Scan(&v)
	} else {
		err = s.DB.QueryRowContext(ctx, `UPDATE e2e_meta SET data=$1, version=version+1, updated_at=now()
			WHERE node_id=$2 AND version=$3 RETURNING version`, data, n.ID, expect).Scan(&v)
	}
	if errors.Is(err, sql.ErrNoRows) {
		return 0, ErrConflict
	}
	return v, err
}
