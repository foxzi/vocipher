package webrtc

import (
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"github.com/foxzi/vocala/internal/logger"
	"net"
	"strings"
	"sync"
	"time"

	"github.com/pion/interceptor"
	"github.com/pion/interceptor/pkg/intervalpli"
	"github.com/pion/rtcp"
	"github.com/pion/webrtc/v4"
)

// Peer represents a connected WebRTC peer in a channel.
type Peer struct {
	UserID   int64
	Username string
	PC       *webrtc.PeerConnection
	// Incoming tracks from this peer
	audioTrack   *webrtc.TrackRemote
	screenTrack  *webrtc.TrackRemote
	cameraTrack  *webrtc.TrackRemote
	expectCamera bool // set via WS before renegotiation
	expectScreen bool // set via WS before renegotiation
	// streamKinds maps incoming MediaStream id (msid) -> kind ("camera"|"screen").
	// Populated via WS "media_track" messages so OnTrack can classify each
	// incoming video track unambiguously, even when camera and screen are
	// added in the same renegotiation. Protected by mu.
	streamKinds map[string]string
	// Outgoing local tracks forwarded to this peer (from other peers)
	outputTracks       map[int64]*webrtc.TrackLocalStaticRTP // audio
	screenOutputTracks map[int64]*webrtc.TrackLocalStaticRTP // screen
	cameraOutputTracks map[int64]*webrtc.TrackLocalStaticRTP // camera
	mu                 sync.Mutex
	negoMu             sync.Mutex
	negoScheduled      bool        // debounce flag for renegotiation
	iceRestartTimer    *time.Timer // pending ICE restart on disconnect
	iceFailoverTimer   *time.Timer // hard timeout to remove peer if ICE never recovers
}

// SFU manages all peer connections for a channel.
type SFU struct {
	mu    sync.RWMutex
	peers map[int64]*Peer

	SendMessage func(userID int64, msg []byte)
}

var (
	globalMu sync.RWMutex
	sfus     = make(map[int64]*SFU)
)

var (
	api     *webrtc.API
	apiOnce sync.Once
	natIP   string
)

var (
	udpPortMin uint16
	udpPortMax uint16
)

func SetNATIP(ip string) {
	natIP = ip
	logger.Info("webrtc: NAT 1:1 IP set to %s", ip)
}

// SetUDPPortRange sets the ephemeral UDP port range for WebRTC.
func SetUDPPortRange(min, max uint16) {
	udpPortMin = min
	udpPortMax = max
	logger.Info("webrtc: UDP port range set to %d-%d", min, max)
}

func getAPI() *webrtc.API {
	apiOnce.Do(func() {
		m := &webrtc.MediaEngine{}
		if err := m.RegisterDefaultCodecs(); err != nil {
			logger.Fatal("webrtc: failed to register codecs:", err)
		}

		i := &interceptor.Registry{}
		if err := webrtc.RegisterDefaultInterceptors(m, i); err != nil {
			logger.Fatal("webrtc: failed to register interceptors:", err)
		}

		pliFactory, err := intervalpli.NewReceiverInterceptor()
		if err != nil {
			logger.Fatal("webrtc: failed to create PLI interceptor:", err)
		}
		i.Add(pliFactory)

		opts := []func(*webrtc.API){
			webrtc.WithMediaEngine(m),
			webrtc.WithInterceptorRegistry(i),
		}

		// Always configure SettingEngine for keepalive and optional NAT/ports
		s := webrtc.SettingEngine{}
		// ICE keepalive every 2s to maintain NAT bindings on mobile networks
		s.SetICETimeouts(15*time.Second, 60*time.Second, 2*time.Second)
		// Enable ICE TCP candidates for mobile clients behind aggressive NAT
		tcpListener, tcpErr := net.ListenTCP("tcp", &net.TCPAddr{Port: 40201})
		if tcpErr == nil {
			tcpMux := webrtc.NewICETCPMux(nil, tcpListener, 8)
			s.SetICETCPMux(tcpMux)
			logger.Debug("webrtc: ICE TCP mux listening on port 40201")
		} else {
			logger.Error("webrtc: failed to start ICE TCP mux: %v", tcpErr)
		}
		if natIP != "" {
			s.SetNAT1To1IPs([]string{natIP}, webrtc.ICECandidateTypeHost)
		}
		if udpPortMin > 0 && udpPortMax > 0 {
			s.SetEphemeralUDPPortRange(udpPortMin, udpPortMax)
		}
		opts = append(opts, webrtc.WithSettingEngine(s))

		api = webrtc.NewAPI(opts...)
	})
	return api
}

