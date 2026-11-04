// Package talk — чат (личные переписки и групповые комнаты) и сигнализация видеозвонков WebRTC.
//
// Сообщения хранятся в PostgreSQL; доставка в реальном времени — через Hub (Server-Sent Events).
// Hub живёт в памяти процесса, поэтому CloudBox с чатом запускается в одном экземпляре.
package talk

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"strings"
	"time"
	"unicode/utf8"

	"github.com/andrk774/cloudbox/internal/files"
)

var (
	ErrNotFound  = errors.New("комната не найдена")
	ErrForbidden = errors.New("недостаточно прав")
	ErrEmpty     = errors.New("пустое сообщение")
	ErrTooLong   = errors.New("сообщение длиннее 4000 символов")
	ErrBadRoom   = errors.New("название комнаты: от 1 до 80 символов")
	ErrSelf      = errors.New("нельзя написать самому себе")
	ErrDirect    = errors.New("в личной переписке нельзя менять участников")
)

const (
	MaxBody     = 4000
	MaxMembers  = 100
	pageDefault = 50
	pageMax     = 200
)

type Service struct {
	DB  *sql.DB
	Hub *Hub
}

type Member struct {
	ID       int64  `json:"id"`
	Username string `json:"username"`
}

type Room struct {
	ID           string     `json:"id"`
	Kind         string     `json:"kind"`
	Name         string     `json:"name"` // для личной переписки — имя собеседника
	CreatedBy    *int64     `json:"created_by"`
	Members      []Member   `json:"members"`
	Unread       int        `json:"unread"`
	LastRead     int64      `json:"last_read"`
	LastActivity time.Time  `json:"last_activity"`
	Last         *Message   `json:"last,omitempty"`
	Call         *CallState `json:"call,omitempty"` // идущий звонок (заполняет API из Hub)
}

type Message struct {
	ID        int64     `json:"id"`
	RoomID    string    `json:"room_id"`
	UserID    *int64    `json:"user_id"`
	Username  string    `json:"username"`
	Kind      string    `json:"kind"`
	Body      string    `json:"body"`
	CreatedAt time.Time `json:"created_at"`
}

func directKey(a, b int64) string {
	if a > b {
		a, b = b, a
	}
	return fmt.Sprintf("%d:%d", a, b)
}

// ---------- комнаты ----------

// IsMember — состоит ли uid в комнате.
func (s *Service) IsMember(ctx context.Context, uid int64, room string) (bool, error) {
	if !files.ValidID(room) {
		return false, nil
	}
	var ok bool
	err := s.DB.QueryRowContext(ctx, `SELECT EXISTS(SELECT 1 FROM talk_members WHERE room_id=$1 AND user_id=$2)`, room, uid).Scan(&ok)
	return ok, err
}

func (s *Service) need(ctx context.Context, uid int64, room string) error {
	ok, err := s.IsMember(ctx, uid, room)
	if err != nil {
		return err
	}
	if !ok {
		return ErrNotFound // чужая комната неотличима от несуществующей
	}
	return nil
}

// MemberIDs — участники комнаты (для рассылки событий).
func (s *Service) MemberIDs(ctx context.Context, room string) ([]int64, error) {
	rows, err := s.DB.QueryContext(ctx, `SELECT user_id FROM talk_members WHERE room_id=$1`, room)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []int64
	for rows.Next() {
		var id int64
		if err := rows.Scan(&id); err != nil {
			return nil, err
		}
		out = append(out, id)
	}
	return out, rows.Err()
}

// Rooms — комнаты пользователя, сначала с последней активностью.
func (s *Service) Rooms(ctx context.Context, uid int64) ([]Room, error) {
	rows, err := s.DB.QueryContext(ctx, `
		SELECT r.id, r.kind, r.name, r.created_by, r.last_activity, m.last_read,
		       (SELECT count(*) FROM talk_messages x WHERE x.room_id=r.id AND x.id > m.last_read
		          AND x.user_id IS DISTINCT FROM $1 AND x.kind='text')
		FROM talk_rooms r JOIN talk_members m ON m.room_id=r.id AND m.user_id=$1
		ORDER BY r.last_activity DESC`, uid)
	if err != nil {
		return nil, err
	}
	out := []Room{}
	for rows.Next() {
		var r Room
		var by sql.NullInt64
		if err := rows.Scan(&r.ID, &r.Kind, &r.Name, &by, &r.LastActivity, &r.LastRead, &r.Unread); err != nil {
			rows.Close()
			return nil, err
		}
		if by.Valid {
			r.CreatedBy = &by.Int64
		}
		out = append(out, r)
	}
	rows.Close()
	if err := rows.Err(); err != nil {
		return nil, err
	}
	for i := range out {
		if err := s.fill(ctx, uid, &out[i]); err != nil {
			return nil, err
		}
	}
	return out, nil
}

