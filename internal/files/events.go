package files

import (
	"context"
	"fmt"
	"log"
	"time"
)

// ---------- журнал действий ----------

type Activity struct {
	ID        int64     `json:"id"`
	OwnerID   int64     `json:"owner_id"`
	ActorID   *int64    `json:"actor_id,omitempty"`
	ActorName string    `json:"actor"`
	Action    string    `json:"action"`
	NodeID    *string   `json:"node_id,omitempty"`
	NodeName  string    `json:"node_name"`
	Details   string    `json:"details"`
	IP        string    `json:"ip"`
	At        time.Time `json:"at"`
}

// Log пишет событие в журнал владельца. actor == nil — гость по публичной ссылке или система.
func (s *Service) Log(ctx context.Context, owner int64, actor *int64, actorName, action string, n *Node, details, ip string) {
	var nodeID *string
	name := ""
	if n != nil {
		nodeID, name = &n.ID, n.Name
	}
	if _, err := s.DB.ExecContext(context.WithoutCancel(ctx), `INSERT INTO activity (owner_id, actor_id, actor_name, action, node_id, node_name, details, ip)
		VALUES ($1,$2,$3,$4,$5,$6,$7,$8)`, owner, actor, actorName, action, nodeID, name, details, ip); err != nil {
		log.Println("activity:", err)
	}
}

// ActivityFor — события по файлам пользователя (и его собственные действия в чужих общих папках).
func (s *Service) ActivityFor(ctx context.Context, uid int64, nodeID string, limit int) ([]Activity, error) {
	if limit <= 0 || limit > 500 {
		limit = 100
	}
	q := `SELECT id, owner_id, actor_id, actor_name, action, node_id, node_name, details, ip, at FROM activity
		WHERE (owner_id=$1 OR actor_id=$1)`
	args := []any{uid}
	if nodeID != "" {
		q = `SELECT id, owner_id, actor_id, actor_name, action, node_id, node_name, details, ip, at FROM activity WHERE node_id=$1`
		args = []any{nodeID}
	}
	rows, err := s.DB.QueryContext(ctx, q+fmt.Sprintf(` ORDER BY at DESC LIMIT %d`, limit), args...)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := []Activity{}
	for rows.Next() {
		var a Activity
		if err := rows.Scan(&a.ID, &a.OwnerID, &a.ActorID, &a.ActorName, &a.Action, &a.NodeID, &a.NodeName, &a.Details, &a.IP, &a.At); err != nil {
			return nil, err
		}
		out = append(out, a)
	}
	return out, rows.Err()
}

// ---------- уведомления ----------

type Notification struct {
	ID        int64      `json:"id"`
	Kind      string     `json:"kind"`
	Title     string     `json:"title"`
	Body      string     `json:"body"`
	Link      string     `json:"link"`
	CreatedAt time.Time  `json:"created_at"`
	ReadAt    *time.Time `json:"read_at"`
}

// Notify создаёт уведомление. dedupe != "" — не создавать повторно с тем же ключом.
func (s *Service) Notify(ctx context.Context, uid int64, kind, title, body, link, dedupe string) {
	var d *string
	if dedupe != "" {
		d = &dedupe
	}
	if _, err := s.DB.ExecContext(context.WithoutCancel(ctx), `INSERT INTO notifications (user_id, kind, title, body, link, dedupe)
		VALUES ($1,$2,$3,$4,$5,$6) ON CONFLICT DO NOTHING`, uid, kind, title, body, link, d); err != nil {
		log.Println("notify:", err)
	}
}

func (s *Service) Notifications(ctx context.Context, uid int64) ([]Notification, int, error) {
	rows, err := s.DB.QueryContext(ctx, `SELECT id, kind, title, body, link, created_at, read_at FROM notifications
		WHERE user_id=$1 ORDER BY created_at DESC LIMIT 100`, uid)
	if err != nil {
		return nil, 0, err
	}
	defer rows.Close()
	out := []Notification{}
	for rows.Next() {
		var n Notification
		if err := rows.Scan(&n.ID, &n.Kind, &n.Title, &n.Body, &n.Link, &n.CreatedAt, &n.ReadAt); err != nil {
			return nil, 0, err
		}
		out = append(out, n)
	}
	var unread int
	s.DB.QueryRowContext(ctx, `SELECT count(*) FROM notifications WHERE user_id=$1 AND read_at IS NULL`, uid).Scan(&unread)
	return out, unread, rows.Err()
}

func (s *Service) UnreadCount(ctx context.Context, uid int64) int {
	var n int
	s.DB.QueryRowContext(ctx, `SELECT count(*) FROM notifications WHERE user_id=$1 AND read_at IS NULL`, uid).Scan(&n)
	return n
}

func (s *Service) MarkRead(ctx context.Context, uid int64, ids []int64) error {
	if len(ids) == 0 {
		_, err := s.DB.ExecContext(ctx, `UPDATE notifications SET read_at=now() WHERE user_id=$1 AND read_at IS NULL`, uid)
		return err
	}
	for _, id := range ids {
		if _, err := s.DB.ExecContext(ctx, `UPDATE notifications SET read_at=now() WHERE user_id=$1 AND id=$2`, uid, id); err != nil {
			return err
		}
	}
	return nil
}

func (s *Service) ClearNotifications(ctx context.Context, uid int64) error {
	_, err := s.DB.ExecContext(ctx, `DELETE FROM notifications WHERE user_id=$1`, uid)
	return err
}

// CheckQuotaWarning — уведомить, если занято больше 90% квоты (не чаще раза в сутки).
func (s *Service) CheckQuotaWarning(ctx context.Context, uid int64) {
	used, quota, err := s.Usage(ctx, uid)
	if err != nil || quota <= 0 || used*10 < quota*9 {
		return
	}
	pct := used * 100 / quota
	s.Notify(ctx, uid, "quota", fmt.Sprintf("Хранилище заполнено на %d%%", pct),
		"Удалите ненужные файлы, очистите корзину или попросите администратора увеличить квоту.", "#/settings",
		"quota-"+time.Now().Format("2006-01-02"))
}

// NotifyExpiringShares — предупредить владельцев о ссылках, которые истекут в ближайшие сутки.
func (s *Service) NotifyExpiringShares(ctx context.Context) {
	rows, err := s.DB.QueryContext(ctx, `SELECT s.id, s.owner_id, n.name, s.expires_at FROM shares s JOIN nodes n ON n.id=s.node_id
		WHERE s.expires_at BETWEEN now() AND now() + interval '24 hours' AND n.deleted_at IS NULL`)
	if err != nil {
		return
	}
	type it struct {
		id    string
		owner int64
		name  string
		exp   time.Time
	}
	var items []it
	for rows.Next() {
		var x it
		if rows.Scan(&x.id, &x.owner, &x.name, &x.exp) == nil {
			items = append(items, x)
		}
	}
	rows.Close()
	for _, x := range items {
		s.Notify(ctx, x.owner, "share_expiring", "Ссылка скоро перестанет работать",
			fmt.Sprintf("Ссылка на «%s» истекает %s.", x.name, x.exp.In(s.loc()).Format("02.01.2006 15:04")),
			"#/shares", "exp-"+x.id)
	}
}

func (s *Service) loc() *time.Location {
	if s.Loc != nil {
		return s.Loc
	}
	return time.Local
}
