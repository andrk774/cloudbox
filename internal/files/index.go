package files

import (
	"context"
	"database/sql"
	"errors"
	"log"
	"strings"
	"time"

	"github.com/andrk774/cloudbox/internal/textext"
)

// ---------- полнотекстовый поиск ----------

func (s *Service) queueIndex(ctx context.Context, q querier, nodeID, name, mime string) error {
	if !textext.Supported(name, mime) {
		_, err := q.ExecContext(ctx, `DELETE FROM node_text WHERE node_id=$1`, nodeID)
		return err
	}
	_, err := q.ExecContext(ctx, `INSERT INTO index_queue (node_id) VALUES ($1)
		ON CONFLICT (node_id) DO UPDATE SET queued_at=now()`, nodeID)
	return err
}

func (s *Service) kickIndexer() {
	s.kickOnce.Do(func() { s.kick = make(chan struct{}, 1) })
	select {
	case s.kick <- struct{}{}:
	default:
	}
}

// RunIndexer — фоновый цикл извлечения текста. Блокирует до отмены ctx.
func (s *Service) RunIndexer(ctx context.Context) {
	s.kickOnce.Do(func() { s.kick = make(chan struct{}, 1) })
	t := time.NewTicker(30 * time.Second)
	defer t.Stop()
	for {
		for s.indexBatch(ctx) > 0 {
		}
		select {
		case <-ctx.Done():
			return
		case <-t.C:
		case <-s.kick:
		}
	}
}

func (s *Service) indexBatch(ctx context.Context) int {
	rows, err := s.DB.QueryContext(ctx, `SELECT node_id FROM index_queue ORDER BY queued_at LIMIT 20`)
	if err != nil {
		if ctx.Err() == nil {
			log.Println("indexer:", err)
		}
		return 0
	}
	var ids []string
	for rows.Next() {
		var id string
		if rows.Scan(&id) == nil {
			ids = append(ids, id)
		}
	}
	rows.Close()
	for _, id := range ids {
		if err := s.indexOne(ctx, id); err != nil && ctx.Err() == nil {
			log.Printf("indexer %s: %v", id, err)
		}
		s.DB.ExecContext(ctx, `DELETE FROM index_queue WHERE node_id=$1`, id)
	}
	return len(ids)
}

func (s *Service) indexOne(ctx context.Context, id string) error {
	var owner int64
	var name, mime string
	var vid sql.NullString
	err := s.DB.QueryRowContext(ctx, `SELECT owner_id, name, mime, current_version_id FROM nodes WHERE id=$1 AND deleted_at IS NULL`, id).
		Scan(&owner, &name, &mime, &vid)
	if errors.Is(err, sql.ErrNoRows) || !vid.Valid {
		return nil
	}
	if err != nil {
		return err
	}
	b, n, err := s.Open(ctx, owner, id, "")
	if err != nil {
		return err
	}
	defer b.Close()
	text, err := textext.Extract(ctx, name, mime, b, n.Size, s.TmpDir)
	if errors.Is(err, textext.ErrUnsupported) {
		return nil
	}
	if err != nil {
		return err
	}
	// В индекс добавляем и имя файла, чтобы фраза находилась вместе с названием.
	_, err = s.DB.ExecContext(ctx, `INSERT INTO node_text (node_id, version_id, content, tsv)
		VALUES ($1, $2, $3, setweight(to_tsvector('russian', $4), 'A') || to_tsvector('russian', $3) || to_tsvector('simple', $3))
		ON CONFLICT (node_id) DO UPDATE SET version_id=EXCLUDED.version_id, content=EXCLUDED.content, tsv=EXCLUDED.tsv`,
		id, vid.String, text, name)
	return err
}

// Search ищет по имени (подстрока) и по содержимому (морфология русского языка).
func (s *Service) Search(ctx context.Context, uid int64, q string) ([]Node, error) {
	q = strings.TrimSpace(q)
	if q == "" {
		return []Node{}, nil
	}
	like := strings.NewReplacer(`\`, `\\`, `%`, `\%`, `_`, `\_`).Replace(q)
	rows, err := s.DB.QueryContext(ctx, `
		WITH tq AS (SELECT websearch_to_tsquery('russian', $3) AS r, websearch_to_tsquery('simple', $3) AS s)
		SELECT `+nodeColsP("n")+`,
		       CASE WHEN t.tsv IS NOT NULL AND (t.tsv @@ tq.r OR t.tsv @@ tq.s) THEN (
		            SELECT CASE WHEN position('«' in h.ru) > 0 THEN h.ru ELSE h.si END FROM (SELECT
		              ts_headline('russian', left(t.content, 100000), tq.r, 'StartSel=«, StopSel=», MaxWords=18, MinWords=8, MaxFragments=1') AS ru,
		              ts_headline('simple', left(t.content, 100000), tq.r || tq.s, 'StartSel=«, StopSel=», MaxWords=18, MinWords=8, MaxFragments=1') AS si
		            ) h) ELSE '' END AS snippet,
		       (n.name ILIKE '%' || $2 || '%') AS name_hit,
		       COALESCE(ts_rank(t.tsv, tq.r), 0) AS rank
		FROM nodes n CROSS JOIN tq
		LEFT JOIN node_text t ON t.node_id = n.id
		WHERE n.owner_id=$1 AND n.deleted_at IS NULL
		  AND (n.name ILIKE '%' || $2 || '%' OR (t.tsv IS NOT NULL AND (t.tsv @@ tq.r OR t.tsv @@ tq.s)))
		ORDER BY name_hit DESC, n.is_dir DESC, rank DESC, n.updated_at DESC
		LIMIT 200`, uid, like, q)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := []Node{}
	for rows.Next() {
		var snippet string
		var nameHit bool
		var rank float64
		n, err := scanNode(scanFunc(func(dest ...any) error {
			return rows.Scan(append(dest, &snippet, &nameHit, &rank)...)
		}))
		if err != nil {
			return nil, err
		}
		n.Snippet = snippet
		out = append(out, *n)
	}
	return out, rows.Err()
}

// scanFunc позволяет дописать к сканированию узла дополнительные колонки.
type scanFunc func(dest ...any) error

func (f scanFunc) Scan(dest ...any) error { return f(dest...) }

// ReindexAll ставит в очередь все подходящие файлы (команда cloudbox reindex).
func (s *Service) ReindexAll(ctx context.Context) (int, error) {
	rows, err := s.DB.QueryContext(ctx, `SELECT id, name, mime FROM nodes WHERE deleted_at IS NULL AND NOT is_dir`)
	if err != nil {
		return 0, err
	}
	type it struct{ id, name, mime string }
	var items []it
	for rows.Next() {
		var x it
		if rows.Scan(&x.id, &x.name, &x.mime) == nil && textext.Supported(x.name, x.mime) {
			items = append(items, x)
		}
	}
	rows.Close()
	for _, x := range items {
		if err := s.queueIndex(ctx, s.DB, x.id, x.name, x.mime); err != nil {
			return 0, err
		}
	}
	return len(items), nil
}
