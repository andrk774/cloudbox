// Package pim — календари (CalDAV) и контакты (CardDAV) с хранением в PostgreSQL.
// Подключение с телефона: DAVx5 (Android), встроенные «Учётные записи» iOS/macOS, Thunderbird.
package pim

import (
	"bytes"
	"context"
	"crypto/sha256"
	"database/sql"
	"encoding/hex"
	"errors"
	"fmt"
	"net/http"
	"path"
	"regexp"
	"sort"
	"strings"
	"time"

	"github.com/andrk774/cloudbox/internal/auth"
	"github.com/andrk774/cloudbox/internal/files"
	"github.com/andrk774/cloudbox/internal/netx"
	"github.com/emersion/go-ical"
	"github.com/emersion/go-vcard"
	webdav "github.com/emersion/go-webdav"
	"github.com/emersion/go-webdav/caldav"
	"github.com/emersion/go-webdav/carddav"
)

const (
	CalPrefix  = "/caldav"
	CardPrefix = "/carddav"
)

var ErrNotFound = errors.New("не найдено")

type Service struct {
	DB  *sql.DB
	Loc *time.Location
}

// Collection — календарь или адресная книга.
type Collection struct {
	ID          int64     `json:"id"`
	Kind        string    `json:"kind"`
	Slug        string    `json:"slug"`
	Name        string    `json:"name"`
	Description string    `json:"description"`
	Color       string    `json:"color"`
	CreatedAt   time.Time `json:"created_at"`
	URL         string    `json:"url,omitempty"`
}

type object struct {
	ID       int64
	CollID   int64
	Name     string
	UID      string
	Data     string
	Etag     string
	Modified time.Time
}

var slugRe = regexp.MustCompile(`^[a-zA-Z0-9_.-]{1,64}$`)

func etagOf(data string) string {
	h := sha256.Sum256([]byte(data))
	return hex.EncodeToString(h[:16])
}

func (s *Service) collections(ctx context.Context, uid int64, kind string) ([]Collection, error) {
	rows, err := s.DB.QueryContext(ctx, `SELECT id, kind, slug, name, description, color, created_at FROM dav_collections
		WHERE user_id=$1 AND kind=$2 ORDER BY id`, uid, kind)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []Collection
	for rows.Next() {
		var c Collection
		if err := rows.Scan(&c.ID, &c.Kind, &c.Slug, &c.Name, &c.Description, &c.Color, &c.CreatedAt); err != nil {
			return nil, err
		}
		out = append(out, c)
	}
	if err := rows.Err(); err != nil {
		return nil, err
	}
	if len(out) == 0 {
		// у каждого пользователя есть хотя бы один календарь и одна адресная книга
		name, slug := "Личный", "personal"
		if kind == "card" {
			name, slug = "Контакты", "contacts"
		}
		c, err := s.CreateCollection(ctx, uid, kind, slug, name, "", "")
		if err != nil {
			return nil, err
		}
		out = append(out, *c)
	}
	return out, nil
}

func (s *Service) Collections(ctx context.Context, uid int64, kind string) ([]Collection, error) {
	return s.collections(ctx, uid, kind)
}

func (s *Service) CreateCollection(ctx context.Context, uid int64, kind, slug, name, desc, color string) (*Collection, error) {
	if slug == "" {
		slug = strings.ToLower(strings.ReplaceAll(files.NewID()[:8], "-", ""))
	}
	if !slugRe.MatchString(slug) {
		return nil, errors.New("недопустимый идентификатор")
	}
	name = strings.TrimSpace(name)
	if name == "" {
		name = slug
	}
	if !regexp.MustCompile(`^#[0-9a-fA-F]{6}$`).MatchString(color) {
		color = "#2f6fed"
	}
	c := Collection{Kind: kind, Slug: slug, Name: name, Description: desc, Color: color}
	err := s.DB.QueryRowContext(ctx, `INSERT INTO dav_collections (user_id, kind, slug, name, description, color)
		VALUES ($1,$2,$3,$4,$5,$6) RETURNING id, created_at`, uid, kind, slug, name, desc, color).Scan(&c.ID, &c.CreatedAt)
	if err != nil && strings.Contains(err.Error(), "duplicate") {
		return nil, files.ErrExists
	}
	return &c, err
}

