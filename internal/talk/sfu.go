package talk

import (
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log"
	"net"
	"strconv"
	"sync"
	"time"

	"github.com/pion/interceptor"
	"github.com/pion/rtcp"
	"github.com/pion/webrtc/v4"
)

// SFU — медиасервер для звонков (Selective Forwarding Unit) на Pion.
//
// В режиме «каждый с каждым» участник отправляет своё видео каждому другому — больше 6 человек
// не вытянуть. С SFU у участника одно соединение с сервером: он отправляет один поток, а сервер
// пересылает его остальным без перекодирования (RTP как есть). Сигнализация — та же, что для mesh
// (offer/answer/ice через SSE), только адресат — "sfu".
//
// Пересогласование: при входе и выходе участников сервер добавляет/убирает дорожки и отправляет
// остальным новый offer. Пока на предыдущий offer нет ответа, следующий откладывается — иначе
// соединение сломалось бы на «have-local-offer».
type SFU struct {
	api    *webrtc.API
	send   func(peer string, e Event) bool
	ice    []webrtc.ICEServer
	mux    net.PacketConn
	mu     sync.Mutex
	rooms  map[string]*sfuRoom
	closed bool
}

// SFUPeerID — адресат сигналов, обращённых к медиасерверу.
const SFUPeerID = "sfu"

// MaxSFUPeers — участников в звонке через SFU.
const MaxSFUPeers = 25

const maxEarlyCandidates = 128

var errSFUClosed = errors.New("медиасервер закрыт")

type SFUConfig struct {
	UDPPort   int      // весь медиатрафик через один UDP-порт (0 — случайные порты, для тестов)
	PublicIPs []string // внешние адреса сервера, если он за NAT (подставляются в ICE-кандидаты)
}

type sfuRoom struct {
	mu     sync.Mutex
	peers  map[string]*sfuPeer
	tracks map[string]*sfuTrack // id дорожки → пересылаемая дорожка
	closed bool
}

type sfuTrack struct {
	local  *webrtc.TrackLocalStaticRTP
	owner  string // peer-источник
	kind   webrtc.RTPCodecType
	ssrc   webrtc.SSRC
	stopCh chan struct{}
}

type sfuPeer struct {
	id      string
	pc      *webrtc.PeerConnection
	senders map[string]*webrtc.RTPSender // id чужой дорожки → отправитель этому участнику

	negMu       sync.Mutex
	ready       bool // initial answer has been sent before server renegotiation
	negotiating bool // отправлен offer, ждём answer
	pending     bool // за это время состав дорожек снова изменился
	early       []webrtc.ICECandidateInit
}

// NewSFU готовит медиасервер. send — доставка события конкретной вкладке (Hub.SendPeer).
func NewSFU(cfg SFUConfig, send func(peer string, e Event) bool) (*SFU, error) {
	m := &webrtc.MediaEngine{}
	if err := m.RegisterDefaultCodecs(); err != nil {
		return nil, err
	}
	reg := &interceptor.Registry{}
	if err := webrtc.RegisterDefaultInterceptors(m, reg); err != nil {
		return nil, err
	}
	se := webrtc.SettingEngine{}
	var mux net.PacketConn
	if cfg.UDPPort > 0 {
		c, err := net.ListenUDP("udp", &net.UDPAddr{Port: cfg.UDPPort})
		if err != nil {
			return nil, fmt.Errorf("SFU: UDP-порт %d: %w", cfg.UDPPort, err)
		}
		mux = c
		se.SetICEUDPMux(webrtc.NewICEUDPMux(nil, c))
	}
	if len(cfg.PublicIPs) > 0 {
		se.SetNAT1To1IPs(cfg.PublicIPs, webrtc.ICECandidateTypeHost)
	}
	return &SFU{
		api:   webrtc.NewAPI(webrtc.WithMediaEngine(m), webrtc.WithInterceptorRegistry(reg), webrtc.WithSettingEngine(se)),
		send:  send,
		mux:   mux,
		rooms: map[string]*sfuRoom{},
	}, nil
}

func (s *SFU) Close() {
	s.mu.Lock()
	if s.closed {
		s.mu.Unlock()
		return
	}
	s.closed = true
	rooms := s.rooms
	s.rooms = map[string]*sfuRoom{}
	s.mu.Unlock()
	for _, r := range rooms {
		r.mu.Lock()
		r.closed = true
		peers := r.peers
		r.peers = map[string]*sfuPeer{}
		r.tracks = map[string]*sfuTrack{}
		r.mu.Unlock()
		for _, p := range peers {
			// The room has been detached; Leave can no longer look it up.
			p.pc.Close()
		}
	}
	if s.mux != nil {
		s.mux.Close()
	}
}

