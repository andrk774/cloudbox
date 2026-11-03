package api

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"strconv"
	"strings"
	"time"

	"github.com/andrk774/cloudbox/internal/talk"
)

// ---------- поток событий (Server-Sent Events) ----------

// talkEvents держит открытое соединение и доставляет события чата и звонков этой вкладке.
// Первое событие — hello с id вкладки (peer), им клиент подписывает WebRTC-сигналы.
func (s *Server) talkEvents(w http.ResponseWriter, r *http.Request) {
	fl, ok := w.(http.Flusher)
	if !ok {
		writeErr(w, http.StatusInternalServerError, "потоковая передача не поддерживается")
		return
	}
	u := userFrom(r)
	sub := s.Talk.Hub.Subscribe(u.ID, u.Username)
	defer s.talkDisconnect(sub)

	h := w.Header()
	h.Set("Content-Type", "text/event-stream")
	h.Set("Cache-Control", "no-store")
	h.Set("X-Accel-Buffering", "no") // nginx: не буферизовать поток
	w.WriteHeader(http.StatusOK)

	write := func(b []byte) bool {
		if _, err := w.Write(b); err != nil {
			return false
		}
		fl.Flush()
		return true
	}
	if !write([]byte("retry: 3000\n\n")) {
		return
	}
	hello := map[string]any{"peer": sub.Peer, "unread": s.Talk.Unread(r.Context(), u.ID)}
	if rooms, err := s.Talk.Rooms(r.Context(), u.ID); err == nil {
		ids := make([]string, len(rooms))
		for i := range rooms {
			ids[i] = rooms[i].ID
		}
		hello["calls"] = s.Talk.Hub.ActiveRooms(ids)
	}
	if !write(eventFrame("hello", hello)) {
		return
	}

	ping := time.NewTicker(25 * time.Second) // держим соединение живым через прокси
	defer ping.Stop()
	for {
		select {
		case <-r.Context().Done():
			return
		case b, open := <-sub.C:
			if !open || !write(b) {
				return
			}
		case <-ping.C:
			if !write([]byte(": ping\n\n")) {
				return
			}
		}
	}
}

func eventFrame(typ string, data any) []byte {
	b, _ := json.Marshal(data)
	return []byte("event: " + typ + "\ndata: " + string(b) + "\n\n")
}

// talkDisconnect: вкладка закрылась — выводим её из звонков и сообщаем остальным.
func (s *Server) talkDisconnect(sub *talk.Sub) {
	ctx := context.Background()
	for _, st := range s.Talk.Hub.Unsubscribe(sub) {
		s.talkPublishRoom(ctx, st.Room, talk.Event{Type: "call", Data: st})
		if ended, d := s.Talk.Hub.EndIfEmpty(st.Room); ended {
			s.talkCallEnded(ctx, st.Room, d)
		}
	}
}

// talkPublishRoom рассылает событие всем участникам комнаты.
func (s *Server) talkPublishRoom(ctx context.Context, room string, e talk.Event) {
	ids, err := s.Talk.MemberIDs(ctx, room)
	if err != nil {
		return
	}
	s.Talk.Hub.Publish(ids, e)
}

func (s *Server) talkSystem(ctx context.Context, room string, uid *int64, body string) {
	if m, err := s.Talk.System(ctx, room, uid, body); err == nil {
		s.talkPublishRoom(ctx, room, talk.Event{Type: "message", Data: m})
	}
}

func (s *Server) talkCallEnded(ctx context.Context, room string, d time.Duration) {
	s.talkSystem(ctx, room, nil, "Звонок завершён · "+fmtDuration(d))
}

func fmtDuration(d time.Duration) string {
	d = d.Round(time.Second)
	if d < time.Minute {
		return fmt.Sprintf("%d с", int(d.Seconds()))
	}
	if d < time.Hour {
		return fmt.Sprintf("%d мин", int(d.Minutes()))
	}
	return fmt.Sprintf("%d ч %d мин", int(d.Hours()), int(d.Minutes())%60)
}

// ---------- комнаты ----------

func (s *Server) talkConfig(w http.ResponseWriter, r *http.Request) {
	writeJSON(w, 200, map[string]any{
		"ice_servers":    s.ICE.Servers(userFrom(r).ID, time.Now()),
		"max_call_peers": talk.MaxCallPeers,
		"max_body":       talk.MaxBody,
	})
}

func (s *Server) talkRooms(w http.ResponseWriter, r *http.Request) {
	rooms, err := s.Talk.Rooms(r.Context(), userFrom(r).ID)
	if err != nil {
		fail(w, err)
		return
	}
	for i := range rooms {
		st := s.Talk.Hub.State(rooms[i].ID)
		if st.Active {
			rooms[i].Call = &st
		}
	}
	writeJSON(w, 200, rooms)
}

func (s *Server) talkRoom(w http.ResponseWriter, r *http.Request) {
	room, err := s.Talk.Room(r.Context(), userFrom(r).ID, r.PathValue("id"))
	if err != nil {
		fail(w, err)
		return
	}
	if st := s.Talk.Hub.State(room.ID); st.Active {
		room.Call = &st
	}
	writeJSON(w, 200, room)
}