type TURNCredentials struct {
	URIs     []string
	Username string
	Password string
}

var turnCreds *TURNCredentials

func SetTURNCredentials(uris []string, username, password string) {
	turnCreds = &TURNCredentials{URIs: uris, Username: username, Password: password}
}

func GetTURNCredentials() *TURNCredentials {
	return turnCreds
}

func newPeerConnectionConfig() webrtc.Configuration {
	// SFU does not need TURN — it has a public IP via NAT 1:1.
	// TURN credentials are only passed to clients via the HTML template.
	return webrtc.Configuration{
		ICEServers: []webrtc.ICEServer{
			{URLs: []string{"stun:stun.l.google.com:19302"}},
			{URLs: []string{"stun:stun1.l.google.com:19302"}},
		},
	}
}

func GetOrCreateSFU(channelID int64, sendMsg func(userID int64, msg []byte)) *SFU {
	globalMu.Lock()
	defer globalMu.Unlock()
	if s, ok := sfus[channelID]; ok {
		return s
	}
	s := &SFU{peers: make(map[int64]*Peer), SendMessage: sendMsg}
	sfus[channelID] = s
	return s
}

func GetSFU(channelID int64) *SFU {
	globalMu.RLock()
	defer globalMu.RUnlock()
	return sfus[channelID]
}

func RemoveSFU(channelID int64) {
	globalMu.Lock()
	defer globalMu.Unlock()
	if s, ok := sfus[channelID]; ok {
		s.mu.RLock()
		empty := len(s.peers) == 0
		s.mu.RUnlock()
		if empty {
			delete(sfus, channelID)
		}
	}
}