func (s *Service) DeleteCollection(ctx context.Context, uid, id int64) error {
	var kind string
	if err := s.DB.QueryRowContext(ctx, `SELECT kind FROM dav_collections WHERE id=$1 AND user_id=$2`, id, uid).Scan(&kind); err != nil {
		return ErrNotFound
	}
	var n int
	s.DB.QueryRowContext(ctx, `SELECT count(*) FROM dav_collections WHERE user_id=$1 AND kind=$2`, uid, kind).Scan(&n)
	if n <= 1 {
		return errors.New("нельзя удалить последний календарь или адресную книгу")
	}
	_, err := s.DB.ExecContext(ctx, `DELETE FROM dav_collections WHERE id=$1 AND user_id=$2`, id, uid)
	return err
}

func (s *Service) collBySlug(ctx context.Context, uid int64, kind, slug string) (*Collection, error) {
	// календарь и адресная книга по умолчанию должны существовать даже без предварительного PROPFIND
	if _, err := s.collections(ctx, uid, kind); err != nil {
		return nil, err
	}
	var c Collection
	err := s.DB.QueryRowContext(ctx, `SELECT id, kind, slug, name, description, color, created_at FROM dav_collections
		WHERE user_id=$1 AND kind=$2 AND slug=$3`, uid, kind, slug).Scan(&c.ID, &c.Kind, &c.Slug, &c.Name, &c.Description, &c.Color, &c.CreatedAt)
	if errors.Is(err, sql.ErrNoRows) {
		return nil, ErrNotFound
	}
	return &c, err
}

func (s *Service) objects(ctx context.Context, collID int64) ([]object, error) {
	rows, err := s.DB.QueryContext(ctx, `SELECT id, collection_id, name, uid, data, etag, modified FROM dav_objects WHERE collection_id=$1 ORDER BY name`, collID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []object
	for rows.Next() {
		var o object
		if err := rows.Scan(&o.ID, &o.CollID, &o.Name, &o.UID, &o.Data, &o.Etag, &o.Modified); err != nil {
			return nil, err
		}
		out = append(out, o)
	}
	return out, rows.Err()
}

func (s *Service) object(ctx context.Context, collID int64, name string) (*object, error) {
	var o object
	err := s.DB.QueryRowContext(ctx, `SELECT id, collection_id, name, uid, data, etag, modified FROM dav_objects WHERE collection_id=$1 AND name=$2`,
		collID, name).Scan(&o.ID, &o.CollID, &o.Name, &o.UID, &o.Data, &o.Etag, &o.Modified)
	if errors.Is(err, sql.ErrNoRows) {
		return nil, ErrNotFound
	}
	return &o, err
}

// putObject сохраняет объект с проверкой условий If-Match / If-None-Match.
func (s *Service) putObject(ctx context.Context, collID int64, name, uid, data string, ifNoneMatch, ifMatch webdav.ConditionalMatch) (*object, error) {
	cur, err := s.object(ctx, collID, name)
	if err != nil && !errors.Is(err, ErrNotFound) {
		return nil, err
	}
	if ifNoneMatch.IsWildcard() && cur != nil {
		return nil, webdav.NewHTTPError(http.StatusPreconditionFailed, errors.New("уже существует"))
	}
	if ifMatch.IsSet() {
		if cur == nil {
			return nil, webdav.NewHTTPError(http.StatusPreconditionFailed, errors.New("не существует"))
		}
		if !ifMatch.IsWildcard() {
			want, _ := ifMatch.ETag()
			if want != cur.Etag {
				return nil, webdav.NewHTTPError(http.StatusPreconditionFailed, errors.New("изменён другим клиентом"))
			}
		}
	}
	et := etagOf(data)
	o := object{CollID: collID, Name: name, UID: uid, Data: data, Etag: et}
	err = s.DB.QueryRowContext(ctx, `INSERT INTO dav_objects (collection_id, name, uid, data, etag) VALUES ($1,$2,$3,$4,$5)
		ON CONFLICT (collection_id, name) DO UPDATE SET uid=EXCLUDED.uid, data=EXCLUDED.data, etag=EXCLUDED.etag, modified=now()
		RETURNING id, modified`, collID, name, uid, data, et).Scan(&o.ID, &o.Modified)
	return &o, err
}

// ---------- HTTP: авторизация и маршрутизация ----------

type Handler struct {
	Auth *auth.Service
	Svc  *Service

	fails netx.Limiter
}

type ctxUser struct{}

const (
	maxFails   = 20
	failWindow = 10 * time.Minute
)

func (h *Handler) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	// Защита от перебора паролей, как у WebDAV: 20 неудач за 10 минут с одного адреса.
	ip := netx.ClientIP(r)
	if h.fails.Blocked(ip, maxFails, failWindow) {
		http.Error(w, "too many failed logins", http.StatusTooManyRequests)
		return
	}
	user, pass, ok := r.BasicAuth()
	var u *auth.User
	var err error
	if ok {
		u, err = h.Auth.CheckBasic(r.Context(), user, pass)
	}
	if !ok || err != nil {
		if ok {
			h.fails.Fail(ip, failWindow)
			h.Auth.LogLogin(r.Context(), 0, user, ip, r.UserAgent(), false, "caldav")
		}
		w.Header().Set("WWW-Authenticate", `Basic realm="CloudBox", charset="UTF-8"`)
		http.Error(w, "unauthorized", http.StatusUnauthorized)
		return
	}
	ctx := context.WithValue(r.Context(), ctxUser{}, u)
	r = r.WithContext(ctx)
	p := r.URL.Path
	switch {
	case p == "/.well-known/caldav" || strings.HasPrefix(p, CalPrefix):
		(&caldav.Handler{Backend: &calBackend{s: h.Svc, u: u}, Prefix: CalPrefix}).ServeHTTP(w, r)
	case p == "/.well-known/carddav" || strings.HasPrefix(p, CardPrefix):
		(&carddav.Handler{Backend: &cardBackend{s: h.Svc, u: u}, Prefix: CardPrefix}).ServeHTTP(w, r)
	default:
		http.NotFound(w, r)
	}
}