func (s *Server) talkCreateRoom(w http.ResponseWriter, r *http.Request) {
	var req struct {
		Name    string  `json:"name"`
		UserIDs []int64 `json:"user_ids"`
	}
	if err := readJSON(r, &req); err != nil {
		writeErr(w, 400, err.Error())
		return
	}
	u := userFrom(r)
	id, err := s.Talk.CreateGroup(r.Context(), u.ID, req.Name, req.UserIDs)
	if err != nil {
		fail(w, err)
		return
	}
	s.talkSystem(r.Context(), id, &u.ID, u.Username+" создал(а) комнату")
	s.talkPublishRoom(r.Context(), id, talk.Event{Type: "room", Data: map[string]string{"id": id}})
	s.talkRespondRoom(w, r, id, 201)
}

func (s *Server) talkDirect(w http.ResponseWriter, r *http.Request) {
	var req struct {
		UserID int64 `json:"user_id"`
	}
	if err := readJSON(r, &req); err != nil {
		writeErr(w, 400, err.Error())
		return
	}
	id, err := s.Talk.Direct(r.Context(), userFrom(r).ID, req.UserID)
	if err != nil {
		fail(w, err)
		return
	}
	s.talkRespondRoom(w, r, id, 200)
}

func (s *Server) talkRespondRoom(w http.ResponseWriter, r *http.Request, id string, code int) {
	room, err := s.Talk.Room(r.Context(), userFrom(r).ID, id)
	if err != nil {
		fail(w, err)
		return
	}
	writeJSON(w, code, room)
}

func (s *Server) talkRename(w http.ResponseWriter, r *http.Request) {
	var req struct {
		Name string `json:"name"`
	}
	if err := readJSON(r, &req); err != nil {
		writeErr(w, 400, err.Error())
		return
	}
	u, id := userFrom(r), r.PathValue("id")
	if err := s.Talk.Rename(r.Context(), u.ID, id, req.Name); err != nil {
		fail(w, err)
		return
	}
	s.talkSystem(r.Context(), id, &u.ID, u.Username+" переименовал(а) комнату в «"+strings.TrimSpace(req.Name)+"»")
	s.talkPublishRoom(r.Context(), id, talk.Event{Type: "room", Data: map[string]string{"id": id}})
	s.talkRespondRoom(w, r, id, 200)
}

func (s *Server) talkAddMembers(w http.ResponseWriter, r *http.Request) {
	var req struct {
		UserIDs []int64 `json:"user_ids"`
	}
	if err := readJSON(r, &req); err != nil {
		writeErr(w, 400, err.Error())
		return
	}
	u, id := userFrom(r), r.PathValue("id")
	added, err := s.Talk.AddMembers(r.Context(), u.ID, id, req.UserIDs)
	if err != nil {
		fail(w, err)
		return
	}
	if len(added) > 0 {
		names := make([]string, len(added))
		for i, a := range added {
			names[i] = s.Talk.Username(r.Context(), a)
		}
		s.talkSystem(r.Context(), id, &u.ID, u.Username+" добавил(а) "+strings.Join(names, ", "))
		s.talkPublishRoom(r.Context(), id, talk.Event{Type: "room", Data: map[string]string{"id": id}})
	}
	s.talkRespondRoom(w, r, id, 200)
}

func (s *Server) talkRemoveMember(w http.ResponseWriter, r *http.Request) {
	u, id := userFrom(r), r.PathValue("id")
	target := u.ID
	if v := r.PathValue("uid"); v != "me" {
		n, err := strconv.ParseInt(v, 10, 64)
		if err != nil {
			writeErr(w, 400, "неверный пользователь")
			return
		}
		target = n
	}
	name := s.Talk.Username(r.Context(), target)
	removed, err := s.Talk.RemoveMember(r.Context(), u.ID, id, target)
	if err != nil {
		fail(w, err)
		return
	}
	// исключённый больше не должен получать ни сообщения, ни медиапоток звонка
	if st, changed := s.Talk.Hub.KickUser(target, id); changed {
		s.talkPublishRoom(r.Context(), id, talk.Event{Type: "call", Data: st})
		s.Talk.Hub.Publish([]int64{target}, talk.Event{Type: "call", Data: talk.CallState{Room: id, Participants: []talk.Participant{}}})
		if ended, d := s.Talk.Hub.EndIfEmpty(id); ended && !removed {
			s.talkCallEnded(r.Context(), id, d)
		}
	}
	s.Talk.Hub.Publish([]int64{target}, talk.Event{Type: "room_removed", Data: map[string]string{"id": id}})
	if !removed {
		msg := name + " покинул(а) комнату"
		if target != u.ID {
			msg = u.Username + " исключил(а) " + name
		}
		s.talkSystem(r.Context(), id, &u.ID, msg)
		s.talkPublishRoom(r.Context(), id, talk.Event{Type: "room", Data: map[string]string{"id": id}})
	}
	okJSON(w)
}

// ---------- сообщения ----------