// HandleOffer processes an SDP offer from a client.
func (s *SFU) HandleOffer(userID int64, username string, offerSDP string) error {
	offer := webrtc.SessionDescription{Type: webrtc.SDPTypeOffer, SDP: offerSDP}
	logger.Debug("webrtc: offer from user %d:\n%s", userID, offerSDP)

	s.mu.RLock()
	existingPeer, exists := s.peers[userID]
	s.mu.RUnlock()

	if exists {
		// Glare handling: if server already has its own offer in flight
		// (have-local-offer), we cannot accept the client offer — pion v4
		// does not expose rollback via its public API. Drop the client
		// offer silently. The client must be "polite": when our pending
		// server offer reaches it, the client will rollback its own offer
		// and accept ours, ending the deadlock. After the exchange, the
		// client's onnegotiationneeded fires again and its offer is re-sent.
		if existingPeer.PC.SignalingState() == webrtc.SignalingStateHaveLocalOffer {
			logger.Warn("webrtc: glare on user %d offer — dropping, re-sending server offer", userID)
			// We can't rollback (pion v4 public API doesn't allow it), so we
			// drop the client offer. The client MUST receive a server offer
			// to know to rollback. If our original server offer got lost or
			// was ignored, re-push it now so the client can rollback and
			// accept it. After that exchange, the client's
			// onnegotiationneeded fires again and re-sends the offer.
			if pending := existingPeer.PC.PendingLocalDescription(); pending != nil && pending.Type == webrtc.SDPTypeOffer {
				data, _ := json.Marshal(map[string]any{
					"type":    "webrtc_offer",
					"payload": map[string]any{"sdp": pending.SDP},
				})
				s.SendMessage(userID, data)
			}
			return nil
		}
		if err := existingPeer.PC.SetRemoteDescription(offer); err != nil {
			return err
		}
		answer, err := existingPeer.PC.CreateAnswer(nil)
		if err != nil {
			return err
		}
		if err := existingPeer.PC.SetLocalDescription(answer); err != nil {
			return err
		}
		logger.Debug("webrtc: answer to user %d:\n%s", userID, answer.SDP)
		data, _ := json.Marshal(map[string]any{
			"type":    "webrtc_answer",
			"payload": map[string]any{"sdp": answer.SDP},
		})
		s.SendMessage(userID, data)
		return nil
	}

	pc, err := getAPI().NewPeerConnection(newPeerConnectionConfig())
	if err != nil {
		return err
	}

	peer := &Peer{
		UserID:             userID,
		Username:           username,
		PC:                 pc,
		outputTracks:       make(map[int64]*webrtc.TrackLocalStaticRTP),
		screenOutputTracks: make(map[int64]*webrtc.TrackLocalStaticRTP),
		cameraOutputTracks: make(map[int64]*webrtc.TrackLocalStaticRTP),
		streamKinds:        make(map[string]string),
	}

	// Video tracks: if expectCamera flag is set, treat as camera; otherwise screen
	pc.OnTrack(func(track *webrtc.TrackRemote, _ *webrtc.RTPReceiver) {
		codec := track.Codec()
		logger.Info("webrtc: incoming %s track from user %d: codec=%s pt=%d clock=%d channels=%d ssrc=%d rid=%q fmtp=%q",
			track.Kind(), userID,
			codec.MimeType, codec.PayloadType, codec.ClockRate, codec.Channels,
			track.SSRC(), track.RID(), codec.SDPFmtpLine,
		)
		switch track.Kind() {
		case webrtc.RTPCodecTypeAudio:
			s.handleAudioTrack(peer, userID, username, track)
		case webrtc.RTPCodecTypeVideo:
			// Classify incoming video track. Prefer the explicit msid->kind
			// mapping the client registered via WS "media_track" — this is
			// reliable when both camera and screen tracks are added in the
			// same renegotiation. Fall back to expectScreen/expectCamera
			// flags for older clients that don't send "media_track".
			peer.mu.Lock()
			hint := peer.streamKinds[track.StreamID()]
			delete(peer.streamKinds, track.StreamID())
			peer.mu.Unlock()
			logger.Info("webrtc: classify video user %d streamID=%q hint=%q expectScreen=%v expectCamera=%v cameraTrack=%v screenTrack=%v",
				userID, track.StreamID(), hint, peer.expectScreen, peer.expectCamera, peer.cameraTrack != nil, peer.screenTrack != nil)
			if hint == "screen" && peer.screenTrack == nil {
				s.handleScreenTrack(peer, userID, username, track)
			} else if hint == "camera" && peer.cameraTrack == nil {
				s.handleCameraTrack(peer, userID, username, track)
			} else if peer.expectScreen && peer.screenTrack == nil {
				peer.expectScreen = false
				s.handleScreenTrack(peer, userID, username, track)
			} else if peer.expectCamera && peer.cameraTrack == nil {
				peer.expectCamera = false
				s.handleCameraTrack(peer, userID, username, track)
			} else {
				// No explicit hint (older client, or flag already consumed).
				// Default: fill whichever slot is empty — camera first since
				// that's the more common case.
				if peer.cameraTrack == nil {
					logger.Warn("webrtc: user %d video track with no hint — defaulting to camera (fallback)", userID)
					s.handleCameraTrack(peer, userID, username, track)
				} else {
					logger.Warn("webrtc: user %d video track with no hint — defaulting to screen (fallback)", userID)
					s.handleScreenTrack(peer, userID, username, track)
				}
			}
		}
	})

	pc.OnICEConnectionStateChange(func(state webrtc.ICEConnectionState) {
		logger.Debug("webrtc: peer %d ICE state: %s", userID, state.String())
		switch state {
		case webrtc.ICEConnectionStateDisconnected:
			logger.Warn("webrtc: peer %d ICE disconnected — scheduling restart in 5s", userID)
			peer.mu.Lock()
			if peer.iceRestartTimer == nil {
				peer.iceRestartTimer = time.AfterFunc(5*time.Second, func() {
					s.iceRestart(peer)
				})
			}
			if peer.iceFailoverTimer == nil {
				peer.iceFailoverTimer = time.AfterFunc(30*time.Second, func() {
					logger.Warn("webrtc: peer %d ICE failover timeout — removing", userID)
					s.RemovePeer(userID)
				})
			}
			peer.mu.Unlock()
		case webrtc.ICEConnectionStateConnected, webrtc.ICEConnectionStateCompleted:
			peer.mu.Lock()
			if peer.iceRestartTimer != nil {
				peer.iceRestartTimer.Stop()
				peer.iceRestartTimer = nil
			}
			if peer.iceFailoverTimer != nil {
				peer.iceFailoverTimer.Stop()
				peer.iceFailoverTimer = nil
			}
			peer.mu.Unlock()
		case webrtc.ICEConnectionStateFailed, webrtc.ICEConnectionStateClosed:
			logger.Warn("webrtc: peer %d ICE %s — network problem (media may be stuck)", userID, state.String())
			peer.mu.Lock()
			if peer.iceRestartTimer != nil {
				peer.iceRestartTimer.Stop()
				peer.iceRestartTimer = nil
			}
			if peer.iceFailoverTimer != nil {
				peer.iceFailoverTimer.Stop()
				peer.iceFailoverTimer = nil
			}
			peer.mu.Unlock()
		}
	})

	pc.OnICECandidate(func(c *webrtc.ICECandidate) {
		if c == nil {
			return
		}
		data, _ := json.Marshal(map[string]any{
			"type":    "ice_candidate",
			"payload": map[string]any{"candidate": c.ToJSON()},
		})
		s.SendMessage(userID, data)
	})

	var addExistingOnce sync.Once
	var logPairOnce sync.Once
	pc.OnConnectionStateChange(func(state webrtc.PeerConnectionState) {
		logger.Debug("webrtc: peer %d connection state: %s", userID, state.String())
		if state == webrtc.PeerConnectionStateConnected {
			logPairOnce.Do(func() { go logSelectedCandidatePair(pc, userID) })
			addExistingOnce.Do(func() { go s.addExistingTracksForPeer(peer, userID) })
		}
		if state == webrtc.PeerConnectionStateFailed || state == webrtc.PeerConnectionStateClosed {
			s.RemovePeer(userID)
		}
	})

	if err := pc.SetRemoteDescription(offer); err != nil {
		pc.Close()
		return err
	}
	answer, err := pc.CreateAnswer(nil)
	if err != nil {
		pc.Close()
		return err
	}
	if err := pc.SetLocalDescription(answer); err != nil {
		pc.Close()
		return err
	}
	logger.Debug("webrtc: answer to user %d:\n%s", userID, answer.SDP)

	s.mu.Lock()
	s.peers[userID] = peer
	s.mu.Unlock()

	data, _ := json.Marshal(map[string]any{
		"type":    "webrtc_answer",
		"payload": map[string]any{"sdp": answer.SDP},
	})
	s.SendMessage(userID, data)
	return nil
}