func notFound(err error) error {
	if errors.Is(err, ErrNotFound) {
		return webdav.NewHTTPError(http.StatusNotFound, err)
	}
	return err
}

// parsePath: /prefix/<user>/<home>/<slug>/<object>
func parsePath(prefix, home, username, p string) (slug, obj string, err error) {
	rest := strings.TrimPrefix(path.Clean(p), prefix+"/"+username+"/"+home)
	if rest == path.Clean(p) {
		return "", "", webdav.NewHTTPError(http.StatusForbidden, errors.New("чужой путь"))
	}
	parts := strings.Split(strings.Trim(rest, "/"), "/")
	if len(parts) == 0 || parts[0] == "" {
		return "", "", webdav.NewHTTPError(http.StatusNotFound, ErrNotFound)
	}
	slug = parts[0]
	if len(parts) > 1 {
		obj = parts[1]
	}
	if len(parts) > 2 {
		return "", "", webdav.NewHTTPError(http.StatusNotFound, ErrNotFound)
	}
	return slug, obj, nil
}

// ---------- CalDAV ----------

type calBackend struct {
	s *Service
	u *auth.User
}

func (b *calBackend) principal() string { return CalPrefix + "/" + b.u.Username + "/" }
func (b *calBackend) home() string      { return b.principal() + "calendars/" }

func (b *calBackend) CurrentUserPrincipal(context.Context) (string, error) { return b.principal(), nil }
func (b *calBackend) CalendarHomeSetPath(context.Context) (string, error)  { return b.home(), nil }

func (b *calBackend) toCal(c Collection) caldav.Calendar {
	return caldav.Calendar{Path: b.home() + c.Slug + "/", Name: c.Name, Description: c.Description,
		MaxResourceSize: 4 << 20, SupportedComponentSet: []string{"VEVENT", "VTODO"}}
}

func (b *calBackend) CreateCalendar(ctx context.Context, cal *caldav.Calendar) error {
	slug, _, err := parsePath(CalPrefix, "calendars", b.u.Username, cal.Path)
	if err != nil {
		return err
	}
	_, err = b.s.CreateCollection(ctx, b.u.ID, "cal", slug, cal.Name, cal.Description, "")
	return err
}

func (b *calBackend) ListCalendars(ctx context.Context) ([]caldav.Calendar, error) {
	cs, err := b.s.collections(ctx, b.u.ID, "cal")
	if err != nil {
		return nil, err
	}
	out := make([]caldav.Calendar, len(cs))
	for i, c := range cs {
		out[i] = b.toCal(c)
	}
	return out, nil
}

