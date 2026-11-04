package mail

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"io"
	"net"
	"sort"
	"strings"
	"time"

	"github.com/emersion/go-imap"
	"github.com/emersion/go-imap/client"
)

var (
	ErrAuth        = errors.New("сервер отклонил логин или пароль (для Яндекса, Mail.ru и Gmail нужен пароль приложения)")
	ErrNoSuchMsg   = errors.New("письмо не найдено — возможно, его уже удалили или переместили")
	ErrBadFlag     = errors.New("недопустимый флаг")
	ErrTooBig      = errors.New("письмо слишком большое для просмотра (больше 30 МБ)")
	ErrFolderEmpty = errors.New("укажите папку")
)

const (
	sessionDeadline = 5 * time.Minute
	maxMessageBytes = 30 << 20
	pageSize        = 50
)

type session struct {
	c   *client.Client
	raw net.Conn
}

func (s *session) close() {
	s.c.Logout() // вежливо; ошибка не важна
	s.raw.Close()
}

// connect — новое IMAP-соединение с входом. Каждый запрос веб-интерфейса работает со своим соединением.
func (s *Service) connect(ctx context.Context, a *Account) (*session, error) {
	raw, err := s.Dialer.Dial(ctx, a.IMAPHost, a.IMAPPort, a.IMAPSec)
	if err != nil {
		return nil, err
	}
	raw.SetDeadline(time.Now().Add(sessionDeadline))
	c, err := client.New(raw)
	if err != nil {
		raw.Close()
		return nil, fmt.Errorf("сервер %s не отвечает как IMAP", a.IMAPHost)
	}
	if a.IMAPSec == SecSTARTTLS {
		if err := c.StartTLS(TLSConfig(a.IMAPHost)); err != nil {
			raw.Close()
			return nil, fmt.Errorf("STARTTLS: %v", err)
		}
	}
	if err := c.Login(a.Login, a.password); err != nil {
		raw.Close()
		return nil, ErrAuth
	}
	return &session{c: c, raw: raw}, nil
}

func (s *Service) with(ctx context.Context, a *Account, fn func(c *client.Client) error) error {
	ss, err := s.connect(ctx, a)
	if err != nil {
		return err
	}
	defer ss.close()
	return fn(ss.c)
}

// ---------- папки ----------

type Folder struct {
	Name    string `json:"name"`
	Delim   string `json:"delim"`
	Special string `json:"special,omitempty"` // inbox, drafts, sent, junk, trash, archive
	Unread  uint32 `json:"unread"`
	Total   uint32 `json:"total"`
}

var specialAttrs = map[string]string{`\Sent`: "sent", `\Trash`: "trash", `\Drafts`: "drafts", `\Junk`: "junk", `\Archive`: "archive", `\All`: "all"}

// Имена служебных папок у серверов без SPECIAL-USE (RFC 6154).
var specialNames = map[string]string{
	"sent": "sent", "sent items": "sent", "sent messages": "sent", "отправленные": "sent",
	"trash": "trash", "deleted": "trash", "deleted items": "trash", "deleted messages": "trash", "удаленные": "trash", "удалённые": "trash", "корзина": "trash",
	"drafts": "drafts", "черновики": "drafts",
	"spam": "junk", "junk": "junk", "спам": "junk",
	"archive": "archive", "архив": "archive",
}

var specialOrder = map[string]int{"inbox": 0, "drafts": 1, "sent": 2, "archive": 3, "junk": 4, "trash": 5, "": 6}

func specialOf(info *imap.MailboxInfo) string {
	if strings.EqualFold(info.Name, "INBOX") {
		return "inbox"
	}
	for _, a := range info.Attributes {
		if sp, ok := specialAttrs[a]; ok {
			return sp
		}
	}
	leaf := info.Name
	if info.Delimiter != "" {
		if i := strings.LastIndex(leaf, info.Delimiter); i >= 0 {
			leaf = leaf[i+len(info.Delimiter):]
		}
	}
	return specialNames[strings.ToLower(leaf)]
}

func listMailboxes(c *client.Client) ([]*imap.MailboxInfo, error) {
	ch := make(chan *imap.MailboxInfo, 64)
	done := make(chan error, 1)
	go func() { done <- c.List("", "*", ch) }()
	var out []*imap.MailboxInfo
	for m := range ch {
		out = append(out, m)
	}
	return out, <-done
}

