// Package db открывает PostgreSQL и применяет миграции.
package db

import (
	"context"
	"database/sql"
	"embed"
	"fmt"
	"io/fs"
	"sort"
	"time"

	_ "github.com/lib/pq"
)

//go:embed migrations/*.sql
var migrations embed.FS

func Open(ctx context.Context, url string) (*sql.DB, error) {
	d, err := sql.Open("postgres", url)
	if err != nil {
		return nil, err
	}
	d.SetMaxOpenConns(25)
	d.SetMaxIdleConns(10)
	d.SetConnMaxLifetime(30 * time.Minute)

	// Ждём БД при старте контейнеров
	var lastErr error
	for i := 0; i < 30; i++ {
		if lastErr = d.PingContext(ctx); lastErr == nil {
			break
		}
		time.Sleep(time.Second)
	}
	if lastErr != nil {
		return nil, fmt.Errorf("postgres недоступен: %w", lastErr)
	}
	if err := migrate(ctx, d); err != nil {
		return nil, err
	}
	return d, nil
}

func migrate(ctx context.Context, d *sql.DB) error {
	if _, err := d.ExecContext(ctx, `CREATE TABLE IF NOT EXISTS schema_migrations (name TEXT PRIMARY KEY, applied_at TIMESTAMPTZ NOT NULL DEFAULT now())`); err != nil {
		return err
	}
	names, err := fs.Glob(migrations, "migrations/*.sql")
	if err != nil {
		return err
	}
	sort.Strings(names)
	for _, n := range names {
		var exists bool
		if err := d.QueryRowContext(ctx, `SELECT EXISTS(SELECT 1 FROM schema_migrations WHERE name=$1)`, n).Scan(&exists); err != nil {
			return err
		}
		if exists {
			continue
		}
		body, _ := migrations.ReadFile(n)
		tx, err := d.BeginTx(ctx, nil)
		if err != nil {
			return err
		}
		if _, err := tx.ExecContext(ctx, string(body)); err != nil {
			tx.Rollback()
			return fmt.Errorf("миграция %s: %w", n, err)
		}
		if _, err := tx.ExecContext(ctx, `INSERT INTO schema_migrations(name) VALUES ($1)`, n); err != nil {
			tx.Rollback()
			return err
		}
		if err := tx.Commit(); err != nil {
			return err
		}
	}
	return nil
}
