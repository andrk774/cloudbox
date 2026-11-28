package talk

import (
	"errors"
	"io"
	"sync"
	"testing"

	"github.com/pion/interceptor"
	"github.com/pion/rtp"
	"github.com/pion/webrtc/v4"
)

func lifecycleSFU(t *testing.T) *SFU {
	t.Helper()
	s, err := NewSFU(SFUConfig{}, func(string, Event) bool { return true })
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(s.Close)
	return s
}

func TestSFUCloseClosesConnectionsAndRejectsNewSignals(t *testing.T) {
	s := lifecycleSFU(t)
	var peers []*sfuPeer
	for _, room := range []string{"one", "two"} {
		r := s.room(room, true)
		for _, id := range []string{"a", "b"} {
			p, err := s.newPeer(room, r, id)
			if err != nil {
				t.Fatal(err)
			}
			peers = append(peers, p)
		}
	}
	stale := s.room("one", false)
	s.Close()
	s.Close() // safe to call again during teardown
	for _, p := range peers {
		if p.pc.ConnectionState() != webrtc.PeerConnectionStateClosed {
			t.Fatalf("connection %s remains %s", p.id, p.pc.ConnectionState())
		}
	}
	if s.Peers("one") != 0 || s.room("new", true) != nil {
		t.Fatal("closed server still exposes or creates rooms")
	}
	if _, err := s.newPeer("one", stale, "late"); !errors.Is(err, errSFUClosed) {
		t.Fatalf("stale room accepted new connection: %v", err)
	}
	if err := s.HandleSignal("late", "p", "offer", raw(webrtc.SessionDescription{Type: webrtc.SDPTypeOffer})); !errors.Is(err, errSFUClosed) {
		t.Fatalf("offer after close: %v", err)
	}
	if err := s.candidate("late", "p", webrtc.ICECandidateInit{}); !errors.Is(err, errSFUClosed) {
		t.Fatalf("ICE after close: %v", err)
	}
}

func TestSFUEarlyICEArrivesBeforeOfferAndIsFlushed(t *testing.T) {
	s := lifecycleSFU(t)
	const room, peer = "room", "peer"
	c := webrtc.ICECandidateInit{Candidate: "candidate:1 1 udp 2130706431 127.0.0.1 50000 typ host"}
	if err := s.HandleSignal(room, peer, "ice", raw(c)); err != nil {
		t.Fatal(err)
	}
	r := s.room(room, false)
	p, err := s.newPeer(room, r, peer)
	if err != nil {
		t.Fatal(err)
	}
	if len(p.early) != 1 || p.early[0].Candidate != c.Candidate {
		t.Fatal("ICE preceding the first offer was lost")
	}
	client, err := webrtc.NewPeerConnection(webrtc.Configuration{})
	if err != nil {
		t.Fatal(err)
	}
	defer client.Close()
	if _, err := client.AddTransceiverFromKind(webrtc.RTPCodecTypeVideo); err != nil {
		t.Fatal(err)
	}
	offer, err := client.CreateOffer(nil)
	if err != nil {
		t.Fatal(err)
	}
	if err := s.HandleSignal(room, peer, "offer", raw(offer)); err != nil {
		t.Fatal(err)
	}
	if r.peers[peer] != p || len(p.early) != 0 {
		t.Fatal("offer replaced the ICE connection or failed to flush its queue")
	}
	hasCandidate := func(port int32) bool {
		for _, stat := range p.pc.GetStats() {
			if c, ok := stat.(webrtc.ICECandidateStats); ok && c.Type == webrtc.StatsTypeRemoteCandidate && c.Port == port {
				return true
			}
		}
		return false
	}
	waitFor(t, "queued candidate is passed to ICE", func() bool { return hasCandidate(50000) })
	if err := s.candidate(room, peer, webrtc.ICECandidateInit{Candidate: "candidate:2 1 udp 2130706430 127.0.0.1 50001 typ host"}); err != nil {
		t.Fatal(err)
	}
	waitFor(t, "candidate after offer is passed to ICE", func() bool { return hasCandidate(50001) })
}