func (s *Service) Folders(ctx context.Context, a *Account) ([]Folder, error) {
	var out []Folder
	err := s.with(ctx, a, func(c *client.Client) error {
		boxes, err := listMailboxes(c)
		if err != nil {
			return err
		}
		for _, b := range boxes {
			if hasAttr(b.Attributes, imap.NoSelectAttr) || hasAttr(b.Attributes, `\NonExistent`) {
				continue
			}
			f := Folder{Name: b.Name, Delim: b.Delimiter, Special: specialOf(b)}
			if f.Special == "all" { // «Вся почта» Gmail дублирует все письма — показываем, но как обычную папку
				f.Special = ""
			}
			if len(out) < 200 { // STATUS по каждой папке; у огромных деревьев счётчики только для первых 200
				if st, err := c.Status(b.Name, []imap.StatusItem{imap.StatusMessages, imap.StatusUnseen}); err == nil {
					f.Total, f.Unread = st.Messages, st.Unseen
				}
			}
			out = append(out, f)
		}
		return nil
	})
	sort.SliceStable(out, func(i, j int) bool {
		oi, oj := specialOrder[out[i].Special], specialOrder[out[j].Special]
		if oi != oj {
			return oi < oj
		}
		return strings.ToLower(out[i].Name) < strings.ToLower(out[j].Name)
	})
	return out, err
}

func hasAttr(attrs []string, a string) bool {
	for _, x := range attrs {
		if strings.EqualFold(x, a) {
			return true
		}
	}
	return false
}

// findSpecial — имя служебной папки (sent, trash…) или "".
func findSpecial(c *client.Client, kind string) string {
	boxes, err := listMailboxes(c)
	if err != nil {
		return ""
	}
	for _, b := range boxes {
		if specialOf(b) == kind && !hasAttr(b.Attributes, imap.NoSelectAttr) {
			return b.Name
		}
	}
	return ""
}

// ---------- список писем ----------

type Address struct {
	Name  string `json:"name"`
	Email string `json:"email"`
}

type Summary struct {
	UID         uint32    `json:"uid"`
	Subject     string    `json:"subject"`
	From        []Address `json:"from"`
	To          []Address `json:"to"`
	Date        time.Time `json:"date"`
	Seen        bool      `json:"seen"`
	Flagged     bool      `json:"flagged"`
	Answered    bool      `json:"answered"`
	Size        uint32    `json:"size"`
	Attachments bool      `json:"attachments"`
}

type Page struct {
	Folder   string    `json:"folder"`
	Total    uint32    `json:"total"`
	Offset   int       `json:"offset"`
	Messages []Summary `json:"messages"`
}

func addrs(list []*imap.Address) []Address {
	out := []Address{}
	for _, a := range list {
		if a == nil || a.MailboxName == "" {
			continue
		}
		out = append(out, Address{Name: a.PersonalName, Email: a.Address()})
	}
	return out
}

func hasAttachments(bs *imap.BodyStructure) bool {
	if bs == nil {
		return false
	}
	if strings.EqualFold(bs.Disposition, "attachment") {
		return true
	}
	for _, p := range bs.Parts {
		if hasAttachments(p) {
			return true
		}
	}
	return false
}

func summarize(m *imap.Message) Summary {
	s := Summary{UID: m.Uid, Size: m.Size, Attachments: hasAttachments(m.BodyStructure), Date: m.InternalDate}
	if e := m.Envelope; e != nil {
		s.Subject = e.Subject
		s.From, s.To = addrs(e.From), addrs(e.To)
		if !e.Date.IsZero() {
			s.Date = e.Date
		}
	}
	for _, f := range m.Flags {
		switch f {
		case imap.SeenFlag:
			s.Seen = true
		case imap.FlaggedFlag:
			s.Flagged = true
		case imap.AnsweredFlag:
			s.Answered = true
		}
	}
	return s
}

