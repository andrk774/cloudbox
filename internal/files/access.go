package files

import (
	"context"
	"database/sql"
	"errors"
	"strings"
	"time"
)

// Perm — права пользователя на узел.
type Perm struct {
	Owner     int64  // владелец узла: все операции выполняются от его имени
	Write     bool   // можно изменять
	ShareRoot string // если доступ через шару — id расшаренного узла
	ShareID   string
}

// OwnPerm — права на собственное дерево (в т.ч. корень).
func OwnPerm(uid int64) *Perm { return &Perm{Owner: uid, Write: true} }

// Access определяет, может ли uid работать с узлом id, и от чьего имени.
func (s *Service) Access(ctx context.Context, uid int64, id string) (*Perm, *Node, error) {
	if !ValidID(id) {
		return nil, nil, ErrNotFound
	}
	n, err := scanNode(s.DB.QueryRowContext(ctx, `SELECT `+nodeCols+` FROM nodes WHERE id=$1 AND deleted_at IS NULL`, id))
	if err != nil {
		return nil, nil, err
	}
	if n.OwnerID == uid {
		return OwnPerm(uid), n, nil
	}
	// Ищем ближайшего предка (или сам узел), расшаренного uid напрямую или через группу.
	var shareRoot, shareID string
	var canWrite bool
	err = s.DB.QueryRowContext(ctx, `
		WITH RECURSIVE a AS (
			SELECT id, parent_id, 0 AS depth FROM nodes WHERE id=$1
			UNION ALL SELECT n.id, n.parent_id, a.depth+1 FROM nodes n JOIN a ON n.id=a.parent_id
		)
		SELECT us.node_id, us.id, bool_or(us.can_write) OVER (PARTITION BY us.node_id)
		FROM a JOIN user_shares us ON us.node_id = a.id
		WHERE us.user_id = $2 OR us.group_id IN (SELECT group_id FROM group_members WHERE user_id = $2)
		ORDER BY a.depth, us.can_write DESC LIMIT 1`, id, uid).Scan(&shareRoot, &shareID, &canWrite)
	if errors.Is(err, sql.ErrNoRows) {
		return nil, nil, ErrNotFound
	}
	if err != nil {
		return nil, nil, err
	}
	// Права могут быть выданы на нескольких уровнях: берём максимум по всей цепочке.
	if !canWrite {
		s.DB.QueryRowContext(ctx, `
			WITH RECURSIVE a AS (
				SELECT id, parent_id FROM nodes WHERE id=$1
				UNION ALL SELECT n.id, n.parent_id FROM nodes n JOIN a ON n.id=a.parent_id
			) SELECT COALESCE(bool_or(us.can_write), false) FROM a JOIN user_shares us ON us.node_id=a.id
			WHERE us.user_id=$2 OR us.group_id IN (SELECT group_id FROM group_members WHERE user_id=$2)`, id, uid).Scan(&canWrite)
	}
	return &Perm{Owner: n.OwnerID, Write: canWrite, ShareRoot: shareRoot, ShareID: shareID}, n, nil
}

// ---------- группы ----------

type Group struct {
	ID        int64     `json:"id"`
	Name      string    `json:"name"`
	Members   []Member  `json:"members"`
	CreatedAt time.Time `json:"created_at"`
}

type Member struct {
	ID       int64  `json:"id"`
	Username string `json:"username"`
}