func (b *calBackend) GetCalendar(ctx context.Context, p string) (*caldav.Calendar, error) {
	slug, _, err := parsePath(CalPrefix, "calendars", b.u.Username, p)
	if err != nil {
		return nil, err
	}
	c, err := b.s.collBySlug(ctx, b.u.ID, "cal", slug)
	if err != nil {
		return nil, notFound(err)
	}
	cal := b.toCal(*c)
	return &cal, nil
}

func (b *calBackend) toObj(c *Collection, o object) (*caldav.CalendarObject, error) {
	cal, err := ical.NewDecoder(strings.NewReader(o.Data)).Decode()
	if err != nil {
		return nil, err
	}
	return &caldav.CalendarObject{Path: b.home() + c.Slug + "/" + o.Name, ModTime: o.Modified,
		ContentLength: int64(len(o.Data)), ETag: o.Etag, Data: cal}, nil
}

func (b *calBackend) GetCalendarObject(ctx context.Context, p string, _ *caldav.CalendarCompRequest) (*caldav.CalendarObject, error) {
	slug, name, err := parsePath(CalPrefix, "calendars", b.u.Username, p)
	if err != nil {
		return nil, err
	}
	c, err := b.s.collBySlug(ctx, b.u.ID, "cal", slug)
	if err != nil {
		return nil, notFound(err)
	}
	o, err := b.s.object(ctx, c.ID, name)
	if err != nil {
		return nil, notFound(err)
	}
	return b.toObj(c, *o)
}

func (b *calBackend) ListCalendarObjects(ctx context.Context, p string, _ *caldav.CalendarCompRequest) ([]caldav.CalendarObject, error) {
	slug, _, err := parsePath(CalPrefix, "calendars", b.u.Username, p)
	if err != nil {
		return nil, err
	}
	c, err := b.s.collBySlug(ctx, b.u.ID, "cal", slug)
	if err != nil {
		return nil, notFound(err)
	}
	objs, err := b.s.objects(ctx, c.ID)
	if err != nil {
		return nil, err
	}
	out := make([]caldav.CalendarObject, 0, len(objs))
	for _, o := range objs {
		co, err := b.toObj(c, o)
		if err == nil {
			out = append(out, *co)
		}
	}
	return out, nil
}

func (b *calBackend) QueryCalendarObjects(ctx context.Context, p string, q *caldav.CalendarQuery) ([]caldav.CalendarObject, error) {
	all, err := b.ListCalendarObjects(ctx, p, &q.CompRequest)
	if err != nil {
		return nil, err
	}
	return caldav.Filter(q, all)
}

func calUID(cal *ical.Calendar) string {
	for _, c := range cal.Children {
		if v, err := c.Props.Text(ical.PropUID); err == nil && v != "" {
			return v
		}
	}
	return ""
}

func (b *calBackend) PutCalendarObject(ctx context.Context, p string, cal *ical.Calendar, opts *caldav.PutCalendarObjectOptions) (*caldav.CalendarObject, error) {
	slug, name, err := parsePath(CalPrefix, "calendars", b.u.Username, p)
	if err != nil {
		return nil, err
	}
	if name == "" {
		return nil, webdav.NewHTTPError(http.StatusBadRequest, errors.New("нет имени объекта"))
	}
	c, err := b.s.collBySlug(ctx, b.u.ID, "cal", slug)
	if err != nil {
		return nil, notFound(err)
	}
	var buf bytes.Buffer
	if err := ical.NewEncoder(&buf).Encode(cal); err != nil {
		return nil, webdav.NewHTTPError(http.StatusBadRequest, err)
	}
	o, err := b.s.putObject(ctx, c.ID, name, calUID(cal), buf.String(), opts.IfNoneMatch, opts.IfMatch)
	if err != nil {
		return nil, err
	}
	return b.toObj(c, *o)
}

func (b *calBackend) DeleteCalendarObject(ctx context.Context, p string) error {
	slug, name, err := parsePath(CalPrefix, "calendars", b.u.Username, p)
	if err != nil {
		return err
	}
	c, err := b.s.collBySlug(ctx, b.u.ID, "cal", slug)
	if err != nil {
		return notFound(err)
	}
	res, err := b.s.DB.ExecContext(ctx, `DELETE FROM dav_objects WHERE collection_id=$1 AND name=$2`, c.ID, name)
	if err != nil {
		return err
	}
	if n, _ := res.RowsAffected(); n == 0 {
		return notFound(ErrNotFound)
	}
	return nil
}