// Room — одна комната (uid должен быть участником).
func (s *Service) Room(ctx context.Context, uid int64, id string) (*Room, error) {
	if err := s.need(ctx, uid, id); err != nil {
		return nil, err
	}
	var r Room
	var by sql.NullInt64
	err := s.DB.QueryRowContext(ctx, `
		SELECT r.id, r.kind, r.name, r.created_by, r.last_activity, m.last_read,
		       (SELECT count(*) FROM talk_messages x WHERE x.room_id=r.id AND x.id > m.last_read
		          AND x.user_id IS DISTINCT FROM $2 AND x.kind='text')
		FROM talk_rooms r JOIN talk_members m ON m.room_id=r.id AND m.user_id=$2 WHERE r.id=$1`, id, uid).
		Scan(&r.ID, &r.Kind, &r.Name, &by, &r.LastActivity, &r.LastRead, &r.Unread)
	if errors.Is(err, sql.ErrNoRows) {
		return nil, ErrNotFound
	}
	if err != nil {
		return nil, err
	}
	if by.Valid {
		r.CreatedBy = &by.Int64
	}
	return &r, s.fill(ctx, uid, &r)
}

// fill дописывает участников, последнее сообщение и имя личной переписки.
func (s *Service) fill(ctx context.Context, uid int64, r *Room) error {
	rows, err := s.DB.QueryContext(ctx, `SELECT u.id, u.username FROM talk_members m JOIN users u ON u.id=m.user_id
		WHERE m.room_id=$1 ORDER BY lower(u.username)`, r.ID)
	if err != nil {
		return err
	}
	r.Members = []Member{}
	for rows.Next() {
		var m Member
		if err := rows.Scan(&m.ID, &m.Username); err != nil {
			rows.Close()
			return err
		}
		r.Members = append(r.Members, m)
	}
	rows.Close()
	if r.Kind == "direct" {
		r.Name = "удалённый пользователь"
		for _, m := range r.Members {
			if m.ID != uid {
				r.Name = m.Username
			}
		}
	}
	msgs, err := s.query(ctx, `WHERE m.room_id=$1 ORDER BY m.id DESC LIMIT 1`, r.ID)
	if err != nil {
		return err
	}
	if len(msgs) > 0 {
		r.Last = &msgs[0]
	}
	return nil
}

// CreateGroup создаёт групповую комнату; создатель входит в неё автоматически.
func (s *Service) CreateGroup(ctx context.Context, uid int64, name string, members []int64) (string, error) {
	name = strings.TrimSpace(name)
	if n := utf8.RuneCountInString(name); n == 0 || n > 80 {
		return "", ErrBadRoom
	}
	if len(members) >= MaxMembers {
		return "", fmt.Errorf("не больше %d участников", MaxMembers)
	}
	tx, err := s.DB.BeginTx(ctx, nil)
	if err != nil {
		return "", err
	}
	defer tx.Rollback()
	id := files.NewID()
	if _, err := tx.ExecContext(ctx, `INSERT INTO talk_rooms (id, kind, name, created_by) VALUES ($1,'group',$2,$3)`, id, name, uid); err != nil {
		return "", err
	}
	for _, m := range append([]int64{uid}, members...) {
		if _, err := tx.ExecContext(ctx, `INSERT INTO talk_members (room_id, user_id) SELECT $1, id FROM users WHERE id=$2
			ON CONFLICT DO NOTHING`, id, m); err != nil {
			return "", err
		}
	}
	return id, tx.Commit()
}