func SDPHasVideoSending(sdp string) bool {
	lines := strings.Split(sdp, "\n")
	inVideo := false
	for _, line := range lines {
		line = strings.TrimSpace(line)
		if strings.HasPrefix(line, "m=video") {
			inVideo = true
		} else if strings.HasPrefix(line, "m=") {
			inVideo = false
		}
		if inVideo && (line == "a=sendrecv" || line == "a=sendonly") {
			return true
		}
	}
	return false
}

func (s *SFU) addExistingTracksForPeer(peer *Peer, userID int64) {
	needsRenego := false
	s.mu.RLock()
	for srcID, ep := range s.peers {
		if srcID == userID {
			continue
		}
		if ep.audioTrack != nil {
			if err := s.addOutputTrack(peer, srcID, ep.audioTrack, "audio"); err != nil {
				logger.Error("webrtc: failed to add existing audio %d->%d: %v", srcID, userID, err)
			} else {
				needsRenego = true
			}
		}
		if ep.screenTrack != nil {
			if err := s.addOutputTrack(peer, srcID, ep.screenTrack, "screen"); err != nil {
				logger.Error("webrtc: failed to add existing screen %d->%d: %v", srcID, userID, err)
			} else {
				needsRenego = true
				s.sendPLI(ep, ep.screenTrack)
			}
		}
		if ep.cameraTrack != nil {
			if err := s.addOutputTrack(peer, srcID, ep.cameraTrack, "camera"); err != nil {
				logger.Error("webrtc: failed to add existing camera %d->%d: %v", srcID, userID, err)
			} else {
				needsRenego = true
				s.sendPLI(ep, ep.cameraTrack)
			}
		}
	}
	s.mu.RUnlock()
	if needsRenego {
		s.renegotiate(peer)
	}
}