func fetchSummaries(c *client.Client, set *imap.SeqSet, uid bool) ([]Summary, error) {
	items := []imap.FetchItem{imap.FetchEnvelope, imap.FetchFlags, imap.FetchUid, imap.FetchInternalDate, imap.FetchRFC822Size, imap.FetchBodyStructure}
	ch := make(chan *imap.Message, 64)
	done := make(chan error, 1)
	go func() {
		if uid {
			done <- c.UidFetch(set, items, ch)
		} else {
			done <- c.Fetch(set, items, ch)
		}
	}()
	out := []Summary{}
	for m := range ch {
		out = append(out, summarize(m))
	}
	if err := <-done; err != nil {
		return nil, err
	}
	sort.Slice(out, func(i, j int) bool { return out[i].UID > out[j].UID }) // новые сверху
	return out, nil
}

// Messages — страница писем папки, новые сверху; q — поиск на стороне сервера (тема, отправитель, текст).
func (s *Service) Messages(ctx context.Context, a *Account, folder string, offset int, q string) (*Page, error) {
	if folder == "" {
		return nil, ErrFolderEmpty
	}
	if offset < 0 {
		offset = 0
	}
	p := &Page{Folder: folder, Offset: offset, Messages: []Summary{}}
	err := s.with(ctx, a, func(c *client.Client) error {
		st, err := c.Select(folder, true)
		if err != nil {
			return fmt.Errorf("папка «%s» недоступна", folder)
		}
		p.Total = st.Messages
		if q = strings.TrimSpace(q); q != "" {
			crit := imap.NewSearchCriteria()
			crit.Text = []string{q}
			uids, err := c.UidSearch(crit)
			if err != nil {
				return fmt.Errorf("сервер не выполнил поиск: %v", err)
			}
			p.Total = uint32(len(uids))
			sort.Slice(uids, func(i, j int) bool { return uids[i] > uids[j] })
			if offset >= len(uids) {
				return nil
			}
			uids = uids[offset:min(len(uids), offset+pageSize)]
			set := new(imap.SeqSet)
			set.AddNum(uids...)
			p.Messages, err = fetchSummaries(c, set, true)
			return err
		}
		hi := int(st.Messages) - offset
		if hi < 1 {
			return nil
		}
		lo := max(1, hi-pageSize+1)
		set := new(imap.SeqSet)
		set.AddRange(uint32(lo), uint32(hi))
		p.Messages, err = fetchSummaries(c, set, false)
		return err
	})
	return p, err
}

// raw — письмо целиком (BODY.PEEK[] — без отметки «прочитано»).
func raw(c *client.Client, uid uint32) ([]byte, []string, error) {
	set := new(imap.SeqSet)
	set.AddNum(uid)
	section := &imap.BodySectionName{Peek: true}
	ch := make(chan *imap.Message, 1)
	done := make(chan error, 1)
	go func() {
		done <- c.UidFetch(set, []imap.FetchItem{section.FetchItem(), imap.FetchFlags, imap.FetchRFC822Size, imap.FetchUid}, ch)
	}()
	var body []byte
	var flags []string
	var found, tooBig bool
	for m := range ch {
		found = true
		flags = m.Flags
		if m.Size > maxMessageBytes {
			tooBig = true
			continue
		}
		if r := m.GetBody(section); r != nil {
			b, err := io.ReadAll(io.LimitReader(r, maxMessageBytes+1))
			if err == nil {
				body = b
			}
		}
	}
	if err := <-done; err != nil {
		return nil, nil, err
	}
	switch {
	case tooBig || len(body) > maxMessageBytes:
		return nil, nil, ErrTooBig
	case !found || body == nil:
		return nil, nil, ErrNoSuchMsg
	}
	return body, flags, nil
}

// Message открывает письмо и отмечает его прочитанным.
func (s *Service) Message(ctx context.Context, a *Account, folder string, uid uint32) (*Parsed, error) {
	var out *Parsed
	err := s.with(ctx, a, func(c *client.Client) error {
		if _, err := c.Select(folder, false); err != nil {
			return fmt.Errorf("папка «%s» недоступна", folder)
		}
		body, flags, err := raw(c, uid)
		if err != nil {
			return err
		}
		if out, err = Parse(body); err != nil {
			return err
		}
		out.UID, out.Folder = uid, folder
		for _, f := range flags {
			out.Flagged = out.Flagged || f == imap.FlaggedFlag
		}
		if !hasAttr(flags, imap.SeenFlag) {
			set := new(imap.SeqSet)
			set.AddNum(uid)
			c.UidStore(set, imap.FormatFlagsOp(imap.AddFlags, true), []interface{}{imap.SeenFlag}, nil)
		}
		return nil
	})
	return out, err
}

