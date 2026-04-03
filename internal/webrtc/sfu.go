package webrtc

import (
	"encoding/json"
	"fmt"
	"log"
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
	// Outgoing local tracks forwarded to this peer (from other peers)
	outputTracks       map[int64]*webrtc.TrackLocalStaticRTP // audio
	screenOutputTracks map[int64]*webrtc.TrackLocalStaticRTP // screen
	cameraOutputTracks map[int64]*webrtc.TrackLocalStaticRTP // camera
	mu                 sync.Mutex
	negoMu             sync.Mutex
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
	log.Printf("webrtc: NAT 1:1 IP set to %s", ip)
}

// SetUDPPortRange sets the ephemeral UDP port range for WebRTC.
func SetUDPPortRange(min, max uint16) {
	udpPortMin = min
	udpPortMax = max
	log.Printf("webrtc: UDP port range set to %d-%d", min, max)
}

func getAPI() *webrtc.API {
	apiOnce.Do(func() {
		m := &webrtc.MediaEngine{}
		if err := m.RegisterDefaultCodecs(); err != nil {
			log.Fatal("webrtc: failed to register codecs:", err)
		}

		i := &interceptor.Registry{}
		if err := webrtc.RegisterDefaultInterceptors(m, i); err != nil {
			log.Fatal("webrtc: failed to register interceptors:", err)
		}

		pliFactory, err := intervalpli.NewReceiverInterceptor()
		if err != nil {
			log.Fatal("webrtc: failed to create PLI interceptor:", err)
		}
		i.Add(pliFactory)

		opts := []func(*webrtc.API){
			webrtc.WithMediaEngine(m),
			webrtc.WithInterceptorRegistry(i),
		}

		if natIP != "" || udpPortMin > 0 {
			s := webrtc.SettingEngine{}
			if natIP != "" {
				s.SetNAT1To1IPs([]string{natIP}, webrtc.ICECandidateTypeHost)
			}
			if udpPortMin > 0 && udpPortMax > 0 {
				s.SetEphemeralUDPPortRange(udpPortMin, udpPortMax)
			}
			opts = append(opts, webrtc.WithSettingEngine(s))
		}

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
	iceServers := []webrtc.ICEServer{
		{URLs: []string{"stun:stun.l.google.com:19302"}},
		{URLs: []string{"stun:stun1.l.google.com:19302"}},
	}
	if turnCreds != nil {
		iceServers = append(iceServers, webrtc.ICEServer{
			URLs:       turnCreds.URIs,
			Username:   turnCreds.Username,
			Credential: turnCreds.Password,
		})
	}
	return webrtc.Configuration{ICEServers: iceServers}
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

	s.mu.RLock()
	existingPeer, exists := s.peers[userID]
	s.mu.RUnlock()

	if exists {
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
	}

	// Video tracks: if expectCamera flag is set, treat as camera; otherwise screen
	pc.OnTrack(func(track *webrtc.TrackRemote, _ *webrtc.RTPReceiver) {
		switch track.Kind() {
		case webrtc.RTPCodecTypeAudio:
			s.handleAudioTrack(peer, userID, username, track)
		case webrtc.RTPCodecTypeVideo:
			if peer.expectCamera && peer.cameraTrack == nil {
				peer.expectCamera = false
				s.handleCameraTrack(peer, userID, username, track)
			} else {
				s.handleScreenTrack(peer, userID, username, track)
			}
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
	pc.OnConnectionStateChange(func(state webrtc.PeerConnectionState) {
		log.Printf("webrtc: peer %d (%s) connection state: %s", userID, username, state.String())
		if state == webrtc.PeerConnectionStateConnected {
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
				log.Printf("webrtc: failed to add existing audio %d->%d: %v", srcID, userID, err)
			} else {
				needsRenego = true
			}
		}
		if ep.screenTrack != nil {
			if err := s.addOutputTrack(peer, srcID, ep.screenTrack, "screen"); err != nil {
				log.Printf("webrtc: failed to add existing screen %d->%d: %v", srcID, userID, err)
			} else {
				needsRenego = true
				s.sendPLI(ep, ep.screenTrack)
			}
		}
		if ep.cameraTrack != nil {
			if err := s.addOutputTrack(peer, srcID, ep.cameraTrack, "camera"); err != nil {
				log.Printf("webrtc: failed to add existing camera %d->%d: %v", srcID, userID, err)
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

	if peer.PC.ConnectionState() != webrtc.PeerConnectionStateClosed {
		peer.PC.Close()
	}
	log.Printf("webrtc: removed peer %d (%s)", userID, peer.Username)
}

// --- Track handlers ---

func (s *SFU) handleAudioTrack(peer *Peer, userID int64, username string, track *webrtc.TrackRemote) {
	log.Printf("webrtc: received audio track from user %d (%s)", userID, username)
	s.mu.Lock()
	peer.audioTrack = track
	s.mu.Unlock()

	s.mu.RLock()
	for otherID, op := range s.peers {
		if otherID == userID {
			continue
		}
		if err := s.addOutputTrack(op, userID, track, "audio"); err != nil {
			log.Printf("webrtc: failed to add audio %d->%d: %v", userID, otherID, err)
		} else {
			s.renegotiate(op)
		}
	}
	s.mu.RUnlock()

	s.forwardRTP(track, userID, "audio", 1500)
}

func (s *SFU) handleScreenTrack(peer *Peer, userID int64, username string, track *webrtc.TrackRemote) {
	log.Printf("webrtc: received screen track from user %d (%s)", userID, username)
	s.mu.Lock()
	peer.screenTrack = track
	s.mu.Unlock()

	s.mu.RLock()
	for otherID, op := range s.peers {
		if otherID == userID {
			continue
		}
		if err := s.addOutputTrack(op, userID, track, "screen"); err != nil {
			log.Printf("webrtc: failed to add screen %d->%d: %v", userID, otherID, err)
		} else {
			s.renegotiate(op)
		}
	}
	s.mu.RUnlock()

	s.sendPLI(peer, track)
	s.forwardRTP(track, userID, "screen", 4096)

	// Track ended — cleanup
	log.Printf("webrtc: screen track ended for user %d (%s)", userID, username)
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
	log.Printf("webrtc: received camera track from user %d (%s)", userID, username)
	s.mu.Lock()
	peer.cameraTrack = track
	s.mu.Unlock()

	s.mu.RLock()
	for otherID, op := range s.peers {
		if otherID == userID {
			continue
		}
		if err := s.addOutputTrack(op, userID, track, "camera"); err != nil {
			log.Printf("webrtc: failed to add camera %d->%d: %v", userID, otherID, err)
		} else {
			s.renegotiate(op)
		}
	}
	s.mu.RUnlock()

	s.sendPLI(peer, track)
	s.forwardRTP(track, userID, "camera", 4096)

	// Track ended — cleanup
	log.Printf("webrtc: camera track ended for user %d (%s)", userID, username)
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
			log.Printf("webrtc: %s track read ended for user %d: %v", kind, userID, err)
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
					log.Printf("webrtc: %s write to user %d failed: %v", kind, otherID, writeErr)
				}
			}
			op.mu.Unlock()
		}
		s.mu.RUnlock()
	}
}

func (s *SFU) addOutputTrack(destPeer *Peer, srcUserID int64, srcTrack *webrtc.TrackRemote, kind string) error {
	// StreamID = kind so client can distinguish camera from screen
	// TrackID = kind-srcUserID to make it unique per source user
	streamID := kind
	trackID := fmt.Sprintf("%s-%d", kind, srcUserID)
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
	return nil
}

func (s *SFU) sendPLI(peer *Peer, track *webrtc.TrackRemote) {
	if peer.PC.ConnectionState() == webrtc.PeerConnectionStateClosed {
		return
	}
	if err := peer.PC.WriteRTCP([]rtcp.Packet{
		&rtcp.PictureLossIndication{MediaSSRC: uint32(track.SSRC())},
	}); err != nil {
		log.Printf("webrtc: failed to send PLI for user %d: %v", peer.UserID, err)
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

func (s *SFU) renegotiate(peer *Peer) {
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
			log.Printf("webrtc: waiting for stable signaling state for user %d (current: %s)", peer.UserID, peer.PC.SignalingState())
		}
		peer.negoMu.Unlock()
		time.Sleep(100 * time.Millisecond)
		peer.negoMu.Lock()
	}
	if peer.PC.SignalingState() != webrtc.SignalingStateStable {
		log.Printf("webrtc: renegotiation timeout for user %d, signaling state: %s", peer.UserID, peer.PC.SignalingState())
		return
	}

	offer, err := peer.PC.CreateOffer(nil)
	if err != nil {
		log.Printf("webrtc: renegotiate offer failed for user %d: %v", peer.UserID, err)
		return
	}
	if err := peer.PC.SetLocalDescription(offer); err != nil {
		log.Printf("webrtc: renegotiate setlocal failed for user %d: %v", peer.UserID, err)
		return
	}

	log.Printf("webrtc: sending renegotiation offer to user %d", peer.UserID)
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

	log.Printf("webrtc: received answer from user %d, signaling state: %s", userID, peer.PC.SignalingState())
	return peer.PC.SetRemoteDescription(webrtc.SessionDescription{
		Type: webrtc.SDPTypeAnswer,
		SDP:  answerSDP,
	})
}
