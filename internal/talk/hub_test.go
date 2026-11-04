package talk

import (
	"crypto/hmac"
	"crypto/sha1"
	"encoding/base64"
	"strings"
	"testing"
	"time"
)

func TestHubCallLifecycle(t *testing.T) {
	h := NewHub()
	a1 := h.Subscribe(1, "alice")
	a2 := h.Subscribe(1, "alice") // вторая вкладка того же пользователя
	b := h.Subscribe(2, "bob")
	const room = "r1"

	// чужой peer не может войти от имени другого пользователя
	if _, _, _, err := h.Join(2, room, a1.Peer); err != ErrPeer {
		t.Fatalf("join foreign peer: %v", err)
	}
	st, others, started, err := h.Join(1, room, a1.Peer)
	if err != nil || !started || len(others) != 0 || !st.Active || len(st.Participants) != 1 {
		t.Fatalf("first join: %+v %v %v %v", st, others, started, err)
	}
	_, others, started, err = h.Join(2, room, b.Peer)
	if err != nil || started || len(others) != 1 || others[0].Peer != a1.Peer {
		t.Fatalf("second join: %v %v %v", others, started, err)
	}
	if !h.InCall(room, a1.Peer, b.Peer) {
		t.Fatal("both should be in call")
	}
	if h.InCall(room, a1.Peer, a2.Peer) {
		t.Fatal("a2 is not in call")
	}

	// закрытие вкладки выводит её из звонка
	changed := h.Unsubscribe(b)
	if len(changed) != 1 || len(changed[0].Participants) != 1 {
		t.Fatalf("unsubscribe: %+v", changed)
	}
	if ended, _ := h.EndIfEmpty(room); ended {
		t.Fatal("call with a participant must not end")
	}
	if _, ended, d := h.Leave(1, room, a1.Peer); !ended || d < 0 {
		t.Fatal("last leave must end the call")
	}
	if h.State(room).Active {
		t.Fatal("call still active")
	}
}

func TestHubCallLimitAndKick(t *testing.T) {
	h := NewHub()
	const room = "r"
	var subs []*Sub
	for i := 0; i < MaxCallPeers; i++ {
		s := h.Subscribe(int64(i+1), "u")
		subs = append(subs, s)
		if _, _, _, err := h.Join(int64(i+1), room, s.Peer); err != nil {
			t.Fatal(err)
		}
	}
	extra := h.Subscribe(99, "late")
	if _, _, _, err := h.Join(99, room, extra.Peer); err != ErrCallFull {
		t.Fatalf("over limit: %v", err)
	}
	// повторный join уже участвующей вкладки не упирается в лимит
	if _, _, _, err := h.Join(1, room, subs[0].Peer); err != nil {
		t.Fatalf("rejoin: %v", err)
	}
	st, changed := h.KickUser(2, room)
	if !changed || len(st.Participants) != MaxCallPeers-1 {
		t.Fatalf("kick: %v %d", changed, len(st.Participants))
	}
	if h.InCall(room, subs[1].Peer) {
		t.Fatal("kicked user still in call")
	}
}

func TestHubDelivery(t *testing.T) {
	h := NewHub()
	a := h.Subscribe(1, "alice")
	b := h.Subscribe(2, "bob")
	h.Publish([]int64{1}, Event{Type: "message", Data: map[string]string{"body": "привет"}})
	select {
	case f := <-a.C:
		if !strings.HasPrefix(string(f), "event: message\ndata: ") || !strings.Contains(string(f), "привет") {
			t.Fatalf("frame: %q", f)
		}
	default:
		t.Fatal("alice got nothing")
	}
	select {
	case <-b.C:
		t.Fatal("bob must not get alice's event")
	default:
	}
	if !h.SendPeer(b.Peer, Event{Type: "signal", Data: 1}) || h.SendPeer("nope", Event{Type: "signal"}) {
		t.Fatal("SendPeer")
	}

	// медленный клиент: буфер переполнен — поток закрывается, а не блокирует сервер
	for i := 0; i < cap(a.C)+5; i++ {
		h.Publish([]int64{1}, Event{Type: "x", Data: i})
	}
	n := 0
	for range a.C {
		n++
	}
	if n != cap(a.C) {
		t.Fatalf("slow consumer: read %d", n)
	}
	h.Unsubscribe(a) // повторное закрытие не паникует
	h.Close()
	if _, open := <-b.C; open {
		// в буфере лежит signal — дочитываем и убеждаемся, что канал закрыт
		if _, open = <-b.C; open {
			t.Fatal("Close must close streams")
		}
	}
}

func TestICEServers(t *testing.T) {
	now := time.Unix(1_800_000_000, 0)
	if got := (ICEConfig{}).Servers(1, now); len(got) != 0 {
		t.Fatalf("empty config: %+v", got)
	}
	// TURN без секрета не отдаём: без учётных данных coturn его всё равно отвергнет
	if got := (ICEConfig{TURN: []string{"turn:x"}}).Servers(1, now); len(got) != 0 {
		t.Fatalf("turn without secret: %+v", got)
	}
	c := ICEConfig{STUN: []string{"stun:s:3478"}, TURN: []string{"turn:t:3478"}, Secret: "sekret", TTL: time.Hour}
	got := c.Servers(42, now)
	if len(got) != 2 || got[0].URLs[0] != "stun:s:3478" {
		t.Fatalf("servers: %+v", got)
	}
	wantUser := "1800003600:42"
	m := hmac.New(sha1.New, []byte("sekret"))
	m.Write([]byte(wantUser))
	if got[1].Username != wantUser || got[1].Credential != base64.StdEncoding.EncodeToString(m.Sum(nil)) {
		t.Fatalf("turn creds: %+v", got[1])
	}
	if l := SplitList(" stun:a , ,stun:b "); len(l) != 2 || l[1] != "stun:b" {
		t.Fatalf("SplitList: %v", l)
	}
}