func (s *Service) ListGroups(ctx context.Context) ([]Group, error) {
	rows, err := s.DB.QueryContext(ctx, `SELECT g.id, g.name, g.created_at, u.id, u.username
		FROM groups g LEFT JOIN group_members m ON m.group_id=g.id LEFT JOIN users u ON u.id=m.user_id
		ORDER BY lower(g.name), lower(u.username)`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := []Group{}
	idx := map[int64]int{}
	for rows.Next() {
		var g Group
		var mid sql.NullInt64
		var mname sql.NullString
		if err := rows.Scan(&g.ID, &g.Name, &g.CreatedAt, &mid, &mname); err != nil {
			return nil, err
		}
		i, ok := idx[g.ID]
		if !ok {
			g.Members = []Member{}
			out = append(out, g)
			i = len(out) - 1
			idx[g.ID] = i
		}
		if mid.Valid {
			out[i].Members = append(out[i].Members, Member{mid.Int64, mname.String})
		}
	}
	return out, rows.Err()
}

func (s *Service) CreateGroup(ctx context.Context, name string) (*Group, error) {
	name = strings.TrimSpace(name)
	if name == "" || len(name) > 64 {
		return nil, ErrInvalidName
	}
	g := Group{Name: name, Members: []Member{}}
	err := s.DB.QueryRowContext(ctx, `INSERT INTO groups (name) VALUES ($1) RETURNING id, created_at`, name).Scan(&g.ID, &g.CreatedAt)
	if isUniqueViolation(err) {
		return nil, ErrExists
	}
	return &g, err
}

func (s *Service) DeleteGroup(ctx context.Context, id int64) error {
	res, err := s.DB.ExecContext(ctx, `DELETE FROM groups WHERE id=$1`, id)
	if err != nil {
		return err
	}
	if n, _ := res.RowsAffected(); n == 0 {
		return ErrNotFound
	}
	return nil
}

func (s *Service) SetGroupMembers(ctx context.Context, id int64, users []int64) error {
	tx, err := s.DB.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer tx.Rollback()
	var exists bool
	if err := tx.QueryRowContext(ctx, `SELECT EXISTS(SELECT 1 FROM groups WHERE id=$1)`, id).Scan(&exists); err != nil {
		return err
	}
	if !exists {
		return ErrNotFound
	}
	if _, err := tx.ExecContext(ctx, `DELETE FROM group_members WHERE group_id=$1`, id); err != nil {
		return err
	}
	for _, u := range users {
		if _, err := tx.ExecContext(ctx, `INSERT INTO group_members (group_id, user_id) SELECT $1, id FROM users WHERE id=$2 ON CONFLICT DO NOTHING`, id, u); err != nil {
			return err
		}
	}
	return tx.Commit()
}

// UserGroups — id групп пользователя.
func (s *Service) UserGroups(ctx context.Context, uid int64) ([]int64, error) {
	rows, err := s.DB.QueryContext(ctx, `SELECT group_id FROM group_members WHERE user_id=$1`, uid)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []int64
	for rows.Next() {
		var g int64
		if rows.Scan(&g) == nil {
			out = append(out, g)
		}
	}
	return out, rows.Err()
}

// ---------- шары пользователям ----------

type UserShare struct {
	ID         string    `json:"id"`
	NodeID     string    `json:"node_id"`
	OwnerID    int64     `json:"owner_id"`
	UserID     *int64    `json:"user_id,omitempty"`
	GroupID    *int64    `json:"group_id,omitempty"`
	TargetName string    `json:"target_name"`
	CanWrite   bool      `json:"can_write"`
	CreatedAt  time.Time `json:"created_at"`
}

func (s *Service) ListUserShares(ctx context.Context, nodeID string) ([]UserShare, error) {
	rows, err := s.DB.QueryContext(ctx, `SELECT s.id, s.node_id, s.owner_id, s.user_id, s.group_id,
		COALESCE(u.username, g.name, ''), s.can_write, s.created_at
		FROM user_shares s LEFT JOIN users u ON u.id=s.user_id LEFT JOIN groups g ON g.id=s.group_id
		WHERE s.node_id=$1 ORDER BY s.created_at`, nodeID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := []UserShare{}
	for rows.Next() {
		var us UserShare
		var u, g sql.NullInt64
		if err := rows.Scan(&us.ID, &us.NodeID, &us.OwnerID, &u, &g, &us.TargetName, &us.CanWrite, &us.CreatedAt); err != nil {
			return nil, err
		}
		if u.Valid {
			us.UserID = &u.Int64
		}
		if g.Valid {
			us.GroupID = &g.Int64
		}
		out = append(out, us)
	}
	return out, rows.Err()
}

// ShareWith выдаёт доступ к узлу владельца owner пользователю или группе.
func (s *Service) ShareWith(ctx context.Context, owner int64, nodeID string, userID, groupID *int64, canWrite bool) (*UserShare, error) {
	n, err := s.Get(ctx, owner, nodeID)
	if err != nil {
		return nil, err
	}
	if (userID == nil) == (groupID == nil) {
		return nil, errors.New("укажите пользователя или группу")
	}
	if userID != nil && *userID == owner {
		return nil, errors.New("нельзя поделиться с самим собой")
	}
	id := NewID()
	_, err = s.DB.ExecContext(ctx, `INSERT INTO user_shares (id, node_id, owner_id, user_id, group_id, can_write) VALUES ($1,$2,$3,$4,$5,$6)`,
		id, n.ID, owner, userID, groupID, canWrite)
	if isUniqueViolation(err) {
		return nil, ErrShared
	}
	if err != nil && strings.Contains(err.Error(), "foreign key") {
		return nil, ErrNotFound
	}
	if err != nil {
		return nil, err
	}
	list, err := s.ListUserShares(ctx, n.ID)
	if err != nil {
		return nil, err
	}
	for i := range list {
		if list[i].ID == id {
			return &list[i], nil
		}
	}
	return nil, ErrNotFound
}

func (s *Service) GetUserShare(ctx context.Context, id string) (*UserShare, error) {
	if !ValidID(id) {
		return nil, ErrNotFound
	}
	var nodeID string
	if err := s.DB.QueryRowContext(ctx, `SELECT node_id FROM user_shares WHERE id=$1`, id).Scan(&nodeID); err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			return nil, ErrNotFound
		}
		return nil, err
	}
	list, err := s.ListUserShares(ctx, nodeID)
	if err != nil {
		return nil, err
	}
	for i := range list {
		if list[i].ID == id {
			return &list[i], nil
		}
	}
	return nil, ErrNotFound
}