// ---------- CardDAV ----------

type cardBackend struct {
	s *Service
	u *auth.User
}

func (b *cardBackend) principal() string { return CardPrefix + "/" + b.u.Username + "/" }
func (b *cardBackend) home() string      { return b.principal() + "contacts/" }

func (b *cardBackend) CurrentUserPrincipal(context.Context) (string, error) {
	return b.principal(), nil
}
func (b *cardBackend) AddressBookHomeSetPath(context.Context) (string, error) { return b.home(), nil }

func (b *cardBackend) toAB(c Collection) carddav.AddressBook {
	return carddav.AddressBook{Path: b.home() + c.Slug + "/", Name: c.Name, Description: c.Description, MaxResourceSize: 4 << 20,
		SupportedAddressData: []carddav.AddressDataType{{ContentType: "text/vcard", Version: "3.0"}, {ContentType: "text/vcard", Version: "4.0"}}}
}

func (b *cardBackend) ListAddressBooks(ctx context.Context) ([]carddav.AddressBook, error) {
	cs, err := b.s.collections(ctx, b.u.ID, "card")
	if err != nil {
		return nil, err
	}
	out := make([]carddav.AddressBook, len(cs))
	for i, c := range cs {
		out[i] = b.toAB(c)
	}
	return out, nil
}

func (b *cardBackend) GetAddressBook(ctx context.Context, p string) (*carddav.AddressBook, error) {
	slug, _, err := parsePath(CardPrefix, "contacts", b.u.Username, p)
	if err != nil {
		return nil, err
	}
	c, err := b.s.collBySlug(ctx, b.u.ID, "card", slug)
	if err != nil {
		return nil, notFound(err)
	}
	ab := b.toAB(*c)
	return &ab, nil
}

func (b *cardBackend) CreateAddressBook(ctx context.Context, ab *carddav.AddressBook) error {
	slug, _, err := parsePath(CardPrefix, "contacts", b.u.Username, ab.Path)
	if err != nil {
		return err
	}
	_, err = b.s.CreateCollection(ctx, b.u.ID, "card", slug, ab.Name, ab.Description, "")
	return err
}

func (b *cardBackend) DeleteAddressBook(ctx context.Context, p string) error {
	slug, _, err := parsePath(CardPrefix, "contacts", b.u.Username, p)
	if err != nil {
		return err
	}
	c, err := b.s.collBySlug(ctx, b.u.ID, "card", slug)
	if err != nil {
		return notFound(err)
	}
	return b.s.DeleteCollection(ctx, b.u.ID, c.ID)
}

func (b *cardBackend) toObj(c *Collection, o object) (*carddav.AddressObject, error) {
	card, err := vcard.NewDecoder(strings.NewReader(o.Data)).Decode()
	if err != nil {
		return nil, err
	}
	return &carddav.AddressObject{Path: b.home() + c.Slug + "/" + o.Name, ModTime: o.Modified,
		ContentLength: int64(len(o.Data)), ETag: o.Etag, Card: card}, nil
}

func (b *cardBackend) GetAddressObject(ctx context.Context, p string, _ *carddav.AddressDataRequest) (*carddav.AddressObject, error) {
	slug, name, err := parsePath(CardPrefix, "contacts", b.u.Username, p)
	if err != nil {
		return nil, err
	}
	c, err := b.s.collBySlug(ctx, b.u.ID, "card", slug)
	if err != nil {
		return nil, notFound(err)
	}
	o, err := b.s.object(ctx, c.ID, name)
	if err != nil {
		return nil, notFound(err)
	}
	return b.toObj(c, *o)
}

func (b *cardBackend) ListAddressObjects(ctx context.Context, p string, _ *carddav.AddressDataRequest) ([]carddav.AddressObject, error) {
	slug, _, err := parsePath(CardPrefix, "contacts", b.u.Username, p)
	if err != nil {
		return nil, err
	}
	c, err := b.s.collBySlug(ctx, b.u.ID, "card", slug)
	if err != nil {
		return nil, notFound(err)
	}
	objs, err := b.s.objects(ctx, c.ID)
	if err != nil {
		return nil, err
	}
	out := make([]carddav.AddressObject, 0, len(objs))
	for _, o := range objs {
		if ao, err := b.toObj(c, o); err == nil {
			out = append(out, *ao)
		}
	}
	return out, nil
}