// SetExpectCamera marks the next video track from this user as a camera track.
func (s *SFU) SetExpectCamera(userID int64, expect bool) {
	s.mu.RLock()
	peer, ok := s.peers[userID]
	s.mu.RUnlock()
	if ok {
		peer.expectCamera = expect
	}
}

// SetExpectScreen marks the next video track from this user as a screen track.
func (s *SFU) SetExpectScreen(userID int64, expect bool) {
	s.mu.RLock()
	peer, ok := s.peers[userID]
	s.mu.RUnlock()
	if ok {
		peer.expectScreen = expect
	}
}

// SetStreamKind registers an explicit msid -> kind mapping so that the next
// OnTrack with track.StreamID()==streamID is classified as the given kind
// ("camera" or "screen"). This is the preferred path: it works correctly
// even when multiple video tracks are added in a single renegotiation.
func (s *SFU) SetStreamKind(userID int64, streamID, kind string) {
	if streamID == "" || (kind != "camera" && kind != "screen") {
		return
	}
	s.mu.RLock()
	peer, ok := s.peers[userID]
	s.mu.RUnlock()
	if !ok {
		return
	}
	peer.mu.Lock()
	if peer.streamKinds == nil {
		peer.streamKinds = make(map[string]string)
	}
	peer.streamKinds[streamID] = kind
	peer.mu.Unlock()
}

func (s *SFU) HandleICECandidate(userID int64, candidateJSON json.RawMessage) error {
	s.mu.RLock()
	peer, ok := s.peers[userID]
	s.mu.RUnlock()
	if !ok {
		return nil
	}
	var candidate webrtc.ICECandidateInit
	if err := json.Unmarshal(candidateJSON, &candidate); err != nil {
		return err
	}
	return peer.PC.AddICECandidate(candidate)
}

func (s *SFU) RemovePeer(userID int64) {
	s.mu.Lock()
	peer, ok := s.peers[userID]
	if !ok {
		s.mu.Unlock()
		return
	}
	delete(s.peers, userID)

	// Remove output tracks from other peers' PeerConnections
	needsRenego := make([]*Peer, 0)
	for _, op := range s.peers {
		op.mu.Lock()
		removed := false
		for _, tracks := range []map[int64]*webrtc.TrackLocalStaticRTP{
			op.outputTracks, op.screenOutputTracks, op.cameraOutputTracks,
		} {
			if lt, exists := tracks[userID]; exists {
				// Remove from PeerConnection
				for _, sender := range op.PC.GetSenders() {
					if sender.Track() == lt {
						op.PC.RemoveTrack(sender)
						break
					}
				}
				delete(tracks, userID)
				removed = true
			}
		}
		op.mu.Unlock()
		if removed {
			needsRenego = append(needsRenego, op)
		}
	}
	s.mu.Unlock()

	// Renegotiate with peers that had tracks removed
	for _, op := range needsRenego {
		s.renegotiate(op)
	}

	peer.mu.Lock()
	if peer.iceRestartTimer != nil {
		peer.iceRestartTimer.Stop()
		peer.iceRestartTimer = nil
	}
	if peer.iceFailoverTimer != nil {
		peer.iceFailoverTimer.Stop()
		peer.iceFailoverTimer = nil
	}
	peer.mu.Unlock()

	if peer.PC.ConnectionState() != webrtc.PeerConnectionStateClosed {
		peer.PC.Close()
	}
	logger.Debug("webrtc: removed peer %d", userID)
}

// --- Track handlers ---

func (s *SFU) handleAudioTrack(peer *Peer, userID int64, username string, track *webrtc.TrackRemote) {
	logger.Info("webrtc: audio track from user %d", userID)
	s.mu.Lock()
	peer.audioTrack = track
	s.mu.Unlock()

	s.mu.RLock()
	for otherID, op := range s.peers {
		if otherID == userID {
			continue
		}
		if err := s.addOutputTrack(op, userID, track, "audio"); err != nil {
			logger.Error("webrtc: failed to add audio %d->%d: %v", userID, otherID, err)
		} else {
			s.renegotiate(op)
		}
	}
	s.mu.RUnlock()

	s.forwardRTP(track, userID, "audio", 1500)
}