func (s *Server) talkMessages(w http.ResponseWriter, r *http.Request) {
	before, _ := strconv.ParseInt(r.URL.Query().Get("before"), 10, 64)
	limit, _ := strconv.Atoi(r.URL.Query().Get("limit"))
	msgs, err := s.Talk.Messages(r.Context(), userFrom(r).ID, r.PathValue("id"), before, limit)
	if err != nil {
		fail(w, err)
		return
	}
	writeJSON(w, 200, msgs)
}

func (s *Server) talkPost(w http.ResponseWriter, r *http.Request) {
	u := userFrom(r)
	if !s.lim.Allow("talk-msg|"+strconv.FormatInt(u.ID, 10), 60, time.Minute) {
		writeErr(w, http.StatusTooManyRequests, "слишком много сообщений, подождите минуту")
		return
	}
	var req struct {
		Body string `json:"body"`
	}
	if err := readJSON(r, &req); err != nil {
		writeErr(w, 400, err.Error())
		return
	}
	id := r.PathValue("id")
	m, err := s.Talk.Post(r.Context(), u.ID, id, req.Body)
	if err != nil {
		fail(w, err)
		return
	}
	s.talkPublishRoom(r.Context(), id, talk.Event{Type: "message", Data: m})
	writeJSON(w, 201, m)
}

func (s *Server) talkRead(w http.ResponseWriter, r *http.Request) {
	var req struct {
		LastID int64 `json:"last_id"`
	}
	if err := readJSON(r, &req); err != nil {
		writeErr(w, 400, err.Error())
		return
	}
	u, id := userFrom(r), r.PathValue("id")
	if err := s.Talk.MarkRead(r.Context(), u.ID, id, req.LastID); err != nil {
		fail(w, err)
		return
	}
	// другие вкладки того же пользователя снимут значок непрочитанного
	s.Talk.Hub.Publish([]int64{u.ID}, talk.Event{Type: "read", Data: map[string]any{"room": id, "last_id": req.LastID}})
	okJSON(w)
}

// ---------- звонки ----------

type peerReq struct {
	Peer string          `json:"peer"`
	To   string          `json:"to"`
	Type string          `json:"type"`
	Data json.RawMessage `json:"data"`
}

func (s *Server) talkJoin(w http.ResponseWriter, r *http.Request) {
	var req peerReq
	if err := readJSON(r, &req); err != nil {
		writeErr(w, 400, err.Error())
		return
	}
	u, id := userFrom(r), r.PathValue("id")
	if ok, err := s.Talk.IsMember(r.Context(), u.ID, id); err != nil || !ok {
		fail(w, talk.ErrNotFound)
		return
	}
	st, others, started, err := s.Talk.Hub.Join(u.ID, id, req.Peer)
	if err != nil {
		writeErr(w, http.StatusConflict, err.Error())
		return
	}
	if started {
		s.talkSystem(r.Context(), id, &u.ID, u.Username+" начал(а) звонок")
	}
	s.talkPublishRoom(r.Context(), id, talk.Event{Type: "call", Data: st})
	if others == nil {
		others = []talk.Participant{}
	}
	writeJSON(w, 200, map[string]any{"participants": others, "ice_servers": s.ICE.Servers(u.ID, time.Now())})
}

func (s *Server) talkLeave(w http.ResponseWriter, r *http.Request) {
	var req peerReq
	if err := readJSON(r, &req); err != nil {
		writeErr(w, 400, err.Error())
		return
	}
	u, id := userFrom(r), r.PathValue("id")
	st, ended, d := s.Talk.Hub.Leave(u.ID, id, req.Peer)
	s.talkPublishRoom(r.Context(), id, talk.Event{Type: "call", Data: st})
	if ended {
		s.talkCallEnded(r.Context(), id, d)
	}
	okJSON(w)
}

// talkSignal пересылает WebRTC-сигнал (offer/answer/ice) одной вкладке.
// Сигналы ходят только между участниками одного звонка: посторонний не может подсунуть
// свой offer и подключиться к чужому медиапотоку.
func (s *Server) talkSignal(w http.ResponseWriter, r *http.Request) {
	var req peerReq
	if err := readJSON(r, &req); err != nil {
		writeErr(w, 400, err.Error())
		return
	}
	switch req.Type {
	case "offer", "answer", "ice":
	default:
		writeErr(w, 400, "неизвестный тип сигнала")
		return
	}
	if len(req.Data) > 64<<10 {
		writeErr(w, http.StatusRequestEntityTooLarge, "сигнал слишком большой")
		return
	}
	u, id := userFrom(r), r.PathValue("id")
	if _, ok := s.Talk.Hub.PeerOf(u.ID, req.Peer); !ok {
		writeErr(w, http.StatusConflict, talk.ErrPeer.Error())
		return
	}
	if !s.Talk.Hub.InCall(id, req.Peer, req.To) {
		writeErr(w, http.StatusConflict, talk.ErrNotInCall.Error())
		return
	}
	s.Talk.Hub.SendPeer(req.To, talk.Event{Type: "signal", Data: map[string]any{
		"room": id, "from": req.Peer, "user_id": u.ID, "username": u.Username, "type": req.Type, "data": req.Data,
	}})
	okJSON(w)
}
