package talk

import (
	"encoding/json"
	"errors"
	"sort"
	"sync"
	"time"

	"github.com/andrk774/cloudbox/internal/secure"
)

// Hub — доставка событий открытым вкладкам (Server-Sent Events) и состояние идущих звонков.
//
// Каждое SSE-подключение — отдельный «пир» со своим случайным id: у пользователя может быть
// несколько вкладок и устройств, и WebRTC-сигналы адресуются конкретной вкладке, а не пользователю.
type Hub struct {
	mu    sync.Mutex
	subs  map[int64]map[string]*Sub // user_id → peer_id → подписка
	peers map[string]*Sub
	calls map[string]*Call // room_id → звонок
}

// Event — событие для клиента: в SSE уходит как `event: <Type>` и `data: <JSON Data>`.
type Event struct {
	Type string
	Data any
}

type Sub struct {
	Peer     string
	UserID   int64
	Username string
	C        chan []byte // готовые SSE-кадры
	closed   bool
}

// Participant — участник звонка (одна вкладка).
type Participant struct {
	Peer     string `json:"peer"`
	UserID   int64  `json:"user_id"`
	Username string `json:"username"`
}

type Call struct {
	Room         string
	Started      time.Time
	Participants map[string]Participant
}

// CallState — то, что видят участники комнаты: кто сейчас в звонке.
type CallState struct {
	Room         string        `json:"room"`
	Active       bool          `json:"active"`
	StartedAt    *time.Time    `json:"started_at,omitempty"`
	Participants []Participant `json:"participants"`
}

// MaxCallPeers — звонок устроен «каждый с каждым» (mesh): больше 6 участников браузеры не вытянут.
const MaxCallPeers = 6

var (
	ErrPeer      = errors.New("подключение к чату не найдено — обновите страницу")
	ErrNotInCall = errors.New("вы не участвуете в этом звонке")
	ErrCallFull  = errors.New("в звонке уже максимум участников")
)

func NewHub() *Hub {
	return &Hub{subs: map[int64]map[string]*Sub{}, peers: map[string]*Sub{}, calls: map[string]*Call{}}
}

func frame(e Event) []byte {
	b, err := json.Marshal(e.Data)
	if err != nil {
		b = []byte("null")
	}
	return []byte("event: " + e.Type + "\ndata: " + string(b) + "\n\n")
}

// Subscribe регистрирует новое подключение пользователя.
func (h *Hub) Subscribe(uid int64, username string) *Sub {
	s := &Sub{Peer: secure.Token(12), UserID: uid, Username: username, C: make(chan []byte, 256)}
	h.mu.Lock()
	defer h.mu.Unlock()
	if h.subs[uid] == nil {
		h.subs[uid] = map[string]*Sub{}
	}
	h.subs[uid][s.Peer] = s
	h.peers[s.Peer] = s
	return s
}

// Unsubscribe убирает подключение и выводит его из всех звонков.
// Возвращает изменившиеся звонки — их состояние нужно разослать участникам комнат.
func (h *Hub) Unsubscribe(s *Sub) []CallState {
	h.mu.Lock()
	defer h.mu.Unlock()
	if m := h.subs[s.UserID]; m != nil {
		delete(m, s.Peer)
		if len(m) == 0 {
			delete(h.subs, s.UserID)
		}
	}
	delete(h.peers, s.Peer)
	h.closeLocked(s)
	var changed []CallState
	for room, c := range h.calls {
		if _, ok := c.Participants[s.Peer]; ok {
			delete(c.Participants, s.Peer)
			changed = append(changed, h.stateLocked(room))
		}
	}
	return changed
}

func (h *Hub) closeLocked(s *Sub) {
	if !s.closed {
		s.closed = true
		close(s.C)
	}
}

func (h *Hub) sendLocked(s *Sub, b []byte) {
	if s.closed {
		return
	}
	select {
	case s.C <- b:
	default:
		// клиент не успевает читать — закрываем поток, браузер переподключится и перечитает состояние
		h.closeLocked(s)
	}
}

// Close закрывает все потоки событий (остановка сервера): иначе http.Server.Shutdown
// ждал бы бесконечные SSE-соединения до таймаута. Браузеры переподключатся сами.
func (h *Hub) Close() {
	h.mu.Lock()
	defer h.mu.Unlock()
	for _, s := range h.peers {
		h.closeLocked(s)
	}
}

// Publish отправляет событие всем вкладкам перечисленных пользователей.
func (h *Hub) Publish(users []int64, e Event) {
	b := frame(e)
	h.mu.Lock()
	defer h.mu.Unlock()
	for _, u := range users {
		for _, s := range h.subs[u] {
			h.sendLocked(s, b)
		}
	}
}

// SendPeer — событие одной вкладке (WebRTC-сигнал). false — такой вкладки нет.
func (h *Hub) SendPeer(peer string, e Event) bool {
	h.mu.Lock()
	defer h.mu.Unlock()
	s, ok := h.peers[peer]
	if !ok {
		return false
	}
	h.sendLocked(s, frame(e))
	return true
}

