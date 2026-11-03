package files

import (
	"context"
	"database/sql"
	"errors"
	"regexp"
	"strings"
	"time"

	"github.com/lib/pq"
)

// Decorate заполняет у узлов поля, зависящие от смотрящего: избранное, теги, наличие шаринга.
func (s *Service) Decorate(ctx context.Context, viewer int64, nodes []Node) error {
	if len(nodes) == 0 {
		return nil
	}
	ids := make([]string, len(nodes))
	pos := make(map[string]int, len(nodes))
	for i, n := range nodes {
		ids[i] = n.ID
		pos[n.ID] = i
	}
	rows, err := s.DB.QueryContext(ctx, `SELECT node_id FROM favorites WHERE user_id=$1 AND node_id = ANY($2::uuid[])`, viewer, pq.Array(ids))
	if err != nil {
		return err
	}
	for rows.Next() {
		var id string
		if rows.Scan(&id) == nil {
			nodes[pos[id]].Favorite = true
		}
	}
	rows.Close()

	rows, err = s.DB.QueryContext(ctx, `SELECT nt.node_id, t.id, t.name, t.color FROM node_tags nt JOIN tags t ON t.id=nt.tag_id
		WHERE t.user_id=$1 AND nt.node_id = ANY($2::uuid[]) ORDER BY lower(t.name)`, viewer, pq.Array(ids))
	if err != nil {
		return err
	}
	for rows.Next() {
		var id string
		var t Tag
		if rows.Scan(&id, &t.ID, &t.Name, &t.Color) == nil {
			nodes[pos[id]].Tags = append(nodes[pos[id]].Tags, t)
		}
	}
	rows.Close()

	rows, err = s.DB.QueryContext(ctx, `SELECT node_id FROM shares WHERE node_id = ANY($1::uuid[])
		UNION SELECT node_id FROM user_shares WHERE node_id = ANY($1::uuid[])`, pq.Array(ids))
	if err != nil {
		return err
	}
	for rows.Next() {
		var id string
		if rows.Scan(&id) == nil {
			nodes[pos[id]].Shared = true
		}
	}
	rows.Close()
	return nil
}

// ---------- избранное ----------

func (s *Service) SetFavorite(ctx context.Context, uid int64, nodeID string, on bool) error {
	if on {
		_, err := s.DB.ExecContext(ctx, `INSERT INTO favorites (user_id, node_id) VALUES ($1,$2) ON CONFLICT DO NOTHING`, uid, nodeID)
		return err
	}
	_, err := s.DB.ExecContext(ctx, `DELETE FROM favorites WHERE user_id=$1 AND node_id=$2`, uid, nodeID)
	return err
}

// Favorites — избранное пользователя (только то, к чему у него ещё есть доступ, проверяется в API).
func (s *Service) Favorites(ctx context.Context, uid int64) ([]Node, error) {
	rows, err := s.DB.QueryContext(ctx, `SELECT `+nodeColsP("n")+` FROM favorites f JOIN nodes n ON n.id=f.node_id
		WHERE f.user_id=$1 AND n.deleted_at IS NULL ORDER BY n.is_dir DESC, lower(n.name)`, uid)
	if err != nil {
		return nil, err
	}
	return collect(rows)
}

// ---------- теги ----------

var colorRe = regexp.MustCompile(`^#[0-9a-fA-F]{6}$`)

func (s *Service) ListTags(ctx context.Context, uid int64) ([]Tag, error) {
	rows, err := s.DB.QueryContext(ctx, `SELECT t.id, t.name, t.color, count(n.id) FROM tags t
		LEFT JOIN node_tags nt ON nt.tag_id=t.id LEFT JOIN nodes n ON n.id=nt.node_id AND n.deleted_at IS NULL
		WHERE t.user_id=$1 GROUP BY t.id ORDER BY lower(t.name)`, uid)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := []Tag{}
	for rows.Next() {
		var t Tag
		if err := rows.Scan(&t.ID, &t.Name, &t.Color, &t.Count); err != nil {
			return nil, err
		}
		out = append(out, t)
	}
	return out, rows.Err()
}

func (s *Service) CreateTag(ctx context.Context, uid int64, name, color string) (*Tag, error) {
	name = strings.TrimSpace(name)
	if name == "" || len(name) > 40 {
		return nil, ErrInvalidName
	}
	if !colorRe.MatchString(color) {
		color = "#2f6fed"
	}
	t := Tag{Name: name, Color: color}
	err := s.DB.QueryRowContext(ctx, `INSERT INTO tags (user_id, name, color) VALUES ($1,$2,$3) RETURNING id`, uid, name, color).Scan(&t.ID)
	if isUniqueViolation(err) {
		return nil, ErrTagExists
	}
	return &t, err
}

func (s *Service) DeleteTag(ctx context.Context, uid, id int64) error {
	res, err := s.DB.ExecContext(ctx, `DELETE FROM tags WHERE id=$1 AND user_id=$2`, id, uid)
	if err != nil {
		return err
	}
	if n, _ := res.RowsAffected(); n == 0 {
		return ErrNotFound
	}
	return nil
}

// SetNodeTags заменяет набор тегов пользователя uid на узле.
func (s *Service) SetNodeTags(ctx context.Context, uid int64, nodeID string, tagIDs []int64) error {
	tx, err := s.DB.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer tx.Rollback()
	if _, err := tx.ExecContext(ctx, `DELETE FROM node_tags WHERE node_id=$1 AND tag_id IN (SELECT id FROM tags WHERE user_id=$2)`, nodeID, uid); err != nil {
		return err
	}
	for _, t := range tagIDs {
		if _, err := tx.ExecContext(ctx, `INSERT INTO node_tags (node_id, tag_id) SELECT $1, id FROM tags WHERE id=$2 AND user_id=$3
			ON CONFLICT DO NOTHING`, nodeID, t, uid); err != nil {
			return err
		}
	}
	return tx.Commit()
}

