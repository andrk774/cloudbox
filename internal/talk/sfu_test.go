package talk

import (
	"encoding/json"
	"sync"
	"testing"
	"time"

	"github.com/pion/rtp"
	"github.com/pion/webrtc/v4"
)

// testClient — участник звонка на Pion вместо браузера: отправляет одну видеодорожку и
// записывает, какие чужие дорожки и сколько пакетов получил.
type testClient struct {
	id    string
	pc    *webrtc.PeerConnection
	track *webrtc.TrackLocalStaticRTP

	mu      sync.Mutex
	got     map[string]int // streamID (= peer-источник) → получено RTP-пакетов
	ended   map[string]bool
	offers  int
	pending []webrtc.ICECandidateInit
}

func (c *testClient) count(src string) int {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.got[src]
}

func (c *testClient) isEnded(src string) bool {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.ended[src]
}

func raw(v any) json.RawMessage { b, _ := json.Marshal(v); return b }

func newTestClient(t *testing.T, s *SFU, room, id string) *testClient {
	t.Helper()
	pc, err := webrtc.NewPeerConnection(webrtc.Configuration{})
	if err != nil {
		t.Fatal(err)
	}
	tr, err := webrtc.NewTrackLocalStaticRTP(webrtc.RTPCodecCapability{MimeType: webrtc.MimeTypeVP8}, "video", id)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := pc.AddTrack(tr); err != nil {
		t.Fatal(err)
	}
	c := &testClient{id: id, pc: pc, track: tr, got: map[string]int{}, ended: map[string]bool{}}
	pc.OnICECandidate(func(cand *webrtc.ICECandidate) {
		if cand != nil {
			s.HandleSignal(room, id, "ice", raw(cand.ToJSON()))
		}
	})
	pc.OnTrack(func(remote *webrtc.TrackRemote, _ *webrtc.RTPReceiver) {
		src := remote.StreamID()
		for {
			if _, _, err := remote.ReadRTP(); err != nil {
				c.mu.Lock()
				c.ended[src] = true
				c.mu.Unlock()
				return
			}
			c.mu.Lock()
			c.got[src]++
			c.mu.Unlock()
		}
	})
	t.Cleanup(func() { pc.Close() })
	return c
}

// join — первый offer клиента медиасерверу (как Call.join в браузере).
func (c *testClient) join(t *testing.T, s *SFU, room string) {
	offer, err := c.pc.CreateOffer(nil)
	if err != nil {
		t.Fatal(err)
	}
	if err := c.pc.SetLocalDescription(offer); err != nil {
		t.Fatal(err)
	}
	if err := s.HandleSignal(room, c.id, "offer", raw(c.pc.LocalDescription())); err != nil {
		t.Fatal(err)
	}
}

// onSignal — сигнал от медиасервера этому клиенту.
func (c *testClient) onSignal(s *SFU, room string, typ string, data any) {
	b, _ := json.Marshal(data)
	switch typ {
	case "answer", "offer":
		var sd webrtc.SessionDescription
		json.Unmarshal(b, &sd)
		if err := c.pc.SetRemoteDescription(sd); err != nil {
			return
		}
		c.mu.Lock()
		pend := c.pending
		c.pending = nil
		if typ == "offer" {
			c.offers++
		}
		c.mu.Unlock()
		for _, p := range pend {
			c.pc.AddICECandidate(p)
		}
		if typ == "offer" { // пересогласование: сервер добавил или убрал дорожки
			ans, _ := c.pc.CreateAnswer(nil)
			c.pc.SetLocalDescription(ans)
			s.HandleSignal(room, c.id, "answer", raw(c.pc.LocalDescription()))
		}
	case "ice":
		var ci webrtc.ICECandidateInit
		json.Unmarshal(b, &ci)
		c.mu.Lock()
		if c.pc.RemoteDescription() == nil {
			c.pending = append(c.pending, ci)
			c.mu.Unlock()
			return
		}
		c.mu.Unlock()
		c.pc.AddICECandidate(ci)
	}
}

// stream — клиент шлёт RTP, пока не закрыт stop.
func (c *testClient) stream(stop chan struct{}) {
	go func() {
		seq := uint16(0)
		tick := time.NewTicker(20 * time.Millisecond)
		defer tick.Stop()
		for {
			select {
			case <-stop:
				return
			case <-tick.C:
				seq++
				c.track.WriteRTP(&rtp.Packet{Header: rtp.Header{Version: 2, PayloadType: 96, SequenceNumber: seq, Timestamp: uint32(seq) * 3000, SSRC: 1},
					Payload: []byte{0x10, 0x00, 0x00, 0x9d, 0x01, 0x2a}})
			}
		}
	}()
}

func waitFor(t *testing.T, what string, cond func() bool) {
	t.Helper()
	end := time.Now().Add(15 * time.Second)
	for time.Now().Before(end) {
		if cond() {
			return
		}
		time.Sleep(50 * time.Millisecond)
	}
	t.Fatalf("не дождались: %s", what)
}