func (s *SFU) room(id string, create bool) *sfuRoom {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.closed {
		return nil
	}
	r := s.rooms[id]
	if r == nil && create {
		r = &sfuRoom{peers: map[string]*sfuPeer{}, tracks: map[string]*sfuTrack{}}
		s.rooms[id] = r
	}
	return r
}

func (s *SFU) signal(room, to, typ string, data any) {
	s.send(to, Event{Type: "signal", Data: map[string]any{"room": room, "from": SFUPeerID, "username": "", "type": typ, "data": data}})
}

// HandleSignal — сигнал от вкладки peer (участника звонка) медиасерверу.
func (s *SFU) HandleSignal(room, peer, typ string, raw json.RawMessage) error {
	switch typ {
	case "offer":
		var sd webrtc.SessionDescription
		if err := json.Unmarshal(raw, &sd); err != nil {
			return err
		}
		return s.offer(room, peer, sd)
	case "answer":
		var sd webrtc.SessionDescription
		if err := json.Unmarshal(raw, &sd); err != nil {
			return err
		}
		return s.answer(room, peer, sd)
	case "ice":
		var c webrtc.ICECandidateInit
		if err := json.Unmarshal(raw, &c); err != nil {
			return err
		}
		return s.candidate(room, peer, c)
	}
	return errors.New("неизвестный тип сигнала")
}

// offer — участник подключается: его первый offer с его дорожками.
func (s *SFU) offer(roomID, peerID string, sd webrtc.SessionDescription) error {
	r := s.room(roomID, true)
	if r == nil {
		return errSFUClosed
	}
	p, err := s.newPeer(roomID, r, peerID)
	if err != nil {
		return err
	}
	if err := p.pc.SetRemoteDescription(sd); err != nil {
		return err
	}
	s.flushCandidates(p)
	// дорожки остальных участников. В answer помещаются только медиалинии из offer клиента
	// (обычно одна аудио и одна видео), поэтому остальные дорожки уходят следующим offer от сервера.
	r.mu.Lock()
	added := 0
	for id, t := range r.tracks {
		if t.owner != peerID {
			if _, ok := p.senders[id]; !ok {
				s.addSender(p, id, t)
				added++
			}
		}
	}
	r.mu.Unlock()
	ans, err := p.pc.CreateAnswer(nil)
	if err != nil {
		return err
	}
	if err := p.pc.SetLocalDescription(ans); err != nil {
		return err
	}
	s.signal(roomID, peerID, "answer", p.pc.LocalDescription())
	p.negMu.Lock()
	p.ready = true
	again := added > 0 || p.pending
	p.pending = false
	p.negMu.Unlock()
	if again {
		s.renegotiate(roomID, p)
	}
	s.requestKeyframes(r, peerID)
	return nil
}

func (s *SFU) newPeer(roomID string, r *sfuRoom, peerID string) (*sfuPeer, error) {
	// ICE and offer requests can arrive concurrently. Reuse exactly one connection.
	r.mu.Lock()
	defer r.mu.Unlock()
	if r.closed {
		return nil, errSFUClosed
	}
	if p := r.peers[peerID]; p != nil {
		return p, nil
	}
	pc, err := s.api.NewPeerConnection(webrtc.Configuration{ICEServers: s.ice})
	if err != nil {
		return nil, err
	}
	p := &sfuPeer{id: peerID, pc: pc, senders: map[string]*webrtc.RTPSender{}}
	pc.OnICECandidate(func(c *webrtc.ICECandidate) {
		if c != nil {
			s.signal(roomID, peerID, "ice", c.ToJSON())
		}
	})
	pc.OnConnectionStateChange(func(st webrtc.PeerConnectionState) {
		if st == webrtc.PeerConnectionStateFailed || st == webrtc.PeerConnectionStateClosed {
			go s.Leave(roomID, peerID)
		}
	})
	pc.OnTrack(func(remote *webrtc.TrackRemote, _ *webrtc.RTPReceiver) { s.forward(roomID, r, peerID, remote) })
	r.peers[peerID] = p
	return p, nil
}

