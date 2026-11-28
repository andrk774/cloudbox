package files

import (
	"context"
	"database/sql"
	"database/sql/driver"
	"errors"
	"os"
	"path/filepath"
	"testing"
)

// A zero-row DELETE is the real SQL result for a different actor's upload.
// Keep the disk operation real, while controlling database outcomes without Postgres.
type uploadDeleteDB struct {
	result driver.Result
	err    error
}

func (d uploadDeleteDB) Connect(context.Context) (driver.Conn, error) { return d, nil }
func (d uploadDeleteDB) Driver() driver.Driver                        { return d }
func (d uploadDeleteDB) Open(string) (driver.Conn, error)             { return d, nil }
func (uploadDeleteDB) Close() error                                   { return nil }
func (uploadDeleteDB) Prepare(string) (driver.Stmt, error) {
	return nil, errors.New("unexpected Prepare")
}
func (uploadDeleteDB) Begin() (driver.Tx, error) { return nil, errors.New("unexpected Begin") }
func (d uploadDeleteDB) ExecContext(context.Context, string, []driver.NamedValue) (driver.Result, error) {
	return d.result, d.err
}

type unknownUploadRows struct{}

func (unknownUploadRows) LastInsertId() (int64, error) { return 0, nil }
func (unknownUploadRows) RowsAffected() (int64, error) {
	return 0, errors.New("row count unavailable")
}

func TestDeleteUploadRequiresConfirmedOwnership(t *testing.T) {
	for _, tc := range []struct {
		name   string
		result driver.Result
		dbErr  error
		remove bool
	}{
		{"other actor or missing row", driver.RowsAffected(0), nil, false},
		{"database failure", nil, errors.New("database offline"), false},
		{"unknown affected rows", unknownUploadRows{}, nil, false},
		{"owner", driver.RowsAffected(1), nil, true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			db := sql.OpenDB(uploadDeleteDB{result: tc.result, err: tc.dbErr})
			defer db.Close()
			s := &Service{DB: db, TmpDir: t.TempDir()}
			id := NewID()
			p := s.uploadPath(id)
			if err := os.MkdirAll(filepath.Dir(p), 0o700); err != nil {
				t.Fatal(err)
			}
			if err := os.WriteFile(p, []byte("upload contents"), 0o600); err != nil {
				t.Fatal(err)
			}
			err := s.DeleteUpload(context.Background(), 999, id)
			if (err == nil) != tc.remove {
				t.Fatalf("DeleteUpload error = %v, remove = %v", err, tc.remove)
			}
			if tc.name == "other actor or missing row" && !errors.Is(err, ErrNotFound) {
				t.Fatalf("expected ErrNotFound, got %v", err)
			}
			data, err := os.ReadFile(p)
			if tc.remove {
				if !errors.Is(err, os.ErrNotExist) {
					t.Fatalf("owner's file should be removed: %v", err)
				}
			} else if err != nil || string(data) != "upload contents" {
				t.Fatalf("unconfirmed delete changed upload bytes: %q, %v", data, err)
			}
		})
	}
}