// Direct — личная переписка с other (создаётся при первом обращении).
func (s *Service) Direct(ctx context.Context, uid, other int64) (string, error) {
	if uid == other {
		return "", ErrSelf
	}
	var exists bool
	if err := s.DB.QueryRowContext(ctx, `SELECT EXISTS(SELECT 1 FROM users WHERE id=$1)`, other).Scan(&exists); err != nil {
		return "", err
	}
	if !exists {
		return "", ErrNotFound
	}
	key := directKey(uid, other)
	tx, err := s.DB.BeginTx(ctx, nil)
	if err != nil {
		return "", err
	}
	defer tx.Rollback()
	id := files.NewID()
	// ON CONFLICT по direct_key: при гонке двух запросов комната всё равно одна
	if err := tx.QueryRowContext(ctx, `INSERT INTO talk_rooms (id, kind, created_by, direct_key) VALUES ($1,'direct',$2,$3)
		ON CONFLICT (direct_key) DO UPDATE SET direct_key=EXCLUDED.direct_key RETURNING id`, id, uid, key).Scan(&id); err != nil {
		return "", err
	}
	for _, m := range []int64{uid, other} {
		if _, err := tx.ExecContext(ctx, `INSERT INTO talk_members (room_id, user_id) VALUES ($1,$2) ON CONFLICT DO NOTHING`, id, m); err != nil {
			return "", err
		}
	}
	return id, tx.Commit()
}

// AddMembers — любой участник группы может пригласить других. Возвращает id реально добавленных.
func (s *Service) AddMembers(ctx context.Context, uid int64, room string, users []int64) ([]int64, error) {
	r, err := s.Room(ctx, uid, room)
	if err != nil {
		return nil, err
	}
	if r.Kind != "group" {
		return nil, ErrDirect
	}
	if len(r.Members)+len(users) > MaxMembers {
		return nil, fmt.Errorf("не больше %d участников", MaxMembers)
	}
	var added []int64
	for _, u := range users {
		res, err := s.DB.ExecContext(ctx, `INSERT INTO talk_members (room_id, user_id) SELECT $1, id FROM users WHERE id=$2
			ON CONFLICT DO NOTHING`, room, u)
		if err != nil {
			return nil, err
		}
		if n, _ := res.RowsAffected(); n > 0 {
			added = append(added, u)
		}
	}
	return added, nil
}

// RemoveMember: участник может выйти сам; исключать других может только создатель комнаты.
// Последний вышедший удаляет комнату. Возвращает true, если комната удалена.
func (s *Service) RemoveMember(ctx context.Context, uid int64, room string, target int64) (bool, error) {
	r, err := s.Room(ctx, uid, room)
	if err != nil {
		return false, err
	}
	if r.Kind != "group" {
		return false, ErrDirect
	}
	if target != uid && (r.CreatedBy == nil || *r.CreatedBy != uid) {
		return false, ErrForbidden
	}
	if _, err := s.DB.ExecContext(ctx, `DELETE FROM talk_members WHERE room_id=$1 AND user_id=$2`, room, target); err != nil {
		return false, err
	}
	res, err := s.DB.ExecContext(ctx, `DELETE FROM talk_rooms WHERE id=$1 AND NOT EXISTS (SELECT 1 FROM talk_members WHERE room_id=$1)`, room)
	if err != nil {
		return false, err
	}
	n, _ := res.RowsAffected()
	return n > 0, nil
}

// Rename — переименовать группу может любой участник.
func (s *Service) Rename(ctx context.Context, uid int64, room, name string) error {
	r, err := s.Room(ctx, uid, room)
	if err != nil {
		return err
	}
	if r.Kind != "group" {
		return ErrDirect
	}
	name = strings.TrimSpace(name)
	if n := utf8.RuneCountInString(name); n == 0 || n > 80 {
		return ErrBadRoom
	}
	_, err = s.DB.ExecContext(ctx, `UPDATE talk_rooms SET name=$1 WHERE id=$2`, name, room)
	return err
}

// ---------- сообщения ----------

const msgCols = `m.id, m.room_id, m.user_id, COALESCE(u.username, ''), m.kind, m.body, m.created_at`

func (s *Service) query(ctx context.Context, where string, args ...any) ([]Message, error) {
	rows, err := s.DB.QueryContext(ctx, `SELECT `+msgCols+` FROM talk_messages m LEFT JOIN users u ON u.id=m.user_id `+where, args...)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := []Message{}
	for rows.Next() {
		var m Message
		var u sql.NullInt64
		if err := rows.Scan(&m.ID, &m.RoomID, &u, &m.Username, &m.Kind, &m.Body, &m.CreatedAt); err != nil {
			return nil, err
		}
		if u.Valid {
			m.UserID = &u.Int64
		}
		out = append(out, m)
	}
	return out, rows.Err()
}