func (s *Service) TaggedNodes(ctx context.Context, uid, tagID int64) ([]Node, error) {
	rows, err := s.DB.QueryContext(ctx, `SELECT `+nodeColsP("n")+` FROM node_tags nt JOIN tags t ON t.id=nt.tag_id
		JOIN nodes n ON n.id=nt.node_id WHERE t.id=$1 AND t.user_id=$2 AND n.deleted_at IS NULL
		ORDER BY n.is_dir DESC, lower(n.name)`, tagID, uid)
	if err != nil {
		return nil, err
	}
	return collect(rows)
}

// ---------- комментарии ----------

type Comment struct {
	ID        int64     `json:"id"`
	NodeID    string    `json:"node_id"`
	UserID    int64     `json:"user_id"`
	Username  string    `json:"username"`
	Body      string    `json:"body"`
	CreatedAt time.Time `json:"created_at"`
}

func (s *Service) Comments(ctx context.Context, nodeID string) ([]Comment, error) {
	rows, err := s.DB.QueryContext(ctx, `SELECT c.id, c.node_id, c.user_id, u.username, c.body, c.created_at
		FROM comments c JOIN users u ON u.id=c.user_id WHERE c.node_id=$1 ORDER BY c.created_at`, nodeID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := []Comment{}
	for rows.Next() {
		var c Comment
		if err := rows.Scan(&c.ID, &c.NodeID, &c.UserID, &c.Username, &c.Body, &c.CreatedAt); err != nil {
			return nil, err
		}
		out = append(out, c)
	}
	return out, rows.Err()
}

func (s *Service) AddComment(ctx context.Context, uid int64, nodeID, body string) (*Comment, error) {
	body = strings.TrimSpace(body)
	if body == "" || len(body) > 10000 {
		return nil, errors.New("комментарий пустой или слишком длинный")
	}
	var c Comment
	err := s.DB.QueryRowContext(ctx, `WITH ins AS (INSERT INTO comments (node_id, user_id, body) VALUES ($1,$2,$3)
		RETURNING id, node_id, user_id, body, created_at)
		SELECT ins.id, ins.node_id, ins.user_id, u.username, ins.body, ins.created_at FROM ins JOIN users u ON u.id=ins.user_id`,
		nodeID, uid, body).Scan(&c.ID, &c.NodeID, &c.UserID, &c.Username, &c.Body, &c.CreatedAt)
	return &c, err
}

func (s *Service) GetComment(ctx context.Context, id int64) (*Comment, error) {
	var c Comment
	err := s.DB.QueryRowContext(ctx, `SELECT c.id, c.node_id, c.user_id, u.username, c.body, c.created_at
		FROM comments c JOIN users u ON u.id=c.user_id WHERE c.id=$1`, id).Scan(&c.ID, &c.NodeID, &c.UserID, &c.Username, &c.Body, &c.CreatedAt)
	if errors.Is(err, sql.ErrNoRows) {
		return nil, ErrNotFound
	}
	return &c, err
}

func (s *Service) DeleteComment(ctx context.Context, id int64) error {
	_, err := s.DB.ExecContext(ctx, `DELETE FROM comments WHERE id=$1`, id)
	return err
}

// ---------- галерея ----------

// Gallery — фото и видео пользователя по дате съёмки (или загрузки), новые сверху.
func (s *Service) Gallery(ctx context.Context, uid int64, before *time.Time, limit int) ([]Node, error) {
	if limit <= 0 || limit > 500 {
		limit = 200
	}
	if before == nil {
		t := time.Now().Add(100 * 365 * 24 * time.Hour)
		before = &t
	}
	rows, err := s.DB.QueryContext(ctx, `SELECT `+nodeCols+` FROM nodes
		WHERE owner_id=$1 AND deleted_at IS NULL AND NOT is_dir AND (mime LIKE 'image/%' OR mime LIKE 'video/%')
		  AND COALESCE(taken_at, created_at) < $2
		ORDER BY COALESCE(taken_at, created_at) DESC LIMIT $3`, uid, *before, limit)
	if err != nil {
		return nil, err
	}
	return collect(rows)
}

// BackfillMedia дозаполняет дату съёмки для JPEG, загруженных до появления этой функции.
func (s *Service) BackfillMedia(ctx context.Context) {
	for ctx.Err() == nil {
		rows, err := s.DB.QueryContext(ctx, `SELECT id, owner_id, mime FROM nodes WHERE NOT media_checked AND NOT is_dir AND deleted_at IS NULL LIMIT 50`)
		if err != nil {
			return
		}
		type it struct {
			id    string
			owner int64
			mime  string
		}
		var items []it
		for rows.Next() {
			var x it
			if rows.Scan(&x.id, &x.owner, &x.mime) == nil {
				items = append(items, x)
			}
		}
		rows.Close()
		if len(items) == 0 {
			return
		}
		for _, x := range items {
			var taken *time.Time
			if x.mime == "image/jpeg" {
				if b, _, err := s.Open(ctx, x.owner, x.id, ""); err == nil {
					taken = exifTaken(b, s.Loc)
					b.Close()
				}
			}
			s.DB.ExecContext(ctx, `UPDATE nodes SET media_checked=TRUE, taken_at=COALESCE($2, taken_at) WHERE id=$1`, x.id, taken)
		}
	}
}