func (b *cardBackend) QueryAddressObjects(ctx context.Context, p string, q *carddav.AddressBookQuery) ([]carddav.AddressObject, error) {
	all, err := b.ListAddressObjects(ctx, p, &q.DataRequest)
	if err != nil {
		return nil, err
	}
	return carddav.Filter(q, all)
}

func (b *cardBackend) PutAddressObject(ctx context.Context, p string, card vcard.Card, opts *carddav.PutAddressObjectOptions) (*carddav.AddressObject, error) {
	slug, name, err := parsePath(CardPrefix, "contacts", b.u.Username, p)
	if err != nil {
		return nil, err
	}
	if name == "" {
		return nil, webdav.NewHTTPError(http.StatusBadRequest, errors.New("нет имени объекта"))
	}
	c, err := b.s.collBySlug(ctx, b.u.ID, "card", slug)
	if err != nil {
		return nil, notFound(err)
	}
	var buf bytes.Buffer
	if err := vcard.NewEncoder(&buf).Encode(card); err != nil {
		return nil, webdav.NewHTTPError(http.StatusBadRequest, err)
	}
	o, err := b.s.putObject(ctx, c.ID, name, card.Value(vcard.FieldUID), buf.String(), opts.IfNoneMatch, opts.IfMatch)
	if err != nil {
		return nil, err
	}
	return b.toObj(c, *o)
}

func (b *cardBackend) DeleteAddressObject(ctx context.Context, p string) error {
	slug, name, err := parsePath(CardPrefix, "contacts", b.u.Username, p)
	if err != nil {
		return err
	}
	c, err := b.s.collBySlug(ctx, b.u.ID, "card", slug)
	if err != nil {
		return notFound(err)
	}
	res, err := b.s.DB.ExecContext(ctx, `DELETE FROM dav_objects WHERE collection_id=$1 AND name=$2`, c.ID, name)
	if err != nil {
		return err
	}
	if n, _ := res.RowsAffected(); n == 0 {
		return notFound(ErrNotFound)
	}
	return nil
}

// ---------- упрощённый API для веб-интерфейса ----------

type Event struct {
	ID          int64     `json:"id"`
	Calendar    string    `json:"calendar"`
	Color       string    `json:"color"`
	UID         string    `json:"uid"`
	Summary     string    `json:"summary"`
	Location    string    `json:"location"`
	Description string    `json:"description"`
	Start       time.Time `json:"start"`
	End         time.Time `json:"end"`
	AllDay      bool      `json:"all_day"`
	Recurring   bool      `json:"recurring"`
}

// Events — события всех календарей пользователя в интервале, с развёрткой повторений.
func (s *Service) Events(ctx context.Context, uid int64, from, to time.Time) ([]Event, error) {
	cs, err := s.collections(ctx, uid, "cal")
	if err != nil {
		return nil, err
	}
	loc := s.Loc
	if loc == nil {
		loc = time.Local
	}
	out := []Event{}
	for _, c := range cs {
		objs, err := s.objects(ctx, c.ID)
		if err != nil {
			return nil, err
		}
		for _, o := range objs {
			cal, err := ical.NewDecoder(strings.NewReader(o.Data)).Decode()
			if err != nil {
				continue
			}
			for _, ch := range cal.Children {
				if ch.Name != ical.CompEvent {
					continue
				}
				ev := ical.Event{Component: ch}
				if ev.Props.Get(ical.PropRecurrenceID) != nil {
					continue // исключения из серий в упрощённом виде не показываем
				}
				start, err := ev.DateTimeStart(loc)
				if err != nil {
					continue
				}
				end, err := ev.DateTimeEnd(loc)
				if err != nil || end.IsZero() || end.Before(start) {
					end = start
				}
				allDay := false
				if p := ev.Props.Get(ical.PropDateTimeStart); p != nil && p.ValueType() == ical.ValueDate {
					allDay = true
					if end.Equal(start) {
						end = start.AddDate(0, 0, 1)
					}
				}
				base := Event{ID: o.ID, Calendar: c.Name, Color: c.Color, UID: o.UID, AllDay: allDay}
				base.Summary, _ = ev.Props.Text(ical.PropSummary)
				base.Location, _ = ev.Props.Text(ical.PropLocation)
				base.Description, _ = ev.Props.Text(ical.PropDescription)
				dur := end.Sub(start)
				if ev.Props.Get(ical.PropRecurrenceRule) != nil {
					set, err := ev.RecurrenceSet(loc)
					if err == nil && set != nil {
						for _, t := range set.Between(from.Add(-dur), to, true) {
							e := base
							e.Start, e.End, e.Recurring = t, t.Add(dur), true
							out = append(out, e)
							if len(out) > 5000 {
								break
							}
						}
						continue
					}
				}
				if start.Before(to) && end.After(from) || start.Equal(from) {
					e := base
					e.Start, e.End = start, end
					out = append(out, e)
				}
			}
		}
	}
	sort.Slice(out, func(i, j int) bool { return out[i].Start.Before(out[j].Start) })
	return out, nil
}