func (s *Service) UpdateUserShare(ctx context.Context, id string, canWrite bool) error {
	_, err := s.DB.ExecContext(ctx, `UPDATE user_shares SET can_write=$1 WHERE id=$2`, canWrite, id)
	return err
}

func (s *Service) DeleteUserShare(ctx context.Context, id string) error {
	_, err := s.DB.ExecContext(ctx, `DELETE FROM user_shares WHERE id=$1`, id)
	return err
}

// ShareTargets — пользователи, которые увидят расшаренный узел (для уведомлений).
func (s *Service) ShareTargets(ctx context.Context, sh *UserShare) []int64 {
	if sh.UserID != nil {
		return []int64{*sh.UserID}
	}
	rows, err := s.DB.QueryContext(ctx, `SELECT user_id FROM group_members WHERE group_id=$1 AND user_id<>$2`, *sh.GroupID, sh.OwnerID)
	if err != nil {
		return nil
	}
	defer rows.Close()
	var out []int64
	for rows.Next() {
		var u int64
		if rows.Scan(&u) == nil {
			out = append(out, u)
		}
	}
	return out
}

// SharedWithMe — корневые узлы, к которым uid получил доступ.
func (s *Service) SharedWithMe(ctx context.Context, uid int64) ([]Node, error) {
	rows, err := s.DB.QueryContext(ctx, `
		SELECT `+nodeColsP("n")+`, u.username, bool_or(us.can_write)
		FROM user_shares us JOIN nodes n ON n.id=us.node_id JOIN users u ON u.id=n.owner_id
		WHERE n.deleted_at IS NULL AND n.owner_id<>$1
		  AND (us.user_id=$1 OR us.group_id IN (SELECT group_id FROM group_members WHERE user_id=$1))
		GROUP BY `+nodeColsP("n")+`, u.username
		ORDER BY n.is_dir DESC, lower(n.name)`, uid)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := []Node{}
	for rows.Next() {
		var owner string
		var cw bool
		n, err := scanNode(scanFunc(func(d ...any) error { return rows.Scan(append(d, &owner, &cw)...) }))
		if err != nil {
			return nil, err
		}
		n.OwnerName, n.CanWrite = owner, &cw
		out = append(out, *n)
	}
	return out, rows.Err()
}

// Directory — пользователи и группы для выбора в диалоге «Поделиться».
type DirEntry struct {
	Kind string `json:"kind"` // user | group
	ID   int64  `json:"id"`
	Name string `json:"name"`
}

func (s *Service) Directory(ctx context.Context, exclude int64, q string) ([]DirEntry, error) {
	like := "%" + strings.NewReplacer(`\`, `\\`, `%`, `\%`, `_`, `\_`).Replace(strings.TrimSpace(q)) + "%"
	rows, err := s.DB.QueryContext(ctx, `
		(SELECT 'user', id, username FROM users WHERE id<>$1 AND username ILIKE $2 ORDER BY lower(username) LIMIT 20)
		UNION ALL
		(SELECT 'group', id, name FROM groups WHERE name ILIKE $2 ORDER BY lower(name) LIMIT 20)`, exclude, like)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := []DirEntry{}
	for rows.Next() {
		var e DirEntry
		if rows.Scan(&e.Kind, &e.ID, &e.Name) == nil {
			out = append(out, e)
		}
	}
	return out, rows.Err()
}

// TrimCrumbs обрезает «хлебные крошки» до расшаренного корня (чужое дерево выше показывать нельзя).
func TrimCrumbs(crumbs []Node, root string) []Node {
	for i, c := range crumbs {
		if c.ID == root {
			return crumbs[i:]
		}
	}
	return crumbs
}
