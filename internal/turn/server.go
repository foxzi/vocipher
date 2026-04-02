package turn

import (
	"crypto/rand"
	"encoding/hex"
	"fmt"
	"log"
	"net"

	pionTurn "github.com/pion/turn/v4"
)

// Config holds TURN server configuration.
type Config struct {
	PublicIP string // External IP that clients use to reach the TURN server
	Port     int    // UDP port for TURN (default 3478)
	Realm    string
	secret   string // shared secret for auth
}

// Server wraps a pion TURN server.
type Server struct {
	cfg    Config
	server *pionTurn.Server
}

// generateSecret creates a random shared secret.
func generateSecret() string {
	b := make([]byte, 32)
	rand.Read(b)
	return hex.EncodeToString(b)
}

// DefaultConfig returns a Config with sensible defaults.
// publicIP must be provided for TURN to work.
func DefaultConfig(publicIP string) Config {
	return Config{
		PublicIP: publicIP,
		Port:     3478,
		Realm:    "vocipher",
		secret:   generateSecret(),
	}
}

// Start creates and starts the embedded TURN server.
func Start(cfg Config) (*Server, error) {
	if cfg.PublicIP == "" {
		return nil, fmt.Errorf("turn: public IP is required")
	}
	if cfg.Port == 0 {
		cfg.Port = 3478
	}
	if cfg.Realm == "" {
		cfg.Realm = "vocipher"
	}
	if cfg.secret == "" {
		cfg.secret = generateSecret()
	}

	addr := fmt.Sprintf("0.0.0.0:%d", cfg.Port)
	udpListener, err := net.ListenPacket("udp4", addr)
	if err != nil {
		return nil, fmt.Errorf("turn: failed to listen on %s: %w", addr, err)
	}

	// Single static user for all WebRTC clients
	username := "vocipher"
	key := pionTurn.GenerateAuthKey(username, cfg.Realm, cfg.secret)

	srv, err := pionTurn.NewServer(pionTurn.ServerConfig{
		Realm: cfg.Realm,
		AuthHandler: func(u, realm string, srcAddr net.Addr) ([]byte, bool) {
			if u == username {
				return key, true
			}
			return nil, false
		},
		PacketConnConfigs: []pionTurn.PacketConnConfig{
			{
				PacketConn: udpListener,
				RelayAddressGenerator: &pionTurn.RelayAddressGeneratorStatic{
					RelayAddress: net.ParseIP(cfg.PublicIP),
					Address:      "0.0.0.0",
				},
			},
		},
	})
	if err != nil {
		udpListener.Close()
		return nil, fmt.Errorf("turn: failed to create server: %w", err)
	}

	log.Printf("TURN server started on %s (public IP: %s)", addr, cfg.PublicIP)

	return &Server{cfg: cfg, server: srv}, nil
}

// Credentials returns the TURN username, password and URI for ICE config.
func (s *Server) Credentials() (username, password, uri string) {
	return "vocipher", s.cfg.secret, fmt.Sprintf("turn:%s:%d", s.cfg.PublicIP, s.cfg.Port)
}

// Close stops the TURN server.
func (s *Server) Close() error {
	if s.server != nil {
		return s.server.Close()
	}
	return nil
}
