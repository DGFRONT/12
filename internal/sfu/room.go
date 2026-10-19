package sfu

import (
	"errors"
	"io"
	"sync"
	"sync/atomic"
	"time"

	"github.com/pion/rtcp"
	"github.com/pion/webrtc/v4"
)

// Signal delivers a signaling message ("rtc.offer", "rtc.candidate") to one participant.
// It is called from a dedicated goroutine, never with the room's media lock held.
type Signal func(typ string, data any)

type Room struct {
	sfu *SFU

	mu     sync.Mutex
	peers  map[string]*peer
	tracks map[string]*forward // by local track ID
	retry  *time.Timer
	closed bool
}

type peer struct {
	id   string
	pc   *webrtc.PeerConnection
	out  chan outMsg
	done chan struct{}

	gateAudio atomic.Bool
	gateVideo atomic.Bool

	needsOffer bool // a renegotiation waits for the answer to the current offer
}

type outMsg struct {
	typ  string
	data any
}

// forward relays one published track to every other participant.
type forward struct {
	owner string
	kind  webrtc.RTPCodecType
	local *webrtc.TrackLocalStaticRTP
	ssrc  webrtc.SSRC
	pc    *webrtc.PeerConnection // publisher's connection, for keyframe requests
}

func (s *SFU) NewRoom() *Room {
	return &Room{sfu: s, peers: map[string]*peer{}, tracks: map[string]*forward{}}
}

func (p *peer) send(typ string, data any) {
	select {
	case <-p.done:
	case p.out <- outMsg{typ, data}:
	default:
		// The participant's signaling queue is full; its WebSocket is about to be dropped anyway.
	}
}

// AddPeer creates the server side of a participant's connection and sends the first offer.
func (r *Room) AddPeer(id string, signal Signal) error {
	pc, err := r.sfu.api.NewPeerConnection(webrtc.Configuration{})
	if err != nil {
		return err
	}
	// Slots for the participant's own microphone and camera.
	for _, kind := range []webrtc.RTPCodecType{webrtc.RTPCodecTypeAudio, webrtc.RTPCodecTypeVideo} {
		if _, err := pc.AddTransceiverFromKind(kind, webrtc.RTPTransceiverInit{Direction: webrtc.RTPTransceiverDirectionRecvonly}); err != nil {
			pc.Close()
			return err
		}
	}
	p := &peer{id: id, pc: pc, out: make(chan outMsg, 128), done: make(chan struct{})}
	go func() {
		for {
			select {
			case <-p.done:
				return
			case m := <-p.out:
				signal(m.typ, m.data)
			}
		}
	}()
	pc.OnICECandidate(func(c *webrtc.ICECandidate) {
		if c != nil {
			p.send("rtc.candidate", c.ToJSON())
		}
	})
	pc.OnTrack(func(remote *webrtc.TrackRemote, _ *webrtc.RTPReceiver) { r.relay(p, remote) })

	r.mu.Lock()
	defer r.mu.Unlock()
	if r.closed {
		close(p.done)
		go pc.Close()
		return errors.New("room is closed")
	}
	if old := r.peers[id]; old != nil {
		r.dropPeerLocked(old)
	}
	r.peers[id] = p
	r.signalLocked()
	return nil
}

// RemovePeer closes a participant's connection and stops relaying its tracks.
func (r *Room) RemovePeer(id string) {
	r.mu.Lock()
	defer r.mu.Unlock()
	if p := r.peers[id]; p != nil {
		r.dropPeerLocked(p)
		r.signalLocked()
	}
}

func (r *Room) dropPeerLocked(p *peer) {
	delete(r.peers, p.id)
	close(p.done)
	for id, f := range r.tracks {
		if f.owner == p.id {
			delete(r.tracks, id)
		}
	}
	go p.pc.Close()
}

// Close disconnects everybody.
func (r *Room) Close() {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.closed = true
	if r.retry != nil {
		r.retry.Stop()
	}
	for _, p := range r.peers {
		r.dropPeerLocked(p)
	}
}

// Answer applies the participant's answer to the last offer.
func (r *Room) Answer(id string, answer webrtc.SessionDescription) error {
	r.mu.Lock()
	defer r.mu.Unlock()
	p := r.peers[id]
	if p == nil {
		return errors.New("no media session")
	}
	if err := p.pc.SetRemoteDescription(answer); err != nil {
		return err
	}
	if p.needsOffer {
		p.needsOffer = false
		r.signalLocked()
	}
	return nil
}

func (r *Room) Candidate(id string, c webrtc.ICECandidateInit) error {
	r.mu.Lock()
	p := r.peers[id]
	r.mu.Unlock()
	if p == nil {
		return errors.New("no media session")
	}
	return p.pc.AddICECandidate(c)
}