// Raw — исходный текст письма (для вложений и «Показать оригинал»).
func (s *Service) Raw(ctx context.Context, a *Account, folder string, uid uint32) ([]byte, error) {
	var body []byte
	err := s.with(ctx, a, func(c *client.Client) error {
		if _, err := c.Select(folder, true); err != nil {
			return fmt.Errorf("папка «%s» недоступна", folder)
		}
		var err error
		body, _, err = raw(c, uid)
		return err
	})
	return body, err
}

// ---------- изменения ----------

var allowedFlags = map[string]string{"seen": imap.SeenFlag, "flagged": imap.FlaggedFlag}

func uidSet(uids []uint32) (*imap.SeqSet, error) {
	if len(uids) == 0 || len(uids) > 1000 {
		return nil, errors.New("выберите от 1 до 1000 писем")
	}
	set := new(imap.SeqSet)
	set.AddNum(uids...)
	return set, nil
}

// SetFlag ставит или снимает «прочитано» / «важное».
func (s *Service) SetFlag(ctx context.Context, a *Account, folder string, uids []uint32, flag string, on bool) error {
	f, ok := allowedFlags[flag]
	if !ok {
		return ErrBadFlag
	}
	set, err := uidSet(uids)
	if err != nil {
		return err
	}
	var op imap.FlagsOp = imap.AddFlags
	if !on {
		op = imap.RemoveFlags
	}
	return s.with(ctx, a, func(c *client.Client) error {
		if _, err := c.Select(folder, false); err != nil {
			return fmt.Errorf("папка «%s» недоступна", folder)
		}
		return c.UidStore(set, imap.FormatFlagsOp(op, true), []interface{}{f}, nil)
	})
}

func (s *Service) Move(ctx context.Context, a *Account, folder string, uids []uint32, dest string) error {
	set, err := uidSet(uids)
	if err != nil {
		return err
	}
	if dest == "" || dest == folder {
		return errors.New("выберите другую папку")
	}
	return s.with(ctx, a, func(c *client.Client) error {
		if _, err := c.Select(folder, false); err != nil {
			return fmt.Errorf("папка «%s» недоступна", folder)
		}
		return moveUIDs(c, set, dest)
	})
}

// Delete переносит письма в корзину, а из самой корзины — удаляет навсегда.
func (s *Service) Delete(ctx context.Context, a *Account, folder string, uids []uint32) (permanent bool, err error) {
	set, err := uidSet(uids)
	if err != nil {
		return false, err
	}
	err = s.with(ctx, a, func(c *client.Client) error {
		trash := findSpecial(c, "trash")
		if _, err := c.Select(folder, false); err != nil {
			return fmt.Errorf("папка «%s» недоступна", folder)
		}
		if trash != "" && trash != folder {
			return moveUIDs(c, set, trash)
		}
		permanent = true
		if err := c.UidStore(set, imap.FormatFlagsOp(imap.AddFlags, true), []interface{}{imap.DeletedFlag}, nil); err != nil {
			return err
		}
		return c.Expunge(nil)
	})
	return permanent, err
}

// appendSent кладёт отправленное письмо в «Отправленные» (если такая папка есть).
func (s *Service) appendSent(ctx context.Context, a *Account, msg []byte) error {
	return s.with(ctx, a, func(c *client.Client) error {
		sent := findSpecial(c, "sent")
		if sent == "" {
			return nil
		}
		return c.Append(sent, []string{imap.SeenFlag}, time.Now(), bytes.NewBuffer(msg))
	})
}

// moveUIDs — MOVE (RFC 6851), а если сервер его объявил, но отказал, — COPY + \Deleted + EXPUNGE.
// MOVE атомарен: при отказе ничего не перемещено, поэтому повтор через COPY не создаёт дублей.
func moveUIDs(c *client.Client, set *imap.SeqSet, dest string) error {
	err := c.UidMove(set, dest)
	if err == nil {
		return nil
	}
	if cerr := c.UidCopy(set, dest); cerr != nil {
		return err
	}
	if serr := c.UidStore(set, imap.FormatFlagsOp(imap.AddFlags, true), []interface{}{imap.DeletedFlag}, nil); serr != nil {
		return serr
	}
	return c.Expunge(nil)
}