func (s *SFU) handleScreenTrack(peer *Peer, userID int64, username string, track *webrtc.TrackRemote) {
	logger.Info("webrtc: screen track from user %d", userID)
	s.mu.Lock()
	peer.screenTrack = track
	s.mu.Unlock()

	s.mu.RLock()
	for otherID, op := range s.peers {
		if otherID == userID {
			continue
		}
		if err := s.addOutputTrack(op, userID, track, "screen"); err != nil {
			logger.Error("webrtc: failed to add screen %d->%d: %v", userID, otherID, err)
		} else {
			s.renegotiate(op)
		}
	}
	s.mu.RUnlock()

	s.sendPLI(peer, track)
	s.forwardRTP(track, userID, "screen", 4096)

	// Track ended — cleanup
	logger.Info("webrtc: screen track ended for user %d", userID)
	s.mu.Lock()
	peer.screenTrack = nil
	for _, op := range s.peers {
		op.mu.Lock()
		delete(op.screenOutputTracks, userID)
		op.mu.Unlock()
	}
	s.mu.Unlock()
	s.renegotiateAllExcept(userID)
}

func (s *SFU) handleCameraTrack(peer *Peer, userID int64, username string, track *webrtc.TrackRemote) {
	logger.Info("webrtc: camera track from user %d", userID)
	s.mu.Lock()
	peer.cameraTrack = track
	s.mu.Unlock()

	s.mu.RLock()
	for otherID, op := range s.peers {
		if otherID == userID {
			continue
		}
		if err := s.addOutputTrack(op, userID, track, "camera"); err != nil {
			logger.Error("webrtc: failed to add camera %d->%d: %v", userID, otherID, err)
		} else {
			s.renegotiate(op)
		}
	}
	s.mu.RUnlock()

	s.sendPLI(peer, track)
	s.forwardRTP(track, userID, "camera", 4096)

	// Track ended — cleanup
	logger.Info("webrtc: camera track ended for user %d", userID)
	s.mu.Lock()
	peer.cameraTrack = nil
	for _, op := range s.peers {
		op.mu.Lock()
		delete(op.cameraOutputTracks, userID)
		op.mu.Unlock()
	}
	s.mu.Unlock()
	s.renegotiateAllExcept(userID)
}

// --- Helpers ---

func (s *SFU) forwardRTP(track *webrtc.TrackRemote, userID int64, kind string, bufSize int) {
	buf := make([]byte, bufSize)
	for {
		n, _, err := track.Read(buf)
		if err != nil {
			logger.Info("webrtc: %s track read ended for user %d: %v", kind, userID, err)
			return
		}
		s.mu.RLock()
		for otherID, op := range s.peers {
			if otherID == userID {
				continue
			}
			op.mu.Lock()
			var lt *webrtc.TrackLocalStaticRTP
			switch kind {
			case "audio":
				lt = op.outputTracks[userID]
			case "screen":
				lt = op.screenOutputTracks[userID]
			case "camera":
				lt = op.cameraOutputTracks[userID]
			}
			if lt != nil {
				if _, writeErr := lt.Write(buf[:n]); writeErr != nil {
					logger.Info("webrtc: %s write to user %d failed: %v", kind, otherID, writeErr)
				}
			}
			op.mu.Unlock()
		}
		s.mu.RUnlock()
	}
}

func randomID() string {
	b := make([]byte, 8)
	rand.Read(b)
	return hex.EncodeToString(b)
}