// SetGate decides whether the participant's audio and video reach the others.
func (r *Room) SetGate(id string, audio, video bool) {
	r.mu.Lock()
	defer r.mu.Unlock()
	p := r.peers[id]
	if p == nil {
		return
	}
	p.gateAudio.Store(audio)
	if !p.gateVideo.Swap(video) && video {
		// Receivers need a keyframe to start decoding the newly opened video.
		for _, f := range r.tracks {
			if f.owner == id && f.kind == webrtc.RTPCodecTypeVideo {
				go requestKeyframe(f)
			}
		}
	}
}

func requestKeyframe(f *forward) {
	_ = f.pc.WriteRTCP([]rtcp.Packet{&rtcp.PictureLossIndication{MediaSSRC: uint32(f.ssrc)}})
}

// relay runs for each track a participant publishes: it copies RTP packets to a local
// track that every other participant receives, dropping them while the gate is closed.
func (r *Room) relay(p *peer, remote *webrtc.TrackRemote) {
	kind := remote.Kind()
	local, err := webrtc.NewTrackLocalStaticRTP(remote.Codec().RTPCodecCapability, kind.String()+"-"+p.id, "p-"+p.id)
	if err != nil {
		r.sfu.log.Error("create local track", "err", err)
		return
	}
	f := &forward{owner: p.id, kind: kind, local: local, ssrc: remote.SSRC(), pc: p.pc}

	r.mu.Lock()
	if r.peers[p.id] != p {
		r.mu.Unlock()
		return
	}
	r.tracks[local.ID()] = f
	r.signalLocked()
	r.mu.Unlock()

	gate := &p.gateAudio
	if kind == webrtc.RTPCodecTypeVideo {
		gate = &p.gateVideo
		go func() {
			// Periodic keyframes let late subscribers start quickly.
			t := time.NewTicker(3 * time.Second)
			defer t.Stop()
			for {
				select {
				case <-p.done:
					return
				case <-t.C:
					if gate.Load() {
						requestKeyframe(f)
					}
				}
			}
		}()
	}

	var dropped uint16 // keeps forwarded sequence numbers contiguous across closed-gate gaps
	for {
		pkt, _, err := remote.ReadRTP()
		if err != nil {
			break
		}
		if !gate.Load() {
			dropped++
			continue
		}
		pkt.SequenceNumber -= dropped
		if err := local.WriteRTP(pkt); err != nil && !errors.Is(err, io.ErrClosedPipe) {
			break
		}
	}

	r.mu.Lock()
	if r.tracks[local.ID()] == f {
		delete(r.tracks, local.ID())
		r.signalLocked()
	}
	r.mu.Unlock()
}

// signalLocked brings every connection's senders in line with the current tracks and
// sends new offers. Connections that are waiting for an answer are renegotiated later.
func (r *Room) signalLocked() {
	if r.closed {
		return
	}
	for attempt := 0; attempt < 25; attempt++ {
		if r.syncLocked() {
			return
		}
	}
	// Something kept failing (usually a connection mid-teardown); try again shortly.
	if r.retry == nil {
		r.retry = time.AfterFunc(3*time.Second, func() {
			r.mu.Lock()
			defer r.mu.Unlock()
			r.retry = nil
			r.signalLocked()
		})
	}
}

func (r *Room) syncLocked() bool {
	for _, p := range r.peers {
		if p.pc.ConnectionState() == webrtc.PeerConnectionStateClosed {
			continue
		}
		sending := map[string]bool{}
		for _, sender := range p.pc.GetSenders() {
			t := sender.Track()
			if t == nil {
				continue
			}
			sending[t.ID()] = true
			if _, ok := r.tracks[t.ID()]; !ok {
				if err := p.pc.RemoveTrack(sender); err != nil {
					return false
				}
			}
		}
		for id, f := range r.tracks {
			if f.owner != p.id && !sending[id] {
				// A dedicated send-only transceiver: AddTrack would reuse the participant's own
				// receive-only slot and leave them nowhere to send their media.
				if _, err := p.pc.AddTransceiverFromTrack(f.local, webrtc.RTPTransceiverInit{Direction: webrtc.RTPTransceiverDirectionSendonly}); err != nil {
					return false
				}
			}
		}
		if p.pc.SignalingState() != webrtc.SignalingStateStable {
			p.needsOffer = true
			continue
		}
		offer, err := p.pc.CreateOffer(nil)
		if err != nil {
			return false
		}
		if err := p.pc.SetLocalDescription(offer); err != nil {
			return false
		}
		p.send("rtc.offer", offer)
	}
	return true
}
