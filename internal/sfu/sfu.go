// Package sfu forwards audio and video between room participants (Selective Forwarding Unit).
//
// Every participant has one PeerConnection with the server. The server is always the
// offerer: it offers one receive-only audio and one receive-only video transceiver for the
// participant's own camera and microphone, plus one send-only track for every other
// participant's published track. When tracks appear or disappear the server renegotiates.
//
// Forwarding of each published track passes through a gate controlled by the room logic,
// so a muted participant or a guest without permission is not heard or seen even if
// a modified client keeps sending media.
package sfu

import (
	"crypto/hmac"
	"crypto/sha1"
	"encoding/base64"
	"fmt"
	"log/slog"
	"net"
	"strconv"
	"time"

	"github.com/pion/interceptor"
	"github.com/pion/logging"
	"github.com/pion/webrtc/v4"
)

type Config struct {
	// UDPPort is the single UDP port used for all media (ICE UDP mux).
	UDPPort int
	// UDPAddr optionally restricts the listen address (tests use 127.0.0.1).
	UDPAddr string
	// PublicIP replaces the local address in host candidates, needed behind Docker or cloud NAT.
	PublicIP string
	// IncludeLoopback gathers loopback candidates (tests only).
	IncludeLoopback bool
	// TURNURLs and TURNSecret configure coturn with "use-auth-secret": clients get short-lived credentials.
	TURNURLs   []string
	TURNSecret string
	// STUNURLs are handed to browsers as is.
	STUNURLs []string
}

// SFU holds the shared WebRTC engine; rooms are created with NewRoom.
type SFU struct {
	cfg  Config
	api  *webrtc.API
	conn net.PacketConn
	log  *slog.Logger
}

func New(cfg Config, log *slog.Logger) (*SFU, error) {
	conn, err := net.ListenPacket("udp4", net.JoinHostPort(cfg.UDPAddr, strconv.Itoa(cfg.UDPPort)))
	if err != nil {
		return nil, fmt.Errorf("listen media UDP port: %w", err)
	}

	m := &webrtc.MediaEngine{}
	if err := m.RegisterDefaultCodecs(); err != nil {
		conn.Close()
		return nil, err
	}
	reg := &interceptor.Registry{}
	if err := webrtc.RegisterDefaultInterceptors(m, reg); err != nil {
		conn.Close()
		return nil, err
	}

	se := webrtc.SettingEngine{}
	se.SetICEUDPMux(webrtc.NewICEUDPMux(logging.NewDefaultLoggerFactory().NewLogger("ice"), conn))
	se.SetNetworkTypes([]webrtc.NetworkType{webrtc.NetworkTypeUDP4})
	se.SetIncludeLoopbackCandidate(cfg.IncludeLoopback)
	if cfg.PublicIP != "" {
		if err := se.SetICEAddressRewriteRules(webrtc.ICEAddressRewriteRule{
			External:        []string{cfg.PublicIP},
			AsCandidateType: webrtc.ICECandidateTypeHost,
		}); err != nil {
			conn.Close()
			return nil, err
		}
	}

	api := webrtc.NewAPI(webrtc.WithMediaEngine(m), webrtc.WithInterceptorRegistry(reg), webrtc.WithSettingEngine(se))
	return &SFU{cfg: cfg, api: api, conn: conn, log: log}, nil
}

func (s *SFU) Close() error { return s.conn.Close() }

// Port reports the UDP port actually in use.
func (s *SFU) Port() int { return s.conn.LocalAddr().(*net.UDPAddr).Port }

// ICEServer is the browser-side RTCIceServer.
type ICEServer struct {
	URLs       []string `json:"urls"`
	Username   string   `json:"username,omitempty"`
	Credential string   `json:"credential,omitempty"`
}

// ICEServers returns the servers a browser should use. TURN credentials follow the
// coturn REST scheme: username "<expiry>:<id>", password base64(HMAC-SHA1(secret, username)).
func (s *SFU) ICEServers(id string) []ICEServer {
	servers := []ICEServer{}
	if len(s.cfg.STUNURLs) > 0 {
		servers = append(servers, ICEServer{URLs: s.cfg.STUNURLs})
	}
	if len(s.cfg.TURNURLs) > 0 && s.cfg.TURNSecret != "" {
		user := strconv.FormatInt(time.Now().Add(12*time.Hour).Unix(), 10) + ":" + id
		mac := hmac.New(sha1.New, []byte(s.cfg.TURNSecret))
		mac.Write([]byte(user))
		servers = append(servers, ICEServer{
			URLs: s.cfg.TURNURLs, Username: user, Credential: base64.StdEncoding.EncodeToString(mac.Sum(nil)),
		})
	}
	return servers
}
