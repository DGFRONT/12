package api

import (
	"encoding/json"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/pion/rtp"
	"github.com/pion/webrtc/v4"

	"github.com/dgfront/12/internal/hub"
)

// rtcClient is a minimal browser stand-in: it answers the server's offers, can publish an
// audio track (sending RTP regardless of permissions, like a modified client would) and
// counts the packets it receives per publishing participant.
type rtcClient struct {
	t        *testing.T
	ws       *wsClient
	pc       *webrtc.PeerConnection
	audio    *webrtc.TrackLocalStaticRTP
	mu       sync.Mutex
	received map[string]*atomic.Int64 // by stream ID "p-<participant id>"
	stop     chan struct{}
}

func newRTCClient(t *testing.T, base, slug, token, name string, publish bool) *rtcClient {
	t.Helper()
	se := webrtc.SettingEngine{}
	se.SetIncludeLoopbackCandidate(true)
	se.SetNetworkTypes([]webrtc.NetworkType{webrtc.NetworkTypeUDP4})
	m := &webrtc.MediaEngine{}
	if err := m.RegisterDefaultCodecs(); err != nil {
		t.Fatal(err)
	}
	pc, err := webrtc.NewAPI(webrtc.WithMediaEngine(m), webrtc.WithSettingEngine(se)).NewPeerConnection(webrtc.Configuration{})
	if err != nil {
		t.Fatal(err)
	}
	c := &rtcClient{t: t, pc: pc, received: map[string]*atomic.Int64{}, stop: make(chan struct{})}
	t.Cleanup(func() { close(c.stop); pc.Close() })

	if publish {
		c.audio, err = webrtc.NewTrackLocalStaticRTP(webrtc.RTPCodecCapability{MimeType: webrtc.MimeTypeOpus, ClockRate: 48000, Channels: 2}, "audio", "mic")
		if err != nil {
			t.Fatal(err)
		}
	}
	pc.OnTrack(func(remote *webrtc.TrackRemote, _ *webrtc.RTPReceiver) {
		counter := c.counter(remote.StreamID())
		for {
			if _, _, err := remote.ReadRTP(); err != nil {
				return
			}
			counter.Add(1)
		}
	})

	var added bool
	c.ws = dialRoomWith(t, base, slug, token, name, func(ws *wsClient, msg wsMsg) bool {
		switch msg.Type {
		case "rtc.offer":
			var offer webrtc.SessionDescription
			json.Unmarshal(msg.Data, &offer)
			// Browsers find their upload slots by direction: the participant's own audio and
			// video sections must stay receive-only on the server side in every offer.
			sections := strings.Split(offer.SDP, "\r\nm=")[1:]
			if len(sections) < 2 || !strings.Contains(sections[0], "a=recvonly") || !strings.Contains(sections[1], "a=recvonly") {
				t.Errorf("%s: upload slots are not receive-only in offer:\n%s", name, offer.SDP)
			}
			if err := pc.SetRemoteDescription(offer); err != nil {
				t.Errorf("%s: set offer: %v", name, err)
				return true
			}
			if c.audio != nil && !added {
				added = true
				if _, err := pc.AddTrack(c.audio); err != nil {
					t.Errorf("%s: add track: %v", name, err)
				}
			}
			answer, err := pc.CreateAnswer(nil)
			if err == nil {
				err = pc.SetLocalDescription(answer)
			}
			if err != nil {
				t.Errorf("%s: answer: %v", name, err)
				return true
			}
			ws.trySend("rtc.answer", answer)
			return true
		case "rtc.candidate":
			var cand webrtc.ICECandidateInit
			json.Unmarshal(msg.Data, &cand)
			pc.AddICECandidate(cand)
			return true
		}
		return false
	})
	pc.OnICECandidate(func(cand *webrtc.ICECandidate) {
		if cand != nil {
			c.ws.trySend("rtc.candidate", cand.ToJSON())
		}
	})
	if publish {
		go c.sendAudio()
	}
	return c
}

// sendAudio emits a fake 20 ms Opus packet stream until the test ends.
func (c *rtcClient) sendAudio() {
	tick := time.NewTicker(20 * time.Millisecond)
	defer tick.Stop()
	var seq uint16
	for {
		select {
		case <-c.stop:
			return
		case <-tick.C:
			seq++
			c.audio.WriteRTP(&rtp.Packet{
				Header:  rtp.Header{Version: 2, SequenceNumber: seq, Timestamp: uint32(seq) * 960},
				Payload: []byte{0xfc, 0xff, 0xfe},
			})
		}
	}
}