func (s *SFU) addOutputTrack(destPeer *Peer, srcUserID int64, srcTrack *webrtc.TrackRemote, kind string) error {
	// Check if we already have an output track of this kind from this source user
	destPeer.mu.Lock()
	switch kind {
	case "camera":
		if _, exists := destPeer.cameraOutputTracks[srcUserID]; exists {
			destPeer.mu.Unlock()
			logger.Debug("webrtc: skipping duplicate %s output track %d->%d", kind, srcUserID, destPeer.UserID)
			return nil
		}
	case "screen":
		if _, exists := destPeer.screenOutputTracks[srcUserID]; exists {
			destPeer.mu.Unlock()
			logger.Debug("webrtc: skipping duplicate %s output track %d->%d", kind, srcUserID, destPeer.UserID)
			return nil
		}
	case "audio":
		if _, exists := destPeer.outputTracks[srcUserID]; exists {
			destPeer.mu.Unlock()
			logger.Debug("webrtc: skipping duplicate %s output track %d->%d", kind, srcUserID, destPeer.UserID)
			return nil
		}
	}
	destPeer.mu.Unlock()

	streamID := fmt.Sprintf("%s-%d", kind, srcUserID)
	trackID := fmt.Sprintf("%s-%d-%s", kind, srcUserID, randomID())
	localTrack, err := webrtc.NewTrackLocalStaticRTP(
		srcTrack.Codec().RTPCodecCapability,
		trackID,
		streamID,
	)
	if err != nil {
		return err
	}

	destPeer.mu.Lock()
	switch kind {
	case "audio":
		destPeer.outputTracks[srcUserID] = localTrack
	case "screen":
		destPeer.screenOutputTracks[srcUserID] = localTrack
	case "camera":
		destPeer.cameraOutputTracks[srcUserID] = localTrack
	}
	destPeer.mu.Unlock()

	if _, err := destPeer.PC.AddTrack(localTrack); err != nil {
		destPeer.mu.Lock()
		switch kind {
		case "audio":
			delete(destPeer.outputTracks, srcUserID)
		case "screen":
			delete(destPeer.screenOutputTracks, srcUserID)
		case "camera":
			delete(destPeer.cameraOutputTracks, srcUserID)
		}
		destPeer.mu.Unlock()
		return err
	}
	logger.Debug("webrtc: forward %s %d->%d codec=%s pt=%d",
		kind, srcUserID, destPeer.UserID,
		srcTrack.Codec().MimeType, srcTrack.Codec().PayloadType)

	// For video tracks (camera, screen), ask the publisher for a keyframe
	// shortly after the new subscriber has had time to finish renegotiation.
	// Without this the subscriber shows a black frame until the next
	// intervalpli tick (up to several seconds) or a publisher-driven keyframe.
	if kind == "screen" || kind == "camera" {
		go func() {
			time.Sleep(800 * time.Millisecond)
			s.mu.RLock()
			srcPeer, ok := s.peers[srcUserID]
			s.mu.RUnlock()
			if ok {
				s.sendPLI(srcPeer, srcTrack)
			}
		}()
	}
	return nil
}

// logSelectedCandidatePair logs the nominated ICE candidate pair once the
// connection is established. Helps diagnose NAT/TURN issues that affect both
// audio and video.
func logSelectedCandidatePair(pc *webrtc.PeerConnection, userID int64) {
	// Stats may not be populated immediately after Connected fires.
	time.Sleep(500 * time.Millisecond)
	stats := pc.GetStats()
	for _, stat := range stats {
		pair, ok := stat.(webrtc.ICECandidatePairStats)
		if !ok || !pair.Nominated || pair.State != webrtc.StatsICECandidatePairStateSucceeded {
			continue
		}
		local, _ := stats[pair.LocalCandidateID].(webrtc.ICECandidateStats)
		remote, _ := stats[pair.RemoteCandidateID].(webrtc.ICECandidateStats)
		logger.Info("webrtc: peer %d ICE pair: local=%s/%s/%s:%d remote=%s/%s/%s:%d",
			userID,
			local.CandidateType, local.Protocol, local.IP, local.Port,
			remote.CandidateType, remote.Protocol, remote.IP, remote.Port,
		)
		return
	}
	logger.Debug("webrtc: peer %d no nominated ICE pair found in stats", userID)
}

func (s *SFU) sendPLI(peer *Peer, track *webrtc.TrackRemote) {
	if peer.PC.ConnectionState() == webrtc.PeerConnectionStateClosed {
		return
	}
	if err := peer.PC.WriteRTCP([]rtcp.Packet{
		&rtcp.PictureLossIndication{MediaSSRC: uint32(track.SSRC())},
	}); err != nil {
		logger.Error("webrtc: failed to send PLI for user %d: %v", peer.UserID, err)
	}
}

func (s *SFU) renegotiateAllExcept(userID int64) {
	s.mu.RLock()
	for otherID, op := range s.peers {
		if otherID == userID {
			continue
		}
		s.renegotiate(op)
	}
	s.mu.RUnlock()
}