type EventInput struct {
	Calendar    int64     `json:"calendar_id"`
	Summary     string    `json:"summary"`
	Location    string    `json:"location"`
	Description string    `json:"description"`
	Start       time.Time `json:"start"`
	End         time.Time `json:"end"`
	AllDay      bool      `json:"all_day"`
	RRule       string    `json:"rrule"` // например FREQ=WEEKLY
}

func (s *Service) collByID(ctx context.Context, uid, id int64, kind string) (*Collection, error) {
	cs, err := s.collections(ctx, uid, kind)
	if err != nil {
		return nil, err
	}
	for i := range cs {
		if cs[i].ID == id || id == 0 {
			return &cs[i], nil
		}
	}
	return nil, ErrNotFound
}

func (s *Service) CreateEvent(ctx context.Context, uid int64, in EventInput) (int64, error) {
	if strings.TrimSpace(in.Summary) == "" {
		return 0, errors.New("укажите название события")
	}
	if in.End.Before(in.Start) {
		return 0, errors.New("окончание раньше начала")
	}
	c, err := s.collByID(ctx, uid, in.Calendar, "cal")
	if err != nil {
		return 0, err
	}
	id := files.NewID()
	ev := ical.NewEvent()
	ev.Props.SetText(ical.PropUID, id)
	ev.Props.SetDateTime(ical.PropDateTimeStamp, time.Now().UTC())
	ev.Props.SetText(ical.PropSummary, strings.TrimSpace(in.Summary))
	if in.Location != "" {
		ev.Props.SetText(ical.PropLocation, in.Location)
	}
	if in.Description != "" {
		ev.Props.SetText(ical.PropDescription, in.Description)
	}
	if in.AllDay {
		ev.Props.SetDate(ical.PropDateTimeStart, in.Start)
		end := in.End
		if !end.After(in.Start) {
			end = in.Start.AddDate(0, 0, 1)
		}
		ev.Props.SetDate(ical.PropDateTimeEnd, end)
	} else {
		ev.Props.SetDateTime(ical.PropDateTimeStart, in.Start.UTC())
		ev.Props.SetDateTime(ical.PropDateTimeEnd, in.End.UTC())
	}
	if rr := strings.TrimSpace(in.RRule); rr != "" {
		if !regexp.MustCompile(`^[A-Z0-9=;,:-]+$`).MatchString(rr) {
			return 0, errors.New("неверное правило повторения")
		}
		ev.Props.Set(&ical.Prop{Name: ical.PropRecurrenceRule, Value: rr, Params: ical.Params{}})
	}
	cal := ical.NewCalendar()
	cal.Props.SetText(ical.PropVersion, "2.0")
	cal.Props.SetText(ical.PropProductID, "-//CloudBox//RU")
	cal.Children = append(cal.Children, ev.Component)
	var buf bytes.Buffer
	if err := ical.NewEncoder(&buf).Encode(cal); err != nil {
		return 0, err
	}
	o, err := s.putObject(ctx, c.ID, id+".ics", id, buf.String(), "", "")
	if err != nil {
		return 0, err
	}
	return o.ID, nil
}

// DeleteObject удаляет событие или контакт по id (только свои).
func (s *Service) DeleteObject(ctx context.Context, uid, id int64) error {
	res, err := s.DB.ExecContext(ctx, `DELETE FROM dav_objects o USING dav_collections c
		WHERE o.id=$1 AND o.collection_id=c.id AND c.user_id=$2`, id, uid)
	if err != nil {
		return err
	}
	if n, _ := res.RowsAffected(); n == 0 {
		return ErrNotFound
	}
	return nil
}