// forward — входящая дорожка участника: создаём пересылаемую копию и раздаём остальным.
func (s *SFU) forward(roomID string, r *sfuRoom, owner string, remote *webrtc.TrackRemote) {
	// streamID = peer-источника: по нему браузер понимает, чьё это видео (плитка с именем)
	local, err := webrtc.NewTrackLocalStaticRTP(remote.Codec().RTPCodecCapability, owner+"-"+remote.ID(), owner)
	if err != nil {
		log.Printf("sfu: дорожка %s: %v", owner, err)
		return
	}
	t := &sfuTrack{local: local, owner: owner, kind: remote.Kind(), ssrc: remote.SSRC(), stopCh: make(chan struct{})}
	id := local.ID()
	r.mu.Lock()
	if r.closed || r.peers[owner] == nil {
		r.mu.Unlock()
		return
	}
	r.tracks[id] = t
	var targets []*sfuPeer
	for pid, p := range r.peers {
		if pid != owner {
			s.addSender(p, id, t)
			targets = append(targets, p)
		}
	}
	r.mu.Unlock()
	for _, p := range targets {
		s.renegotiate(roomID, p)
	}
	if t.kind == webrtc.RTPCodecTypeVideo {
		go s.keyframeLoop(r, t)
	}
	// Only the source ending stops forwarding, not a failed individual receiver.
	relayRTP(remote, local)
	close(t.stopCh)
	s.dropTrack(roomID, r, id)
}

type rtpReader interface {
	Read([]byte) (int, interceptor.Attributes, error)
}

func relayRTP(remote rtpReader, local io.Writer) {
	buf := make([]byte, 1500)
	for {
		n, _, err := remote.Read(buf)
		if err != nil {
			return
		}
		// Pion writes to every bound receiver and aggregates their errors. Healthy
		// receivers must keep receiving even when one transport is closing or fails.
		// Failed peers are removed by OnConnectionStateChange / Leave.
		_, _ = local.Write(buf[:n])
	}
}

// addSender — добавить чужую дорожку участнику p (r.mu должен быть захвачен).
func (s *SFU) addSender(p *sfuPeer, id string, t *sfuTrack) {
	if _, ok := p.senders[id]; ok {
		return
	}
	sender, err := p.pc.AddTrack(t.local)
	if err != nil {
		return
	}
	p.senders[id] = sender
	go func() { // RTCP от получателя (NACK, PLI) читает интерцептор; без чтения он встанет
		buf := make([]byte, 1500)
		for {
			if _, _, err := sender.Read(buf); err != nil {
				return
			}
		}
	}()
}

// dropTrack — источник ушёл: убираем его дорожку у всех и пересогласуем.
func (s *SFU) dropTrack(roomID string, r *sfuRoom, id string) {
	r.mu.Lock()
	if _, ok := r.tracks[id]; !ok {
		r.mu.Unlock()
		return
	}
	delete(r.tracks, id)
	var targets []*sfuPeer
	for _, p := range r.peers {
		if sender, ok := p.senders[id]; ok {
			p.pc.RemoveTrack(sender)
			delete(p.senders, id)
			targets = append(targets, p)
		}
	}
	r.mu.Unlock()
	for _, p := range targets {
		s.renegotiate(roomID, p)
	}
}

// renegotiate — новый offer участнику; если ответа на прошлый ещё нет — после него.
func (s *SFU) renegotiate(roomID string, p *sfuPeer) {
	p.negMu.Lock()
	if !p.ready || p.negotiating || p.pc.SignalingState() != webrtc.SignalingStateStable {
		p.pending = true
		p.negMu.Unlock()
		return
	}
	p.negotiating = true
	p.negMu.Unlock()
	offer, err := p.pc.CreateOffer(nil)
	if err == nil {
		err = p.pc.SetLocalDescription(offer)
	}
	if err != nil {
		p.negMu.Lock()
		p.negotiating = false
		p.negMu.Unlock()
		return
	}
	s.signal(roomID, p.id, "offer", p.pc.LocalDescription())
}

func (s *SFU) answer(roomID, peerID string, sd webrtc.SessionDescription) error {
	r := s.room(roomID, false)
	if r == nil {
		return errors.New("звонок не найден")
	}
	r.mu.Lock()
	p := r.peers[peerID]
	r.mu.Unlock()
	if p == nil {
		return errors.New("подключение к медиасерверу не найдено")
	}
	if err := p.pc.SetRemoteDescription(sd); err != nil {
		return err
	}
	s.flushCandidates(p)
	p.negMu.Lock()
	p.negotiating = false
	again := p.pending
	p.pending = false
	p.negMu.Unlock()
	if again {
		s.renegotiate(roomID, p)
	}
	s.requestKeyframes(r, peerID)
	return nil
}

