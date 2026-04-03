package turn

import (
	"crypto/rand"
	"crypto/tls"
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
	TLSPort  int    // TCP/TLS port for TURNS (default 5349, 0 = disabled)
	TLSHost  string // Domain name for TURNS URI (must match TLS cert SAN)
	CertFile string // TLS certificate file path
	KeyFile  string // TLS private key file path
	Realm    string
	secret   string
}

// Server wraps a pion TURN server.
type Server struct {
	cfg    Config
	server *pionTurn.Server
	tlsOn  bool
}

func generateSecret() string {
	b := make([]byte, 32)
	rand.Read(b)
	return hex.EncodeToString(b)
}

// DefaultConfig returns a Config with sensible defaults.
func DefaultConfig(publicIP string) Config {
	return Config{
		PublicIP: publicIP,
		Port:     3478,
		TLSPort:  5349,
		Realm:    "vocipher",
		secret:   generateSecret(),
	}
}

// Start creates and starts the embedded TURN/TURNS server.
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

	username := "vocipher"
	key := pionTurn.GenerateAuthKey(username, cfg.Realm, cfg.secret)

	authHandler := func(u, realm string, srcAddr net.Addr) ([]byte, bool) {
		if u == username {
			return key, true
		}
		return nil, false
	}

	relayGen := &pionTurn.RelayAddressGeneratorStatic{
		RelayAddress: net.ParseIP(cfg.PublicIP),
		Address:      "0.0.0.0",
	}

	serverCfg := pionTurn.ServerConfig{
		Realm:       cfg.Realm,
		AuthHandler: authHandler,
	}

	// UDP listener (standard TURN)
	udpAddr := fmt.Sprintf("0.0.0.0:%d", cfg.Port)
	udpListener, err := net.ListenPacket("udp4", udpAddr)
	if err != nil {
		return nil, fmt.Errorf("turn: failed to listen UDP on %s: %w", udpAddr, err)
	}
	serverCfg.PacketConnConfigs = []pionTurn.PacketConnConfig{
		{
			PacketConn:            udpListener,
			RelayAddressGenerator: relayGen,
		},
	}
	log.Printf("TURN server listening on UDP %s (public IP: %s)", udpAddr, cfg.PublicIP)

	// TLS listener (TURNS) — optional
	tlsOn := false
	if cfg.TLSPort > 0 && cfg.CertFile != "" && cfg.KeyFile != "" {
		cert, err := tls.LoadX509KeyPair(cfg.CertFile, cfg.KeyFile)
		if err != nil {
			udpListener.Close()
			return nil, fmt.Errorf("turn: failed to load TLS certificate: %w", err)
		}

		tlsAddr := fmt.Sprintf("0.0.0.0:%d", cfg.TLSPort)
		tlsListener, err := tls.Listen("tcp4", tlsAddr, &tls.Config{
			Certificates: []tls.Certificate{cert},
			MinVersion:   tls.VersionTLS12,
		})
		if err != nil {
			udpListener.Close()
			return nil, fmt.Errorf("turn: failed to listen TLS on %s: %w", tlsAddr, err)
		}

		serverCfg.ListenerConfigs = []pionTurn.ListenerConfig{
			{
				Listener:              tlsListener,
				RelayAddressGenerator: relayGen,
			},
		}
		tlsOn = true
		log.Printf("TURNS server listening on TLS %s", tlsAddr)
	}

	srv, err := pionTurn.NewServer(serverCfg)
	if err != nil {
		udpListener.Close()
		return nil, fmt.Errorf("turn: failed to create server: %w", err)
	}

	return &Server{cfg: cfg, server: srv, tlsOn: tlsOn}, nil
}

// Credentials returns TURN username, password, and URIs for ICE config.
// Returns both turn: (UDP) and turns: (TLS) URIs if TLS is enabled.
func (s *Server) Credentials() (username, password string, uris []string) {
	username = "vocipher"
	password = s.cfg.secret
	uris = []string{
		fmt.Sprintf("turn:%s:%d?transport=udp", s.cfg.PublicIP, s.cfg.Port),
	}
	if s.tlsOn {
		turnsHost := s.cfg.TLSHost
		if turnsHost == "" {
			turnsHost = s.cfg.PublicIP
		}
		uris = append(uris, fmt.Sprintf("turns:%s:%d?transport=tcp", turnsHost, s.cfg.TLSPort))
	}
	return
}

// Close stops the TURN server.
func (s *Server) Close() error {
	if s.server != nil {
		return s.server.Close()
	}
	return nil
}