func (c *rtcClient) counter(stream string) *atomic.Int64 {
	c.mu.Lock()
	defer c.mu.Unlock()
	if c.received[stream] == nil {
		c.received[stream] = &atomic.Int64{}
	}
	return c.received[stream]
}

func (c *rtcClient) packetsFrom(pid string) int64 {
	return c.counter("p-" + pid).Load()
}

// eventually polls cond for up to 5 seconds.
func eventually(t *testing.T, what string, cond func() bool) {
	t.Helper()
	deadline := time.Now().Add(5 * time.Second)
	for !cond() {
		if time.Now().After(deadline) {
			t.Fatalf("timeout: %s", what)
		}
		time.Sleep(50 * time.Millisecond)
	}
}

func TestMediaForwardingRespectsPermissions(t *testing.T) {
	_, st, base := setup(t)
	anna := newUser(t, st, base, "anna")
	var room roomResponse
	anna.expect(201, "POST", "/api/rooms", map[string]string{"name": "Звонок"}, &room)

	host := newRTCClient(t, base, room.Slug, anna.token, "host", true)
	var hh hello
	host.ws.waitFor("hello", &hh)
	var adm struct {
		Media      bool `json:"media"`
		ICEServers []any
	}
	host.ws.waitFor("admitted", &adm)
	if !adm.Media {
		t.Fatal("media not enabled")
	}

	// The guest's client sends audio from the start, without permission.
	guest := newRTCClient(t, base, room.Slug, guestToken(t, base, room.Slug, "Гость"), "guest", true)
	var gh hello
	guest.ws.waitFor("hello", &gh)
	host.ws.send("queue.admit", map[string]string{"participant_id": gh.ParticipantID})
	guest.ws.waitFor("admitted", nil)

	eventually(t, "both connections up", func() bool {
		return host.pc.ConnectionState() == webrtc.PeerConnectionStateConnected &&
			guest.pc.ConnectionState() == webrtc.PeerConnectionStateConnected
	})
	time.Sleep(time.Second)
	if n := host.packetsFrom(gh.ParticipantID); n != 0 {
		t.Fatalf("host heard a guest without permission: %d packets", n)
	}
	// The host is not heard either until they switch the microphone on.
	if n := guest.packetsFrom(hh.ParticipantID); n != 0 {
		t.Fatalf("guest heard a muted host: %d packets", n)
	}

	host.ws.send("media.toggle", map[string]any{"kind": "audio", "enabled": true})
	eventually(t, "guest hears host", func() bool { return guest.packetsFrom(hh.ParticipantID) > 10 })

	// A guest switching on without permission is still not forwarded.
	guest.ws.send("media.toggle", map[string]any{"kind": "audio", "enabled": true})
	guest.ws.expectError("не разрешил")
	host.ws.send("media.permission", map[string]any{"participant_id": gh.ParticipantID, "audio": true})
	guest.ws.waitFor("permission", nil)
	guest.ws.send("media.toggle", map[string]any{"kind": "audio", "enabled": true})
	eventually(t, "host hears guest", func() bool { return host.packetsFrom(gh.ParticipantID) > 10 })

	// The host mutes the guest: forwarding stops even though the guest keeps sending.
	host.ws.send("media.mute", map[string]any{"participant_id": gh.ParticipantID, "kind": "audio"})
	host.ws.waitParticipants(func(l []hub.ParticipantView) bool { p := find(l, gh.ParticipantID); return p != nil && !p.Audio })
	time.Sleep(300 * time.Millisecond)
	before := host.packetsFrom(gh.ParticipantID)
	time.Sleep(700 * time.Millisecond)
	if after := host.packetsFrom(gh.ParticipantID); after != before {
		t.Fatalf("muted guest still forwarded: %d -> %d packets", before, after)
	}

	// When the guest leaves, the host's offer no longer carries the guest's track.
	guest.ws.send("leave", nil)
	eventually(t, "guest track removed", func() bool {
		ld := host.pc.CurrentRemoteDescription()
		return ld != nil && !strings.Contains(ld.SDP, "p-"+gh.ParticipantID)
	})
}