func (s *SFU) candidate(roomID, peerID string, c webrtc.ICECandidateInit) error {
	r := s.room(roomID, true)
	if r == nil {
		return errSFUClosed
	}
	// The API checks call membership before accepting a signal, including ICE.
	// Create the connection early so candidates preceding the offer are retained.
	p, err := s.newPeer(roomID, r, peerID)
	if err != nil {
		return err
	}
	p.negMu.Lock()
	defer p.negMu.Unlock()
	if p.pc.RemoteDescription() == nil { // кандидат пришёл раньше offer — придержим
		if len(p.early) >= maxEarlyCandidates {
			return errors.New("слишком много ICE-кандидатов до offer")
		}
		p.early = append(p.early, c)
		return nil
	}
	return p.pc.AddICECandidate(c)
}

func (s *SFU) flushCandidates(p *sfuPeer) {
	p.negMu.Lock()
	early := p.early
	p.early = nil
	p.negMu.Unlock()
	for _, c := range early {
		p.pc.AddICECandidate(c)
	}
}

// requestKeyframes — новому зрителю нужен ключевой кадр, иначе видео не начнётся до следующего.
func (s *SFU) requestKeyframes(r *sfuRoom, except string) {
	r.mu.Lock()
	defer r.mu.Unlock()
	for _, t := range r.tracks {
		if t.kind != webrtc.RTPCodecTypeVideo || t.owner == except {
			continue
		}
		if src := r.peers[t.owner]; src != nil {
			src.pc.WriteRTCP([]rtcp.Packet{&rtcp.PictureLossIndication{MediaSSRC: uint32(t.ssrc)}})
		}
	}
}

// keyframeLoop — ключевой кадр раз в 3 с: после потери пакетов видео восстановится быстро.
func (s *SFU) keyframeLoop(r *sfuRoom, t *sfuTrack) {
	tick := time.NewTicker(3 * time.Second)
	defer tick.Stop()
	for {
		select {
		case <-t.stopCh:
			return
		case <-tick.C:
			r.mu.Lock()
			src := r.peers[t.owner]
			r.mu.Unlock()
			if src == nil {
				return
			}
			src.pc.WriteRTCP([]rtcp.Packet{&rtcp.PictureLossIndication{MediaSSRC: uint32(t.ssrc)}})
		}
	}
}

// Leave — участник вышел из звонка (или вкладка закрылась): закрываем соединение и его дорожки.
func (s *SFU) Leave(roomID, peerID string) {
	r := s.room(roomID, false)
	if r == nil {
		return
	}
	r.mu.Lock()
	p := r.peers[peerID]
	delete(r.peers, peerID)
	var own []string
	for id, t := range r.tracks {
		if t.owner == peerID {
			own = append(own, id)
		}
	}
	empty := len(r.peers) == 0
	r.mu.Unlock()
	if p != nil {
		p.pc.Close()
	}
	for _, id := range own {
		s.dropTrack(roomID, r, id)
	}
	if empty {
		s.mu.Lock()
		if s.rooms[roomID] == r {
			r.mu.Lock()
			if len(r.peers) == 0 {
				delete(s.rooms, roomID)
			}
			r.mu.Unlock()
		}
		s.mu.Unlock()
	}
}

// Reconcile — оставить в медиасервере только тех, кто в звонке (исключённые, закрытые вкладки).
func (s *SFU) Reconcile(st CallState) {
	r := s.room(st.Room, false)
	if r == nil {
		return
	}
	in := map[string]bool{}
	for _, p := range st.Participants {
		in[p.Peer] = true
	}
	r.mu.Lock()
	var gone []string
	for id := range r.peers {
		if !in[id] {
			gone = append(gone, id)
		}
	}
	r.mu.Unlock()
	for _, id := range gone {
		s.Leave(st.Room, id)
	}
}

// Peers — число подключённых к медиасерверу (для тестов и метрик).
func (s *SFU) Peers(room string) int {
	r := s.room(room, false)
	if r == nil {
		return 0
	}
	r.mu.Lock()
	defer r.mu.Unlock()
	return len(r.peers)
}

// ParsePort — «7882» → 7882 (0 — выключено).
func ParsePort(s string) int {
	n, err := strconv.Atoi(s)
	if err != nil || n < 0 || n > 65535 {
		return 0
	}
	return n
}