func TestSFUForwardsMedia(t *testing.T) {
	const room = "room-1"
	var mu sync.Mutex
	clients := map[string]*testClient{}
	var s *SFU
	type msg struct {
		peer string
		e    Event
	}
	ch := make(chan msg, 1000)
	s, err := NewSFU(SFUConfig{}, func(peer string, e Event) bool { ch <- msg{peer, e}; return true })
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(s.Close)
	done := make(chan struct{})
	t.Cleanup(func() { close(done) })
	go func() { // доставка сигналов (в настоящем сервере — SSE)
		for {
			select {
			case <-done:
				return
			case m := <-ch:
				d := m.e.Data.(map[string]any)
				if d["from"] != SFUPeerID || d["room"] != room {
					t.Errorf("сигнал не от медиасервера: %v", d)
					continue
				}
				mu.Lock()
				c := clients[m.peer]
				mu.Unlock()
				if c != nil {
					c.onSignal(s, room, d["type"].(string), d["data"])
				}
			}
		}
	}()
	add := func(id string) *testClient {
		c := newTestClient(t, s, room, id)
		mu.Lock()
		clients[id] = c
		mu.Unlock()
		c.join(t, s, room)
		return c
	}

	stopA, stopB := make(chan struct{}), make(chan struct{})
	a := add("peerA")
	a.stream(stopA)
	b := add("peerB")
	b.stream(stopB)
	waitFor(t, "B получает видео A через сервер", func() bool { return b.count("peerA") > 10 })
	waitFor(t, "A получает видео B через сервер", func() bool { return a.count("peerB") > 10 })
	if a.count("peerA") != 0 || b.count("peerB") != 0 {
		t.Fatal("участник не должен получать собственное видео обратно")
	}

	stopC := make(chan struct{})
	c := add("peerC") // вошёл позже — должен сразу получить обоих
	c.stream(stopC)
	waitFor(t, "C получает A и B", func() bool { return c.count("peerA") > 10 && c.count("peerB") > 10 })
	waitFor(t, "A и B получают видео C (сервер пересогласовал их соединения)", func() bool {
		return a.count("peerC") > 10 && b.count("peerC") > 10
	})
	if s.Peers(room) != 3 {
		t.Fatalf("в медиасервере %d участников, ждали 3", s.Peers(room))
	}

	close(stopA)
	s.Leave(room, "peerA")
	waitFor(t, "у B и C закончилась дорожка A", func() bool { return b.isEnded("peerA") && c.isEnded("peerA") })
	if s.Peers(room) != 2 {
		t.Fatalf("после выхода A осталось %d, ждали 2", s.Peers(room))
	}

	// исключённый из комнаты/закрывший вкладку участник убирается сверкой с составом звонка
	s.Reconcile(CallState{Room: room, Active: true, Participants: []Participant{{Peer: "peerC"}}})
	if s.Peers(room) != 1 {
		t.Fatalf("после сверки осталось %d, ждали 1", s.Peers(room))
	}
	close(stopB)
	close(stopC)
	s.Leave(room, "peerC")
	if s.Peers(room) != 0 {
		t.Fatal("пустой звонок должен удаляться из медиасервера")
	}
}

func TestSFURejectsGarbage(t *testing.T) {
	s, err := NewSFU(SFUConfig{}, func(string, Event) bool { return true })
	if err != nil {
		t.Fatal(err)
	}
	defer s.Close()
	if err := s.HandleSignal("r", "p", "evil", raw(1)); err == nil {
		t.Fatal("неизвестный тип сигнала")
	}
	if err := s.HandleSignal("r", "p", "offer", json.RawMessage(`{"type":"offer","sdp":"не sdp"}`)); err == nil {
		t.Fatal("битый SDP должен отклоняться")
	}
	if err := s.HandleSignal("r", "p", "answer", raw(webrtc.SessionDescription{Type: webrtc.SDPTypeAnswer, SDP: "x"})); err == nil {
		t.Fatal("answer без подключения должен отклоняться")
	}
}

// Another participant can publish while this peer has sent only early ICE.
// That must not turn the server into the initial offerer.
func TestSFUDefersOfferUntilInitialAnswer(t *testing.T) {
	var signals []string
	var mu sync.Mutex
	s, err := NewSFU(SFUConfig{}, func(_ string, e Event) bool {
		mu.Lock()
		defer mu.Unlock()
		signals = append(signals, e.Data.(map[string]any)["type"].(string))
		return true
	})
	if err != nil {
		t.Fatal(err)
	}
	defer s.Close()
	p, err := s.newPeer("room", s.room("room", true), "peer")
	if err != nil {
		t.Fatal(err)
	}
	s.renegotiate("room", p)
	if !p.pending || p.negotiating || p.pc.LocalDescription() != nil {
		t.Fatal("server started negotiation before the client's initial offer")
	}
	c := newTestClient(t, s, "room", "peer")
	c.join(t, s, "room")
	mu.Lock()
	defer mu.Unlock()
	var sdp []string
	for _, typ := range signals {
		if typ != "ice" {
			sdp = append(sdp, typ)
		}
	}
	if len(sdp) != 2 || sdp[0] != "answer" || sdp[1] != "offer" {
		t.Fatalf("expected initial answer then deferred offer, got %v", sdp)
	}
}