type Contact struct {
	ID     int64    `json:"id"`
	Book   string   `json:"book"`
	UID    string   `json:"uid"`
	Name   string   `json:"name"`
	Org    string   `json:"org"`
	Emails []string `json:"emails"`
	Phones []string `json:"phones"`
	Note   string   `json:"note"`
}

func (s *Service) Contacts(ctx context.Context, uid int64) ([]Contact, error) {
	cs, err := s.collections(ctx, uid, "card")
	if err != nil {
		return nil, err
	}
	out := []Contact{}
	for _, c := range cs {
		objs, err := s.objects(ctx, c.ID)
		if err != nil {
			return nil, err
		}
		for _, o := range objs {
			card, err := vcard.NewDecoder(strings.NewReader(o.Data)).Decode()
			if err != nil {
				continue
			}
			ct := Contact{ID: o.ID, Book: c.Name, UID: o.UID, Name: card.PreferredValue(vcard.FieldFormattedName),
				Org: card.PreferredValue(vcard.FieldOrganization), Note: card.Value(vcard.FieldNote),
				Emails: card.Values(vcard.FieldEmail), Phones: card.Values(vcard.FieldTelephone)}
			if ct.Name == "" {
				if n := card.Name(); n != nil {
					ct.Name = strings.TrimSpace(n.GivenName + " " + n.FamilyName)
				}
			}
			if ct.Emails == nil {
				ct.Emails = []string{}
			}
			if ct.Phones == nil {
				ct.Phones = []string{}
			}
			out = append(out, ct)
		}
	}
	sort.Slice(out, func(i, j int) bool { return strings.ToLower(out[i].Name) < strings.ToLower(out[j].Name) })
	return out, nil
}

type ContactInput struct {
	Book   int64    `json:"book_id"`
	Name   string   `json:"name"`
	Org    string   `json:"org"`
	Emails []string `json:"emails"`
	Phones []string `json:"phones"`
	Note   string   `json:"note"`
}

func (s *Service) CreateContact(ctx context.Context, uid int64, in ContactInput) (int64, error) {
	in.Name = strings.TrimSpace(in.Name)
	if in.Name == "" {
		return 0, errors.New("укажите имя")
	}
	c, err := s.collByID(ctx, uid, in.Book, "card")
	if err != nil {
		return 0, err
	}
	id := files.NewID()
	card := vcard.Card{}
	card.SetValue(vcard.FieldVersion, "3.0")
	card.SetValue(vcard.FieldUID, id)
	card.SetValue(vcard.FieldFormattedName, in.Name)
	parts := strings.Fields(in.Name)
	n := &vcard.Name{GivenName: in.Name}
	if len(parts) >= 2 {
		n = &vcard.Name{GivenName: parts[0], FamilyName: strings.Join(parts[1:], " ")}
	}
	card.SetName(n)
	if in.Org != "" {
		card.SetValue(vcard.FieldOrganization, in.Org)
	}
	for _, e := range in.Emails {
		if e = strings.TrimSpace(e); e != "" {
			card.AddValue(vcard.FieldEmail, e)
		}
	}
	for _, p := range in.Phones {
		if p = strings.TrimSpace(p); p != "" {
			card.AddValue(vcard.FieldTelephone, p)
		}
	}
	if in.Note != "" {
		card.SetValue(vcard.FieldNote, in.Note)
	}
	var buf bytes.Buffer
	if err := vcard.NewEncoder(&buf).Encode(card); err != nil {
		return 0, err
	}
	o, err := s.putObject(ctx, c.ID, id+".vcf", id, buf.String(), "", "")
	if err != nil {
		return 0, err
	}
	return o.ID, nil
}

// URLs — адреса для настройки клиентов.
func URLs(public, username string) map[string]string {
	return map[string]string{
		"caldav":       public + CalPrefix + "/" + username + "/",
		"carddav":      public + CardPrefix + "/" + username + "/",
		"server":       public,
		"davx5":        public + "/.well-known/caldav",
		"how_to_login": fmt.Sprintf("логин %s, пароль приложения из настроек", username),
	}
}
