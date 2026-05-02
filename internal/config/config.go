package config

import (
	"log"
	"os"
	"strconv"

	"gopkg.in/yaml.v3"
)

type Config struct {
	Server   ServerConfig   `yaml:"server"`
	Database DatabaseConfig `yaml:"database"`
	WebRTC   WebRTCConfig   `yaml:"webrtc"`
	TURN     TURNConfig     `yaml:"turn"`
	Auth     AuthConfig     `yaml:"auth"`
	OAuth    OAuthConfig    `yaml:"oauth"`
	Security SecurityConfig `yaml:"security"`
}

type ServerConfig struct {
	Addr         string `yaml:"addr"`
	ReadTimeout  int    `yaml:"read_timeout"`  // seconds
	WriteTimeout int    `yaml:"write_timeout"` // seconds
	IdleTimeout  int    `yaml:"idle_timeout"`  // seconds
	LogLevel     string `yaml:"log_level"`     // debug, info, warn, error
}

type DatabaseConfig struct {
	Path              string `yaml:"path"`
	ChatRetentionDays int    `yaml:"chat_retention_days"` // 0 = keep forever
}

type WebRTCConfig struct {
	NATIP        string `yaml:"nat_ip"`
	UDPPortMin   uint16 `yaml:"udp_port_min"`
	UDPPortMax   uint16 `yaml:"udp_port_max"`
	ICETCPPort   uint16 `yaml:"ice_tcp_port"`   // TCP mux port for ICE TCP candidates; 0 disables
	MaxMessageKB int    `yaml:"max_message_kb"` // WebSocket message size limit
}

type TURNConfig struct {
	Enabled  bool   `yaml:"enabled"`
	IP       string `yaml:"ip"`
	Port     int    `yaml:"port"`      // UDP TURN port (default 3478)
	TLSPort  int    `yaml:"tls_port"`  // TLS TURNS port (default 5349, 0 = disabled)
	TLSHost  string `yaml:"tls_host"`  // Domain name for TURNS URI (must match TLS cert)
	CertFile string `yaml:"cert_file"` // TLS certificate file for TURNS
	KeyFile  string `yaml:"key_file"`  // TLS private key file for TURNS
}

type AuthConfig struct {
	SessionDays         int  `yaml:"session_days"`
	MinPassword         int  `yaml:"min_password"`
	CookieSecure        bool `yaml:"cookie_secure"`
	RegistrationEnabled bool `yaml:"registration_enabled"`
	RateLimitRPS        int  `yaml:"rate_limit_rps"`
	RateLimitBurst      int  `yaml:"rate_limit_burst"`
}

type SecurityConfig struct {
	AllowedOrigins []string `yaml:"allowed_origins"` // empty = same-origin only
}

type OAuthProvider struct {
	Name         string   `yaml:"name"` // display name ("Google", "GitHub", etc.)
	ClientID     string   `yaml:"client_id"`
	ClientSecret string   `yaml:"client_secret"`
	AuthURL      string   `yaml:"auth_url"`     // authorization endpoint
	TokenURL     string   `yaml:"token_url"`    // token endpoint
	UserInfoURL  string   `yaml:"userinfo_url"` // userinfo endpoint (for getting email/name)
	Scopes       []string `yaml:"scopes"`
	AutoActivate bool     `yaml:"auto_activate"` // auto-activate OAuth users (default: true)
}

type OAuthConfig struct {
	Enabled   bool            `yaml:"enabled"`
	Providers []OAuthProvider `yaml:"providers"`
}

// Default returns config with sensible defaults.
func Default() *Config {
	return &Config{
		Server: ServerConfig{
			Addr:         ":8090",
			ReadTimeout:  15,
			WriteTimeout: 30,
			IdleTimeout:  120,
			LogLevel:     "info",
		},
		Database: DatabaseConfig{
			Path:              "vocala.db",
			ChatRetentionDays: 30,
		},
		WebRTC: WebRTCConfig{
			UDPPortMin:   40000,
			UDPPortMax:   40200,
			ICETCPPort:   40201,
			MaxMessageKB: 512,
		},
		TURN: TURNConfig{
			Port:    3478,
			TLSPort: 5349,
		},
		Auth: AuthConfig{
			SessionDays:         30,
			MinPassword:         8,
			CookieSecure:        false,
			RegistrationEnabled: true,
			RateLimitRPS:        10,
			RateLimitBurst:      20,
		},
	}
}

// Load reads config from a YAML file, then applies env var overrides.
func Load(path string) *Config {
	cfg := Default()

	if path != "" {
		data, err := os.ReadFile(path)
		if err != nil {
			if !os.IsNotExist(err) {
				log.Fatalf("config: failed to read %s: %v", path, err)
			}
			log.Printf("config: %s not found, using defaults", path)
		} else {
			if err := yaml.Unmarshal(data, cfg); err != nil {
				log.Fatalf("config: failed to parse %s: %v", path, err)
			}
			log.Printf("config: loaded from %s", path)
		}
	}

	// Env vars override YAML values
	envOverrides(cfg)

	return cfg
}

func envOverrides(cfg *Config) {
	if v := os.Getenv("VOCALA_ADDR"); v != "" {
		cfg.Server.Addr = v
	}
	if v := os.Getenv("VOCALA_DB_PATH"); v != "" {
		cfg.Database.Path = v
	}
	if v := os.Getenv("VOCALA_NAT_IP"); v != "" {
		cfg.WebRTC.NATIP = v
	}
	if v := os.Getenv("VOCALA_TURN_IP"); v != "" {
		cfg.TURN.Enabled = true
		cfg.TURN.IP = v
	}
	if v := os.Getenv("VOCALA_TURN_PORT"); v != "" {
		if p, err := strconv.Atoi(v); err == nil {
			cfg.TURN.Port = p
		}
	}
	if v := os.Getenv("VOCALA_COOKIE_SECURE"); v == "true" || v == "1" {
		cfg.Auth.CookieSecure = true
	}
	if v := os.Getenv("VOCALA_REGISTRATION"); v == "false" || v == "0" {
		cfg.Auth.RegistrationEnabled = false
	} else if v == "true" || v == "1" {
		cfg.Auth.RegistrationEnabled = true
	}
}