func TestSFUConcurrentEarlyICEReusesOnePeerAndLeaveClearsIt(t *testing.T) {
	s := lifecycleSFU(t)
	var wg sync.WaitGroup
	for i := 0; i < 24; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			if err := s.candidate("room", "peer", webrtc.ICECandidateInit{}); err != nil {
				t.Error(err)
			}
		}()
	}
	wg.Wait()
	r := s.room("room", false)
	p := r.peers["peer"]
	if s.Peers("room") != 1 || len(p.early) != 24 {
		t.Fatal("parallel ICE requests replaced a connection or lost candidates")
	}
	for i := 24; i < maxEarlyCandidates; i++ {
		if err := s.candidate("room", "peer", webrtc.ICECandidateInit{}); err != nil {
			t.Fatal(err)
		}
	}
	if err := s.candidate("room", "peer", webrtc.ICECandidateInit{}); err == nil {
		t.Fatal("early candidate queue must be bounded")
	}
	s.Reconcile(CallState{Room: "room"})
	if s.Peers("room") != 0 || p.pc.ConnectionState() != webrtc.PeerConnectionStateClosed {
		t.Fatal("leaving before the offer must close the pending connection")
	}
}

type packetSource struct{ next uint16 }

func (s *packetSource) Read(buf []byte) (int, interceptor.Attributes, error) {
	if s.next == 5 {
		return 0, nil, io.EOF
	}
	s.next++
	p := rtp.Packet{Header: rtp.Header{Version: 2, PayloadType: 96, SequenceNumber: s.next}, Payload: []byte{1, 2, 3}}
	b, err := p.Marshal()
	return copy(buf, b), nil, err
}

type packetSink struct {
	err error
	got []uint16
}

func (w *packetSink) WriteRTP(h *rtp.Header, p []byte) (int, error) {
	if w.err != nil {
		return 0, w.err
	}
	w.got = append(w.got, h.SequenceNumber)
	return len(p), nil
}
func (w *packetSink) Write(b []byte) (int, error) { return len(b), w.err }

type packetBinding struct {
	id string
	w  *packetSink
}

func (b packetBinding) ID() string                                           { return b.id }
func (b packetBinding) WriteStream() webrtc.TrackLocalWriter                 { return b.w }
func (packetBinding) SSRC() webrtc.SSRC                                      { return 123 }
func (packetBinding) SSRCRetransmission() webrtc.SSRC                        { return 0 }
func (packetBinding) SSRCForwardErrorCorrection() webrtc.SSRC                { return 0 }
func (packetBinding) RTCPReader() interceptor.RTCPReader                     { return nil }
func (packetBinding) HeaderExtensions() []webrtc.RTPHeaderExtensionParameter { return nil }
func (packetBinding) CodecParameters() []webrtc.RTPCodecParameters {
	return []webrtc.RTPCodecParameters{{RTPCodecCapability: webrtc.RTPCodecCapability{MimeType: webrtc.MimeTypeVP8, ClockRate: 90000}, PayloadType: 96}}
}

func TestSFUFailedReceiverDoesNotStopOtherReceivers(t *testing.T) {
	for _, receiverErr := range []error{io.ErrClosedPipe, errors.New("transport failed")} {
		t.Run(receiverErr.Error(), func(t *testing.T) {
			track, err := webrtc.NewTrackLocalStaticRTP(webrtc.RTPCodecCapability{MimeType: webrtc.MimeTypeVP8, ClockRate: 90000}, "video", "owner")
			if err != nil {
				t.Fatal(err)
			}
			dead, healthy := &packetSink{err: receiverErr}, &packetSink{}
			for _, binding := range []packetBinding{{"dead", dead}, {"healthy", healthy}} {
				if _, err := track.Bind(binding); err != nil {
					t.Fatal(err)
				}
			}
			// Real Pion fan-out returns an error for the dead transport on every packet.
			relayRTP(&packetSource{}, track)
			if len(healthy.got) != 5 || healthy.got[4] != 5 {
				t.Fatalf("healthy receiver lost the rest of the source: %v", healthy.got)
			}
		})
	}
}