// renegotiate schedules a debounced renegotiation for a peer.
// Multiple calls within 300ms are coalesced into one offer.
func (s *SFU) renegotiate(peer *Peer) {
	peer.mu.Lock()
	if peer.negoScheduled {
		peer.mu.Unlock()
		return
	}
	peer.negoScheduled = true
	peer.mu.Unlock()

	go func() {
		time.Sleep(500 * time.Millisecond)

		peer.mu.Lock()
		peer.negoScheduled = false
		peer.mu.Unlock()

		s.doRenegotiate(peer)
	}()
}

func (s *SFU) doRenegotiate(peer *Peer) {
	peer.negoMu.Lock()
	defer peer.negoMu.Unlock()

	if peer.PC.ConnectionState() == webrtc.PeerConnectionStateClosed {
		return
	}

	for attempts := 0; attempts < 50; attempts++ {
		if peer.PC.SignalingState() == webrtc.SignalingStateStable {
			break
		}
		if attempts == 0 {
			logger.Debug("webrtc: waiting for stable signaling state for user %d (current: %s)", peer.UserID, peer.PC.SignalingState())
		}
		peer.negoMu.Unlock()
		time.Sleep(100 * time.Millisecond)
		peer.negoMu.Lock()
	}
	if peer.PC.SignalingState() != webrtc.SignalingStateStable {
		logger.Debug("webrtc: renegotiation timeout for user %d, signaling state: %s", peer.UserID, peer.PC.SignalingState())
		return
	}

	offer, err := peer.PC.CreateOffer(nil)
	if err != nil {
		logger.Debug("webrtc: renegotiate offer failed for user %d: %v", peer.UserID, err)
		return
	}
	if err := peer.PC.SetLocalDescription(offer); err != nil {
		logger.Debug("webrtc: renegotiate setlocal failed for user %d: %v", peer.UserID, err)
		return
	}

	logger.Debug("webrtc: sending renegotiation offer to user %d:\n%s", peer.UserID, offer.SDP)
	data, _ := json.Marshal(map[string]any{
		"type":    "webrtc_offer",
		"payload": map[string]any{"sdp": offer.SDP},
	})
	s.SendMessage(peer.UserID, data)
}

// iceRestart sends a new offer with ICERestart:true to recover from a
// prolonged ICE "disconnected" state without tearing down the PeerConnection.
func (s *SFU) iceRestart(peer *Peer) {
	if peer.PC.ConnectionState() == webrtc.PeerConnectionStateClosed {
		return
	}

	peer.negoMu.Lock()
	defer peer.negoMu.Unlock()

	if peer.PC.SignalingState() != webrtc.SignalingStateStable {
		logger.Debug("webrtc: ICE restart skipped for user %d — signaling not stable (%s)", peer.UserID, peer.PC.SignalingState())
		return
	}

	offer, err := peer.PC.CreateOffer(&webrtc.OfferOptions{ICERestart: true})
	if err != nil {
		logger.Debug("webrtc: ICE restart offer failed for user %d: %v", peer.UserID, err)
		return
	}
	if err := peer.PC.SetLocalDescription(offer); err != nil {
		logger.Debug("webrtc: ICE restart setlocal failed for user %d: %v", peer.UserID, err)
		return
	}

	logger.Info("webrtc: ICE restart triggered for user %d", peer.UserID)
	data, _ := json.Marshal(map[string]any{
		"type":    "webrtc_offer",
		"payload": map[string]any{"sdp": offer.SDP},
	})
	s.SendMessage(peer.UserID, data)
}

func (s *SFU) HandleAnswer(userID int64, answerSDP string) error {
	s.mu.RLock()
	peer, ok := s.peers[userID]
	s.mu.RUnlock()
	if !ok {
		return nil
	}

	peer.negoMu.Lock()
	defer peer.negoMu.Unlock()

	logger.Debug("webrtc: received answer from user %d, signaling state: %s\n%s", userID, peer.PC.SignalingState(), answerSDP)
	return peer.PC.SetRemoteDescription(webrtc.SessionDescription{
		Type: webrtc.SDPTypeAnswer,
		SDP:  answerSDP,
	})
}