// Online — есть ли у пользователя открытые вкладки.
func (h *Hub) Online(uid int64) bool {
	h.mu.Lock()
	defer h.mu.Unlock()
	return len(h.subs[uid]) > 0
}

// PeerOf — подключение с этим id, если оно принадлежит uid.
func (h *Hub) PeerOf(uid int64, peer string) (*Sub, bool) {
	h.mu.Lock()
	defer h.mu.Unlock()
	s, ok := h.peers[peer]
	if !ok || s.UserID != uid {
		return nil, false
	}
	return s, true
}

// ---------- звонки ----------

func (h *Hub) stateLocked(room string) CallState {
	st := CallState{Room: room, Participants: []Participant{}}
	c := h.calls[room]
	if c == nil || len(c.Participants) == 0 {
		return st
	}
	st.Active = true
	t := c.Started
	st.StartedAt = &t
	for _, p := range c.Participants {
		st.Participants = append(st.Participants, p)
	}
	sort.Slice(st.Participants, func(i, j int) bool { return st.Participants[i].Peer < st.Participants[j].Peer })
	return st
}

// State — текущее состояние звонка в комнате.
func (h *Hub) State(room string) CallState {
	h.mu.Lock()
	defer h.mu.Unlock()
	return h.stateLocked(room)
}

// ActiveRooms — комнаты из списка, в которых сейчас идёт звонок.
func (h *Hub) ActiveRooms(rooms []string) []CallState {
	h.mu.Lock()
	defer h.mu.Unlock()
	var out []CallState
	for _, r := range rooms {
		if c := h.calls[r]; c != nil && len(c.Participants) > 0 {
			out = append(out, h.stateLocked(r))
		}
	}
	return out
}

// Join добавляет вкладку peer пользователя uid в звонок комнаты.
// started — звонок только что начался (первый участник). others — кто уже был в звонке.
func (h *Hub) Join(uid int64, room, peer string) (st CallState, others []Participant, started bool, err error) {
	h.mu.Lock()
	defer h.mu.Unlock()
	s, ok := h.peers[peer]
	if !ok || s.UserID != uid {
		return st, nil, false, ErrPeer
	}
	c := h.calls[room]
	if c == nil || len(c.Participants) == 0 {
		c = &Call{Room: room, Started: time.Now(), Participants: map[string]Participant{}}
		h.calls[room] = c
		started = true
	}
	if _, already := c.Participants[peer]; !already && len(c.Participants) >= MaxCallPeers {
		return st, nil, false, ErrCallFull
	}
	for p, x := range c.Participants {
		if p != peer {
			others = append(others, x)
		}
	}
	c.Participants[peer] = Participant{Peer: peer, UserID: uid, Username: s.Username}
	return h.stateLocked(room), others, started, nil
}

// Leave выводит вкладку из звонка. ended — звонок опустел; duration — сколько он шёл.
func (h *Hub) Leave(uid int64, room, peer string) (st CallState, ended bool, duration time.Duration) {
	h.mu.Lock()
	defer h.mu.Unlock()
	c := h.calls[room]
	if c == nil {
		return h.stateLocked(room), false, 0
	}
	if p, ok := c.Participants[peer]; ok && p.UserID == uid {
		delete(c.Participants, peer)
	}
	if len(c.Participants) == 0 {
		delete(h.calls, room)
		return h.stateLocked(room), true, time.Since(c.Started)
	}
	return h.stateLocked(room), false, 0
}

// EndIfEmpty — после отключения вкладки: если звонок опустел, убрать его и вернуть длительность.
func (h *Hub) EndIfEmpty(room string) (bool, time.Duration) {
	h.mu.Lock()
	defer h.mu.Unlock()
	c := h.calls[room]
	if c == nil || len(c.Participants) > 0 {
		return false, 0
	}
	delete(h.calls, room)
	return true, time.Since(c.Started)
}

// InCall — участвуют ли обе вкладки в звонке комнаты (сигналы только между участниками).
func (h *Hub) InCall(room string, peers ...string) bool {
	h.mu.Lock()
	defer h.mu.Unlock()
	c := h.calls[room]
	if c == nil {
		return false
	}
	for _, p := range peers {
		if _, ok := c.Participants[p]; !ok {
			return false
		}
	}
	return true
}

// KickUser выводит все вкладки пользователя из звонка комнаты (его исключили или он вышел из комнаты).
func (h *Hub) KickUser(uid int64, room string) (CallState, bool) {
	h.mu.Lock()
	defer h.mu.Unlock()
	c := h.calls[room]
	if c == nil {
		return h.stateLocked(room), false
	}
	changed := false
	for p, x := range c.Participants {
		if x.UserID == uid {
			delete(c.Participants, p)
			changed = true
		}
	}
	return h.stateLocked(room), changed
}