// Messages — страница истории: limit сообщений до before (0 — самые новые), по возрастанию id.
func (s *Service) Messages(ctx context.Context, uid int64, room string, before int64, limit int) ([]Message, error) {
	if err := s.need(ctx, uid, room); err != nil {
		return nil, err
	}
	if limit <= 0 || limit > pageMax {
		limit = pageDefault
	}
	if before <= 0 {
		before = 1<<63 - 1
	}
	msgs, err := s.query(ctx, `WHERE m.room_id=$1 AND m.id < $2 ORDER BY m.id DESC LIMIT $3`, room, before, limit)
	if err != nil {
		return nil, err
	}
	for i, j := 0, len(msgs)-1; i < j; i, j = i+1, j-1 {
		msgs[i], msgs[j] = msgs[j], msgs[i]
	}
	return msgs, nil
}

// Post сохраняет сообщение участника.
func (s *Service) Post(ctx context.Context, uid int64, room, body string) (*Message, error) {
	if err := s.need(ctx, uid, room); err != nil {
		return nil, err
	}
	body = strings.TrimSpace(strings.ToValidUTF8(body, "�"))
	if body == "" {
		return nil, ErrEmpty
	}
	if utf8.RuneCountInString(body) > MaxBody {
		return nil, ErrTooLong
	}
	return s.insert(ctx, room, &uid, "text", body)
}

// System — служебное сообщение («начат звонок», «X добавил Y»).
func (s *Service) System(ctx context.Context, room string, uid *int64, body string) (*Message, error) {
	return s.insert(ctx, room, uid, "system", body)
}

func (s *Service) insert(ctx context.Context, room string, uid *int64, kind, body string) (*Message, error) {
	tx, err := s.DB.BeginTx(ctx, nil)
	if err != nil {
		return nil, err
	}
	defer tx.Rollback()
	var id int64
	if err := tx.QueryRowContext(ctx, `INSERT INTO talk_messages (room_id, user_id, kind, body) VALUES ($1,$2,$3,$4) RETURNING id`,
		room, uid, kind, body).Scan(&id); err != nil {
		return nil, err
	}
	if _, err := tx.ExecContext(ctx, `UPDATE talk_rooms SET last_activity=now() WHERE id=$1`, room); err != nil {
		return nil, err
	}
	// своё сообщение сразу прочитано
	if uid != nil && kind == "text" {
		if _, err := tx.ExecContext(ctx, `UPDATE talk_members SET last_read=$1 WHERE room_id=$2 AND user_id=$3`, id, room, *uid); err != nil {
			return nil, err
		}
	}
	if err := tx.Commit(); err != nil {
		return nil, err
	}
	msgs, err := s.query(context.WithoutCancel(ctx), `WHERE m.id=$1`, id)
	if err != nil || len(msgs) == 0 {
		return nil, fmt.Errorf("talk: сообщение %d не найдено после записи: %v", id, err)
	}
	return &msgs[0], nil
}

// MarkRead сдвигает отметку прочитанного (только вперёд).
func (s *Service) MarkRead(ctx context.Context, uid int64, room string, upto int64) error {
	if err := s.need(ctx, uid, room); err != nil {
		return err
	}
	_, err := s.DB.ExecContext(ctx, `UPDATE talk_members SET last_read=GREATEST(last_read, LEAST($1, (SELECT COALESCE(max(id),0) FROM talk_messages WHERE room_id=$2)))
		WHERE room_id=$2 AND user_id=$3`, upto, room, uid)
	return err
}

// Unread — всего непрочитанных сообщений у пользователя (для значка в меню).
func (s *Service) Unread(ctx context.Context, uid int64) int {
	var n int
	s.DB.QueryRowContext(ctx, `SELECT count(*) FROM talk_messages x JOIN talk_members m ON m.room_id=x.room_id AND m.user_id=$1
		WHERE x.id > m.last_read AND x.user_id IS DISTINCT FROM $1 AND x.kind='text'`, uid).Scan(&n)
	return n
}

// Username — имя пользователя для системных сообщений.
func (s *Service) Username(ctx context.Context, uid int64) string {
	var n string
	s.DB.QueryRowContext(ctx, `SELECT username FROM users WHERE id=$1`, uid).Scan(&n)
	return n
}
