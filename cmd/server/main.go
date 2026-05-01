package main

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"html/template"
	"net"
	"net/http"
	"os"
	"os/signal"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"syscall"
	"time"

	"github.com/foxzi/vocala/internal/auth"
	"github.com/foxzi/vocala/internal/channel"
	"github.com/foxzi/vocala/internal/config"
	"github.com/foxzi/vocala/internal/database"
	"github.com/foxzi/vocala/internal/logger"
	"github.com/foxzi/vocala/internal/signaling"
	embeddedTurn "github.com/foxzi/vocala/internal/turn"
	rtc "github.com/foxzi/vocala/internal/webrtc"
	"golang.org/x/oauth2"
	"golang.org/x/term"
	"golang.org/x/time/rate"
)

// Set via ldflags: -ldflags "-X main.version=0.1.0"
var version = "dev"

var templates map[string]*template.Template
var cacheBust = fmt.Sprintf("%d", time.Now().Unix())
var cfg *config.Config

// Context key for authenticated user (#15 — avoid double DB query)
type ctxKey string

const userCtxKey ctxKey = "user"

var funcMap = template.FuncMap{
	"toJSON": func(v any) template.JS {
		b, _ := json.Marshal(v)
		return template.JS(b)
	},
}

func loadTemplates() map[string]*template.Template {
	layoutFile := filepath.Join("web", "templates", "layout.html")
	pages := []string{"login.html", "register.html", "app.html", "admin.html", "guest.html", "guest-app.html"}
	t := make(map[string]*template.Template, len(pages))
	for _, page := range pages {
		t[page] = template.Must(
			template.New("").Funcs(funcMap).ParseFiles(layoutFile, filepath.Join("web", "templates", page)),
		)
	}
	return t
}

// --- Rate limiter (#6) ---

type limiterEntry struct {
	lim      *rate.Limiter
	lastSeen time.Time
}

type ipLimiter struct {
	mu       sync.Mutex
	limiters map[string]*limiterEntry
}

func newIPLimiter() *ipLimiter {
	l := &ipLimiter{limiters: make(map[string]*limiterEntry)}
	go l.cleanup()
	return l
}

func (l *ipLimiter) get(ip string) *rate.Limiter {
	l.mu.Lock()
	defer l.mu.Unlock()
	if e, ok := l.limiters[ip]; ok {
		e.lastSeen = time.Now()
		return e.lim
	}
	rps := 10
	burst := 20
	if cfg != nil {
		rps = cfg.Auth.RateLimitRPS
		burst = cfg.Auth.RateLimitBurst
	}
	lim := rate.NewLimiter(rate.Limit(rps), burst)
	l.limiters[ip] = &limiterEntry{lim: lim, lastSeen: time.Now()}
	return lim
}

func (l *ipLimiter) cleanup() {
	for {
		time.Sleep(10 * time.Minute)
		l.mu.Lock()
		cutoff := time.Now().Add(-30 * time.Minute)
		for ip, e := range l.limiters {
			if e.lastSeen.Before(cutoff) {
				delete(l.limiters, ip)
			}
		}
		l.mu.Unlock()
	}
}

var limiter = newIPLimiter()

func clientIP(r *http.Request) string {
	// Only trust X-Forwarded-For when behind reverse proxy (RemoteAddr is loopback)
	if fwd := r.Header.Get("X-Forwarded-For"); fwd != "" {
		host, _, _ := net.SplitHostPort(r.RemoteAddr)
		if host == "127.0.0.1" || host == "::1" {
			// Use first IP from X-Forwarded-For (set by trusted proxy)
			if idx := strings.Index(fwd, ","); idx > 0 {
				return strings.TrimSpace(fwd[:idx])
			}
			return strings.TrimSpace(fwd)
		}
	}
	return r.RemoteAddr
}

func rateLimitMiddleware(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		ip := clientIP(r)
		if !limiter.get(ip).Allow() {
			http.Error(w, "too many requests", http.StatusTooManyRequests)
			return
		}
		next.ServeHTTP(w, r)
	})
}

// --- Security headers (#9) ---

func buildICEServers() []map[string]any {
	servers := []map[string]any{
		{"urls": "stun:stun.l.google.com:19302"},
		{"urls": "stun:stun1.l.google.com:19302"},
	}
	if creds := rtc.GetTURNCredentials(); creds != nil {
		servers = append(servers, map[string]any{
			"urls":       creds.URIs,
			"username":   creds.Username,
			"credential": creds.Password,
		})
	}
	return servers
}

var httpsDetected sync.Once

func securityHeaders(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("X-Content-Type-Options", "nosniff")
		w.Header().Set("X-Frame-Options", "DENY")
		w.Header().Set("Referrer-Policy", "strict-origin-when-cross-origin")
		w.Header().Set("Permissions-Policy", "microphone=(self), camera=(self)")
		// Auto-detect HTTPS and enable secure cookies
		if cfg != nil && !cfg.Auth.CookieSecure {
			if r.Header.Get("X-Forwarded-Proto") == "https" || r.TLS != nil {
				httpsDetected.Do(func() {
					cfg.Auth.CookieSecure = true
					logger.Info("HTTPS detected, cookie_secure auto-enabled")
				})
			}
		}
		next.ServeHTTP(w, r)
	})
}

// --- CSRF protection (#4) ---

func generateCSRFToken() string {
	b := make([]byte, 32)
	rand.Read(b)
	return hex.EncodeToString(b)
}

func csrfProtect(next http.HandlerFunc) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		if r.Method == http.MethodPost {
			cookieToken, err := r.Cookie("csrf_token")
			formToken := r.FormValue("csrf_token")
			if err != nil || cookieToken.Value == "" || cookieToken.Value != formToken {
				http.Error(w, "invalid CSRF token", http.StatusForbidden)
				return
			}
		}
		next(w, r)
	}
}

func setCSRFCookie(w http.ResponseWriter, r *http.Request) string {
	if c, err := r.Cookie("csrf_token"); err == nil && c.Value != "" {
		return c.Value
	}
	token := generateCSRFToken()
	secure := cfg != nil && cfg.Auth.CookieSecure
	http.SetCookie(w, &http.Cookie{
		Name:     "csrf_token",
		Value:    token,
		Path:     "/",
		HttpOnly: false, // JS needs to read it for HTMX
		Secure:   secure,
		SameSite: http.SameSiteStrictMode,
		MaxAge:   86400,
	})
	return token
}

// --- Cookie helper (#14) ---

func sessionCookie(token string, maxAge int) *http.Cookie {
	secure := false
	if cfg != nil {
		secure = cfg.Auth.CookieSecure
	}
	return &http.Cookie{
		Name:     "session",
		Value:    token,
		Path:     "/",
		HttpOnly: true,
		SameSite: http.SameSiteLaxMode,
		Secure:   secure,
		MaxAge:   maxAge,
	}
}

// --- Main ---

func readPasswordStdin() ([]byte, error) {
	return term.ReadPassword(int(os.Stdin.Fd()))
}

func runCLI(args []string) {
	switch args[0] {
	case "user-add":
		if len(args) < 2 {
			fmt.Println("Usage: vocala user-add USERNAME [PASSWORD] [--admin] [--active]")
			fmt.Println("If PASSWORD is omitted, it will be read from stdin.")
			os.Exit(1)
		}
		username := args[1]
		password := ""
		if len(args) >= 3 && !strings.HasPrefix(args[2], "--") {
			password = args[2]
		} else {
			fmt.Print("Password: ")
			pwBytes, err := readPasswordStdin()
			if err != nil {
				fmt.Printf("Failed to read password: %v\n", err)
				os.Exit(1)
			}
			password = string(pwBytes)
			fmt.Println()
		}
		if len(password) < cfg.Auth.MinPassword {
			fmt.Printf("Password must be at least %d characters\n", cfg.Auth.MinPassword)
			os.Exit(1)
		}
		user, err := auth.Register(username, password)
		if err != nil {
			fmt.Printf("Failed to create user: %v\n", err)
			os.Exit(1)
		}
		for _, a := range args[3:] {
			switch a {
			case "--admin":
				auth.SetUserAdmin(user.ID, true)
			case "--active":
				auth.SetUserActive(user.ID, true)
			}
		}
		fmt.Printf("Created user: %s (id=%d)\n", username, user.ID)

	case "user-delete":
		if len(args) < 2 {
			fmt.Println("Usage: vocala user-delete USERNAME")
			os.Exit(1)
		}
		u, err := auth.GetUserByUsername(args[1])
		if err != nil {
			fmt.Printf("User not found: %s\n", args[1])
			os.Exit(1)
		}
		if err := auth.DeleteUser(u.ID); err != nil {
			fmt.Printf("Failed to delete user: %v\n", err)
			os.Exit(1)
		}
		fmt.Printf("Deleted user: %s\n", u.Username)

	case "user-activate":
		if len(args) < 2 {
			fmt.Println("Usage: vocala user-activate USERNAME")
			os.Exit(1)
		}
		u, err := auth.GetUserByUsername(args[1])
		if err != nil {
			fmt.Printf("User not found: %s\n", args[1])
			os.Exit(1)
		}
		auth.SetUserActive(u.ID, true)
		fmt.Printf("Activated user: %s\n", u.Username)

	case "user-deactivate":
		if len(args) < 2 {
			fmt.Println("Usage: vocala user-deactivate USERNAME")
			os.Exit(1)
		}
		u, err := auth.GetUserByUsername(args[1])
		if err != nil {
			fmt.Printf("User not found: %s\n", args[1])
			os.Exit(1)
		}
		auth.SetUserActive(u.ID, false)
		fmt.Printf("Deactivated user: %s\n", u.Username)

	case "user-admin":
		if len(args) < 2 {
			fmt.Println("Usage: vocala user-admin USERNAME [--revoke]")
			os.Exit(1)
		}
		u, err := auth.GetUserByUsername(args[1])
		if err != nil {
			fmt.Printf("User not found: %s\n", args[1])
			os.Exit(1)
		}
		revoke := len(args) > 2 && args[2] == "--revoke"
		auth.SetUserAdmin(u.ID, !revoke)
		if revoke {
			fmt.Printf("Revoked admin from: %s\n", u.Username)
		} else {
			fmt.Printf("Granted admin to: %s\n", u.Username)
		}

	case "user-password":
		if len(args) < 2 {
			fmt.Println("Usage: vocala user-password USERNAME [NEWPASSWORD]")
			fmt.Println("If NEWPASSWORD is omitted, it will be read from stdin.")
			os.Exit(1)
		}
		u, err := auth.GetUserByUsername(args[1])
		if err != nil {
			fmt.Printf("User not found: %s\n", args[1])
			os.Exit(1)
		}
		newPass := ""
		if len(args) >= 3 {
			newPass = args[2]
		} else {
			fmt.Print("New password: ")
			pwBytes, err := readPasswordStdin()
			if err != nil {
				fmt.Printf("Failed to read password: %v\n", err)
				os.Exit(1)
			}
			newPass = string(pwBytes)
			fmt.Println()
		}
		if len(newPass) < cfg.Auth.MinPassword {
			fmt.Printf("Password must be at least %d characters\n", cfg.Auth.MinPassword)
			os.Exit(1)
		}
		if err := auth.SetUserPassword(u.ID, newPass); err != nil {
			fmt.Printf("Failed to reset password: %v\n", err)
			os.Exit(1)
		}
		fmt.Printf("Password reset for: %s\n", u.Username)

	case "user-list":
		users, err := auth.ListUsers()
		if err != nil {
			fmt.Printf("Failed to list users: %v\n", err)
			os.Exit(1)
		}
		fmt.Printf("%-4s %-20s %-8s %-8s %s\n", "ID", "USERNAME", "ADMIN", "ACTIVE", "CREATED")
		for _, u := range users {
			fmt.Printf("%-4d %-20s %-8v %-8v %s\n", u.ID, u.Username, u.IsAdmin, u.IsActive, u.CreatedAt.Format("2006-01-02 15:04"))
		}

	default:
		fmt.Printf("Unknown command: %s\n", args[0])
		fmt.Println("Available commands:")
		fmt.Println("  user-add USERNAME PASSWORD [--admin] [--active]")
		fmt.Println("  user-delete USERNAME")
		fmt.Println("  user-activate USERNAME")
		fmt.Println("  user-deactivate USERNAME")
		fmt.Println("  user-admin USERNAME [--revoke]")
		fmt.Println("  user-password USERNAME NEWPASSWORD")
		fmt.Println("  user-list")
		os.Exit(1)
	}
}

func main() {
	showVersion := flag.Bool("version", false, "print version and exit")
	configPath := flag.String("config", "config.yaml", "path to config file")
	flag.Parse()

	if *showVersion {
		fmt.Println("vocala", version)
		os.Exit(0)
	}

	// CLI subcommands: vocala -config ... user-add USERNAME PASSWORD
	args := flag.Args()
	if len(args) > 0 {
		cfg = config.Load(*configPath)
		logger.SetLevel(logger.ParseLevel(cfg.Server.LogLevel))
		database.Init(cfg.Database.Path)
		runCLI(args)
		return
	}

	cfg = config.Load(*configPath)
	logger.SetLevel(logger.ParseLevel(cfg.Server.LogLevel))

	database.Init(cfg.Database.Path)

	// Set NAT IP for WebRTC ICE candidates (required in Docker)
	if cfg.WebRTC.NATIP != "" {
		rtc.SetNATIP(cfg.WebRTC.NATIP)
	}
	if cfg.WebRTC.UDPPortMin > 0 && cfg.WebRTC.UDPPortMax > 0 {
		rtc.SetUDPPortRange(cfg.WebRTC.UDPPortMin, cfg.WebRTC.UDPPortMax)
	}

	// Set WebSocket message size limit
	if cfg.WebRTC.MaxMessageKB > 0 {
		signaling.SetMaxMessageSize(int64(cfg.WebRTC.MaxMessageKB) * 1024)
	}

	// Start embedded TURN/TURNS server if configured
	if cfg.TURN.Enabled && cfg.TURN.IP != "" {
		turnCfg := embeddedTurn.DefaultConfig(cfg.TURN.IP)
		if cfg.TURN.Port > 0 {
			turnCfg.Port = cfg.TURN.Port
		}
		turnCfg.TLSPort = cfg.TURN.TLSPort
		turnCfg.TLSHost = cfg.TURN.TLSHost
		turnCfg.CertFile = cfg.TURN.CertFile
		turnCfg.KeyFile = cfg.TURN.KeyFile

		turnServer, err := embeddedTurn.Start(turnCfg)
		if err != nil {
			logger.Fatal("failed to start TURN server:", err)
		}
		defer turnServer.Close()

		username, password, uris := turnServer.Credentials()
		rtc.SetTURNCredentials(uris, username, password)
		logger.Info("TURN enabled: uris=%v", uris)
	}

	// Periodic session cleanup
	go func() {
		for {
			auth.CleanExpiredSessions()
			time.Sleep(1 * time.Hour)
		}
	}()

	// Periodic chat message cleanup
	if cfg.Database.ChatRetentionDays > 0 {
		go func() {
			for {
				if n, err := database.CleanupOldMessages(cfg.Database.ChatRetentionDays); err != nil {
					logger.Error("chat cleanup error: %v", err)
				} else if n > 0 {
					logger.Info("chat cleanup: removed %d old messages", n)
				}
				time.Sleep(6 * time.Hour)
			}
		}()
	}

	templates = loadTemplates()

	mux := http.NewServeMux()

	// Static files
	mux.Handle("/static/", http.StripPrefix("/static/", http.FileServer(http.Dir("web/static"))))

	// Auth routes
	mux.HandleFunc("/login", handleLogin)
	mux.HandleFunc("/register", handleRegister)
	mux.HandleFunc("/logout", handleLogout)
	mux.HandleFunc("/auth/oauth/", handleOAuth)

	// App routes
	mux.HandleFunc("/", requireAuth(handleApp))
	mux.HandleFunc("/channels", requireAuth(csrfProtect(handleChannels)))
	mux.HandleFunc("/channels/delete", requireAuth(csrfProtect(handleDeleteChannel)))
	mux.HandleFunc("/channels/privacy", requireAuth(csrfProtect(handleChannelPrivacy)))
	mux.HandleFunc("/channels/members", requireAuth(csrfProtect(handleChannelMembers)))
	mux.HandleFunc("/channels/members/add", requireAuth(csrfProtect(handleChannelMemberAdd)))
	mux.HandleFunc("/channels/members/remove", requireAuth(csrfProtect(handleChannelMemberRemove)))
	mux.HandleFunc("/channels/invite", requireAuth(csrfProtect(handleChannelInvite)))
	mux.HandleFunc("/invite/", handleInviteAccept)
	mux.HandleFunc("/api/users", requireAuth(handleAPIUsers))
	mux.HandleFunc("/account/password", requireAuth(csrfProtect(handleChangePassword)))

	// Guest routes
	mux.HandleFunc("/guest/", handleGuest)
	mux.HandleFunc("/guest-app", handleGuestApp)
	mux.HandleFunc("/channels/guest-invite", requireAuth(csrfProtect(handleCreateGuestInvite)))

	// Admin routes
	mux.HandleFunc("/admin", requireAdmin(handleAdmin))
	mux.HandleFunc("/admin/users/create", requireAdmin(csrfProtect(handleAdminCreateUser)))
	mux.HandleFunc("/admin/users/activate", requireAdmin(csrfProtect(handleAdminActivate)))
	mux.HandleFunc("/admin/users/deactivate", requireAdmin(csrfProtect(handleAdminDeactivate)))
	mux.HandleFunc("/admin/users/make-admin", requireAdmin(csrfProtect(handleAdminMakeAdmin)))
	mux.HandleFunc("/admin/users/revoke-admin", requireAdmin(csrfProtect(handleAdminRevokeAdmin)))
	mux.HandleFunc("/admin/users/delete", requireAdmin(csrfProtect(handleAdminDeleteUser)))
	mux.HandleFunc("/admin/users/reset-password", requireAdmin(csrfProtect(handleAdminResetPassword)))

	// WebSocket
	mux.HandleFunc("/ws", signaling.HandleWebSocket)

	handler := securityHeaders(rateLimitMiddleware(mux))

	server := &http.Server{
		Addr:         cfg.Server.Addr,
		Handler:      handler,
		ReadTimeout:  time.Duration(cfg.Server.ReadTimeout) * time.Second,
		WriteTimeout: time.Duration(cfg.Server.WriteTimeout) * time.Second,
		IdleTimeout:  time.Duration(cfg.Server.IdleTimeout) * time.Second,
	}

	// Graceful shutdown
	go func() {
		logger.Info("Vocala server starting on %s", cfg.Server.Addr)
		if err := server.ListenAndServe(); err != http.ErrServerClosed {
			logger.Fatal("server error:", err)
		}
	}()

	quit := make(chan os.Signal, 1)
	signal.Notify(quit, syscall.SIGINT, syscall.SIGTERM)
	<-quit

	logger.Info("Shutting down server...")
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	if err := server.Shutdown(ctx); err != nil {
		logger.Fatal("server shutdown error:", err)
	}
	logger.Info("Server stopped")
}

// --- Middleware ---

func requireAuth(next http.HandlerFunc) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		user := auth.UserFromRequest(r)
		if user == nil {
			http.Redirect(w, r, "/login", http.StatusSeeOther)
			return
		}
		if !user.IsActive {
			http.Redirect(w, r, "/login", http.StatusSeeOther)
			return
		}
		// Store user in context to avoid double DB query (#15)
		ctx := context.WithValue(r.Context(), userCtxKey, user)
		next(w, r.WithContext(ctx))
	}
}

func requireAdmin(next http.HandlerFunc) http.HandlerFunc {
	return requireAuth(func(w http.ResponseWriter, r *http.Request) {
		user := userFromContext(r)
		if user == nil || !user.IsAdmin {
			http.Error(w, "forbidden", http.StatusForbidden)
			return
		}
		next(w, r)
	})
}

func userFromContext(r *http.Request) *auth.User {
	if u, ok := r.Context().Value(userCtxKey).(*auth.User); ok {
		return u
	}
	return nil
}

// --- Handlers ---

func handleLogin(w http.ResponseWriter, r *http.Request) {
	// #10 — method check
	if r.Method == http.MethodGet {
		if user := auth.UserFromRequest(r); user != nil && user.IsActive {
			http.Redirect(w, r, "/", http.StatusSeeOther)
			return
		}
		csrfToken := setCSRFCookie(w, r)
		templates["login.html"].ExecuteTemplate(w, "layout.html", map[string]any{
			"CSRFToken":           csrfToken,
			"Next":                r.URL.Query().Get("next"),
			"RegistrationEnabled": cfg.Auth.RegistrationEnabled,
			"OAuthProviders":      cfg.OAuth.Providers,
		})
		return
	}
	if r.Method != http.MethodPost {
		http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
		return
	}

	// CSRF check for login
	cookieToken, err := r.Cookie("csrf_token")
	formToken := r.FormValue("csrf_token")
	if err != nil || cookieToken.Value == "" || cookieToken.Value != formToken {
		http.Error(w, "invalid CSRF token", http.StatusForbidden)
		return
	}

	username := r.FormValue("username")
	password := r.FormValue("password")

	user, err := auth.Login(username, password)
	if errors.Is(err, auth.ErrNotActive) {
		csrfToken := setCSRFCookie(w, r)
		templates["login.html"].ExecuteTemplate(w, "layout.html", map[string]any{
			"Error":     "Your account is pending activation by an administrator",
			"CSRFToken": csrfToken,
		})
		return
	}
	if err != nil {
		csrfToken := setCSRFCookie(w, r)
		templates["login.html"].ExecuteTemplate(w, "layout.html", map[string]any{
			"Error":          "Invalid username or password",
			"CSRFToken":      csrfToken,
			"OAuthProviders": cfg.OAuth.Providers,
		})
		return
	}

	token, err := auth.CreateSession(user.ID)
	if err != nil {
		logger.Error("failed to create session for user %d: %v", user.ID, err)
		csrfToken := setCSRFCookie(w, r)
		templates["login.html"].ExecuteTemplate(w, "layout.html", map[string]any{
			"Error":     "Something went wrong",
			"CSRFToken": csrfToken,
		})
		return
	}

	http.SetCookie(w, sessionCookie(token, 86400*30))
	next := r.FormValue("next")
	if next == "" || !strings.HasPrefix(next, "/") {
		next = "/"
	}
	http.Redirect(w, r, next, http.StatusSeeOther)
}

func handleRegister(w http.ResponseWriter, r *http.Request) {
	if !cfg.Auth.RegistrationEnabled {
		csrfToken := setCSRFCookie(w, r)
		templates["login.html"].ExecuteTemplate(w, "layout.html", map[string]any{
			"Info":      "Registration is disabled. Contact an administrator.",
			"CSRFToken": csrfToken,
		})
		return
	}
	if r.Method == http.MethodGet {
		if user := auth.UserFromRequest(r); user != nil && user.IsActive {
			http.Redirect(w, r, "/", http.StatusSeeOther)
			return
		}
		csrfToken := setCSRFCookie(w, r)
		templates["register.html"].ExecuteTemplate(w, "layout.html", map[string]any{"CSRFToken": csrfToken})
		return
	}
	if r.Method != http.MethodPost {
		http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
		return
	}

	// CSRF check for register
	cookieToken, err := r.Cookie("csrf_token")
	formToken := r.FormValue("csrf_token")
	if err != nil || cookieToken.Value == "" || cookieToken.Value != formToken {
		http.Error(w, "invalid CSRF token", http.StatusForbidden)
		return
	}

	username := r.FormValue("username")
	password := r.FormValue("password")

	// #16 — stronger password requirement
	if len(username) < 2 || len(password) < 8 {
		csrfToken := setCSRFCookie(w, r)
		templates["register.html"].ExecuteTemplate(w, "layout.html", map[string]any{
			"Error":     "Username must be at least 2 characters, password at least 8",
			"CSRFToken": csrfToken,
		})
		return
	}

	user, err := auth.Register(username, password)
	if err != nil {
		csrfToken := setCSRFCookie(w, r)
		templates["register.html"].ExecuteTemplate(w, "layout.html", map[string]any{
			"Error":     "Username already taken",
			"CSRFToken": csrfToken,
		})
		return
	}

	// If user is not active (not the first user), show pending message instead of creating session
	if !user.IsActive {
		csrfToken := setCSRFCookie(w, r)
		templates["login.html"].ExecuteTemplate(w, "layout.html", map[string]any{
			"Info":      "Account created. Please wait for an administrator to activate your account.",
			"CSRFToken": csrfToken,
		})
		return
	}

	token, err := auth.CreateSession(user.ID)
	if err != nil {
		logger.Error("failed to create session for user %d: %v", user.ID, err)
		csrfToken := setCSRFCookie(w, r)
		templates["register.html"].ExecuteTemplate(w, "layout.html", map[string]any{
			"Error":     "Something went wrong",
			"CSRFToken": csrfToken,
		})
		return
	}

	http.SetCookie(w, sessionCookie(token, 86400*30))
	http.Redirect(w, r, "/", http.StatusSeeOther)
}

func handleLogout(w http.ResponseWriter, r *http.Request) {
	if cookie, err := r.Cookie("session"); err == nil {
		auth.DeleteSession(cookie.Value)
	}
	http.SetCookie(w, sessionCookie("", -1))
	http.Redirect(w, r, "/login", http.StatusSeeOther)
}

func handleApp(w http.ResponseWriter, r *http.Request) {
	// Allow / and /channels/{name}
	path := r.URL.Path
	if path != "/" && !strings.HasPrefix(path, "/channels/") {
		http.NotFound(w, r)
		return
	}
	if r.Method != http.MethodGet {
		http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
		return
	}

	// Extract channel name from URL
	autoJoinChannel := ""
	if strings.HasPrefix(path, "/channels/") {
		autoJoinChannel = strings.TrimPrefix(path, "/channels/")
	}

	user := userFromContext(r)
	channels, err := channel.ListForUser(user.ID, user.IsAdmin)
	if err != nil {
		logger.Error("failed to list channels: %v", err)
		http.Error(w, "internal server error", http.StatusInternalServerError)
		return
	}

	csrfToken := setCSRFCookie(w, r)

	iceServers := buildICEServers()

	data := map[string]any{
		"User":            user,
		"Channels":        channels,
		"CacheBust":       cacheBust,
		"CSRFToken":       csrfToken,
		"ICEServers":      iceServers,
		"AutoJoinChannel": autoJoinChannel,
	}
	templates["app.html"].ExecuteTemplate(w, "layout.html", data)
}

func handleChannels(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
		return
	}

	user := userFromContext(r)

	name := r.FormValue("name")
	isPrivate := r.FormValue("is_private") == "on"
	if name != "" {
		if _, err := channel.Create(name, user.ID, isPrivate); err != nil {
			logger.Error("failed to create channel: %v", err)
		}
	}

	channels, err := channel.ListForUser(user.ID, user.IsAdmin)
	if err != nil {
		logger.Error("failed to list channels: %v", err)
		http.Error(w, "internal server error", http.StatusInternalServerError)
		return
	}

	csrfToken := setCSRFCookie(w, r)
	data := map[string]any{
		"User":      user,
		"Channels":  channels,
		"CSRFToken": csrfToken,
	}

	// Return just the channel list partial for HTMX
	templates["app.html"].ExecuteTemplate(w, "channel-list", data)
}

func handleDeleteChannel(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
		return
	}

	idStr := r.FormValue("id")
	id, err := strconv.ParseInt(idStr, 10, 64)
	if err != nil {
		http.Error(w, "invalid id", http.StatusBadRequest)
		return
	}

	user := userFromContext(r)
	ch, err := channel.GetByID(id)
	if err != nil {
		http.Error(w, "channel not found", http.StatusNotFound)
		return
	}
	if ch.CreatedBy != user.ID && !user.IsAdmin {
		http.Error(w, "forbidden", http.StatusForbidden)
		return
	}

	if err := channel.Delete(id); err != nil {
		logger.Error("failed to delete channel %d: %v", id, err)
	}
	signaling.ClearChannelPreview(id)

	channels, err := channel.ListForUser(user.ID, user.IsAdmin)
	if err != nil {
		logger.Error("failed to list channels: %v", err)
		http.Error(w, "internal server error", http.StatusInternalServerError)
		return
	}

	csrfToken := setCSRFCookie(w, r)
	data := map[string]any{
		"User":      user,
		"Channels":  channels,
		"CSRFToken": csrfToken,
	}
	templates["app.html"].ExecuteTemplate(w, "channel-list", data)
}

func handleChannelPrivacy(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
		return
	}

	id, err := strconv.ParseInt(r.FormValue("id"), 10, 64)
	if err != nil {
		http.Error(w, "invalid id", http.StatusBadRequest)
		return
	}
	isPrivate := r.FormValue("is_private") == "true"

	user := userFromContext(r)
	ch, err := channel.GetByID(id)
	if err != nil {
		http.Error(w, "channel not found", http.StatusNotFound)
		return
	}
	if ch.CreatedBy != user.ID && !user.IsAdmin {
		http.Error(w, "forbidden", http.StatusForbidden)
		return
	}

	if err := channel.SetPrivacy(id, isPrivate); err != nil {
		logger.Error("failed to set privacy on channel %d: %v", id, err)
		http.Error(w, "internal server error", http.StatusInternalServerError)
		return
	}

	channels, err := channel.ListForUser(user.ID, user.IsAdmin)
	if err != nil {
		logger.Error("failed to list channels: %v", err)
		http.Error(w, "internal server error", http.StatusInternalServerError)
		return
	}

	csrfToken := setCSRFCookie(w, r)
	data := map[string]any{
		"User":      user,
		"Channels":  channels,
		"CSRFToken": csrfToken,
	}
	templates["app.html"].ExecuteTemplate(w, "channel-list", data)
}

// --- Admin handlers ---

func handleAdmin(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodGet {
		http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
		return
	}

	users, err := auth.ListUsers()
	if err != nil {
		logger.Error("failed to list users: %v", err)
		http.Error(w, "internal server error", http.StatusInternalServerError)
		return
	}

	csrfToken := setCSRFCookie(w, r)
	data := map[string]any{
		"Users":               users,
		"CSRFToken":           csrfToken,
		"Flash":               r.URL.Query().Get("flash"),
		"RegistrationEnabled": cfg.Auth.RegistrationEnabled,
	}
	templates["admin.html"].ExecuteTemplate(w, "layout.html", data)
}

func adminUserAction(w http.ResponseWriter, r *http.Request, action func(int64) error, flash string) {
	if r.Method != http.MethodPost {
		http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
		return
	}

	userID, err := strconv.ParseInt(r.FormValue("user_id"), 10, 64)
	if err != nil {
		http.Error(w, "invalid user_id", http.StatusBadRequest)
		return
	}

	// Prevent admin from modifying themselves
	currentUser := userFromContext(r)
	if currentUser.ID == userID {
		http.Redirect(w, r, "/admin?flash=Cannot+modify+your+own+account", http.StatusSeeOther)
		return
	}

	if err := action(userID); err != nil {
		logger.Error("admin action failed for user %d: %v", userID, err)
	}

	http.Redirect(w, r, "/admin?flash="+flash, http.StatusSeeOther)
}

func handleAdminCreateUser(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
		return
	}

	username := r.FormValue("username")
	password := r.FormValue("password")
	if len(username) < 2 || len(password) < cfg.Auth.MinPassword {
		http.Redirect(w, r, "/admin?flash=Username+min+2+chars,+password+min+8", http.StatusSeeOther)
		return
	}

	user, err := auth.Register(username, password)
	if err != nil {
		http.Redirect(w, r, "/admin?flash=Failed:+"+err.Error(), http.StatusSeeOther)
		return
	}

	if r.FormValue("is_active") == "on" {
		auth.SetUserActive(user.ID, true)
	}
	if r.FormValue("is_admin") == "on" {
		auth.SetUserAdmin(user.ID, true)
	}

	http.Redirect(w, r, "/admin?flash=User+"+username+"+created", http.StatusSeeOther)
}

func handleAdminActivate(w http.ResponseWriter, r *http.Request) {
	adminUserAction(w, r, func(id int64) error {
		return auth.SetUserActive(id, true)
	}, "User+activated")
}

func handleAdminDeactivate(w http.ResponseWriter, r *http.Request) {
	adminUserAction(w, r, func(id int64) error {
		return auth.SetUserActive(id, false)
	}, "User+deactivated")
}

func handleAdminMakeAdmin(w http.ResponseWriter, r *http.Request) {
	adminUserAction(w, r, func(id int64) error {
		return auth.SetUserAdmin(id, true)
	}, "Admin+granted")
}

func handleAdminRevokeAdmin(w http.ResponseWriter, r *http.Request) {
	adminUserAction(w, r, func(id int64) error {
		return auth.SetUserAdmin(id, false)
	}, "Admin+revoked")
}

func handleAdminDeleteUser(w http.ResponseWriter, r *http.Request) {
	adminUserAction(w, r, func(id int64) error {
		return auth.DeleteUser(id)
	}, "User+deleted")
}

func handleAdminResetPassword(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
		return
	}

	userID, err := strconv.ParseInt(r.FormValue("user_id"), 10, 64)
	if err != nil {
		http.Error(w, "invalid user_id", http.StatusBadRequest)
		return
	}

	newPassword := r.FormValue("new_password")
	if len(newPassword) < 8 {
		http.Redirect(w, r, "/admin?flash=Password+must+be+at+least+8+characters", http.StatusSeeOther)
		return
	}

	if err := auth.SetUserPassword(userID, newPassword); err != nil {
		logger.Error("admin: failed to reset password for user %d: %v", userID, err)
		http.Redirect(w, r, "/admin?flash=Failed+to+reset+password", http.StatusSeeOther)
		return
	}

	http.Redirect(w, r, "/admin?flash=Password+reset+successfully", http.StatusSeeOther)
}

// --- Channel member management ---

func handleChannelMembers(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodGet {
		http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
		return
	}

	chID, err := strconv.ParseInt(r.URL.Query().Get("id"), 10, 64)
	if err != nil {
		http.Error(w, "invalid id", http.StatusBadRequest)
		return
	}

	user := userFromContext(r)
	if !channel.CanManage(chID, user.ID, user.IsAdmin) {
		http.Error(w, "forbidden", http.StatusForbidden)
		return
	}

	members, err := channel.GetMembersWithNames(chID)
	if err != nil {
		http.Error(w, "internal server error", http.StatusInternalServerError)
		return
	}

	ch, err := channel.GetByID(chID)
	if err != nil {
		http.Error(w, "channel not found", http.StatusNotFound)
		return
	}

	w.Header().Set("Content-Type", "application/json")
	json.NewEncoder(w).Encode(map[string]any{
		"channel_id": ch.ID,
		"name":       ch.Name,
		"members":    members,
		"created_by": ch.CreatedBy,
	})
}

func handleChannelMemberAdd(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
		return
	}

	chID, err := strconv.ParseInt(r.FormValue("channel_id"), 10, 64)
	if err != nil {
		http.Error(w, "invalid channel_id", http.StatusBadRequest)
		return
	}

	user := userFromContext(r)
	if !channel.CanManage(chID, user.ID, user.IsAdmin) {
		http.Error(w, "forbidden", http.StatusForbidden)
		return
	}

	username := r.FormValue("username")
	target, err := auth.GetUserByUsername(username)
	if err != nil {
		http.Error(w, "user not found", http.StatusNotFound)
		return
	}

	if err := channel.AddMember(chID, target.ID); err != nil {
		http.Error(w, "failed to add member", http.StatusInternalServerError)
		return
	}

	w.Header().Set("Content-Type", "application/json")
	json.NewEncoder(w).Encode(map[string]any{"ok": true})
}

func handleChannelMemberRemove(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
		return
	}

	chID, err := strconv.ParseInt(r.FormValue("channel_id"), 10, 64)
	if err != nil {
		http.Error(w, "invalid channel_id", http.StatusBadRequest)
		return
	}

	user := userFromContext(r)
	if !channel.CanManage(chID, user.ID, user.IsAdmin) {
		http.Error(w, "forbidden", http.StatusForbidden)
		return
	}

	memberID, err := strconv.ParseInt(r.FormValue("user_id"), 10, 64)
	if err != nil {
		http.Error(w, "invalid user_id", http.StatusBadRequest)
		return
	}

	if err := channel.RemoveMember(chID, memberID); err != nil {
		http.Error(w, "failed to remove member", http.StatusInternalServerError)
		return
	}

	w.Header().Set("Content-Type", "application/json")
	json.NewEncoder(w).Encode(map[string]any{"ok": true})
}

// --- API ---

func handleChangePassword(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
		return
	}

	user := userFromContext(r)
	oldPw := r.FormValue("old_password")
	newPw := r.FormValue("new_password")

	if err := auth.CheckPassword(user.ID, oldPw); err != nil {
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusBadRequest)
		json.NewEncoder(w).Encode(map[string]string{"error": "Current password is incorrect"})
		return
	}

	if len(newPw) < cfg.Auth.MinPassword {
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusBadRequest)
		json.NewEncoder(w).Encode(map[string]string{"error": fmt.Sprintf("Password must be at least %d characters", cfg.Auth.MinPassword)})
		return
	}

	if err := auth.SetUserPassword(user.ID, newPw); err != nil {
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusInternalServerError)
		json.NewEncoder(w).Encode(map[string]string{"error": "Failed to change password"})
		return
	}

	w.Header().Set("Content-Type", "application/json")
	json.NewEncoder(w).Encode(map[string]string{"ok": "true"})
}

func handleAPIUsers(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodGet {
		http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
		return
	}
	users, err := auth.ListUsers()
	if err != nil {
		http.Error(w, "internal server error", http.StatusInternalServerError)
		return
	}
	type userInfo struct {
		ID       int64  `json:"id"`
		Username string `json:"username"`
	}
	var result []userInfo
	for _, u := range users {
		if u.IsActive {
			result = append(result, userInfo{ID: u.ID, Username: u.Username})
		}
	}
	w.Header().Set("Content-Type", "application/json")
	json.NewEncoder(w).Encode(result)
}

// --- Invite links ---

func handleChannelInvite(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
		return
	}

	chID, err := strconv.ParseInt(r.FormValue("channel_id"), 10, 64)
	if err != nil {
		http.Error(w, "invalid channel_id", http.StatusBadRequest)
		return
	}

	user := userFromContext(r)
	if !channel.CanManage(chID, user.ID, user.IsAdmin) {
		http.Error(w, "forbidden", http.StatusForbidden)
		return
	}

	token, err := channel.CreateInvite(chID, user.ID)
	if err != nil {
		logger.Error("failed to create invite: %v", err)
		http.Error(w, "internal server error", http.StatusInternalServerError)
		return
	}

	w.Header().Set("Content-Type", "application/json")
	json.NewEncoder(w).Encode(map[string]any{
		"token": token,
		"url":   fmt.Sprintf("/invite/%s", token),
	})
}
func handleInviteAccept(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodGet {
		http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
		return
	}

	token := strings.TrimPrefix(r.URL.Path, "/invite/")
	if token == "" {
		http.Error(w, "invalid invite", http.StatusBadRequest)
		return
	}

	user := auth.UserFromRequest(r)
	if user == nil {
		http.Redirect(w, r, "/login?next="+r.URL.Path, http.StatusSeeOther)
		return
	}

	chID, err := channel.AcceptInvite(token, user.ID)
	if err != nil {
		http.Error(w, "Invalid or expired invite link", http.StatusBadRequest)
		return
	}

	ch, err := channel.GetByID(chID)
	if err != nil {
		http.Redirect(w, r, "/", http.StatusSeeOther)
		return
	}

	http.Redirect(w, r, "/channels/"+ch.Name, http.StatusSeeOther)
}

// --- Guest access ---

func handleGuest(w http.ResponseWriter, r *http.Request) {
	token := strings.TrimPrefix(r.URL.Path, "/guest/")
	if token == "" || token == "leave" {
		// Guest leave — clear cookie and redirect
		http.SetCookie(w, &http.Cookie{Name: "guest_session", Value: "", Path: "/", MaxAge: -1})
		http.Redirect(w, r, "/login", http.StatusSeeOther)
		return
	}

	chID, inviteExpiresAt, err := channel.ValidateGuestInvite(token)
	if err != nil {
		http.Error(w, "Invalid or expired guest link", http.StatusBadRequest)
		return
	}
	ch, err := channel.GetByID(chID)
	if err != nil {
		http.Error(w, "Channel not found", http.StatusNotFound)
		return
	}

	if r.Method == http.MethodGet {
		csrfToken := setCSRFCookie(w, r)
		templates["guest.html"].ExecuteTemplate(w, "layout.html", map[string]any{
			"Token":       token,
			"ChannelName": ch.Name,
			"ChannelID":   ch.ID,
			"CSRFToken":   csrfToken,
		})
		return
	}

	if r.Method != http.MethodPost {
		http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
		return
	}

	// CSRF check
	cookieToken, err2 := r.Cookie("csrf_token")
	formToken := r.FormValue("csrf_token")
	if err2 != nil || cookieToken.Value == "" || cookieToken.Value != formToken {
		http.Error(w, "invalid CSRF token", http.StatusForbidden)
		return
	}

	guestName := strings.TrimSpace(r.FormValue("guest_name"))
	if guestName == "" || len(guestName) > 30 {
		csrfToken := setCSRFCookie(w, r)
		templates["guest.html"].ExecuteTemplate(w, "layout.html", map[string]any{
			"Token":       token,
			"ChannelName": ch.Name,
			"ChannelID":   ch.ID,
			"CSRFToken":   csrfToken,
			"Error":       "Name must be 1-30 characters",
		})
		return
	}

	// Create guest session with the same expiry as the invite.
	sessionToken, err := channel.CreateGuestSession(guestName, chID, token, inviteExpiresAt)
	if err != nil {
		http.Error(w, "Failed to create guest session", http.StatusInternalServerError)
		return
	}

	secure := cfg.Auth.CookieSecure
	maxAge := int(inviteExpiresAt - time.Now().Unix())
	if maxAge < 1 {
		maxAge = 1
	}
	http.SetCookie(w, &http.Cookie{
		Name:     "guest_session",
		Value:    sessionToken,
		Path:     "/",
		HttpOnly: true,
		Secure:   secure,
		SameSite: http.SameSiteLaxMode,
		MaxAge:   maxAge,
	})

	http.Redirect(w, r, "/guest-app?t="+sessionToken, http.StatusSeeOther)
}

func handleGuestApp(w http.ResponseWriter, r *http.Request) {
	// Try cookie first, then query param (mobile fallback)
	guestToken := ""
	if cookie, err := r.Cookie("guest_session"); err == nil && cookie.Value != "" {
		guestToken = cookie.Value
	}
	if guestToken == "" {
		guestToken = r.URL.Query().Get("t")
	}
	if guestToken == "" {
		http.Redirect(w, r, "/login", http.StatusSeeOther)
		return
	}

	gs, err := channel.ValidateGuestSession(guestToken)
	if err != nil {
		http.SetCookie(w, &http.Cookie{Name: "guest_session", Value: "", Path: "/", MaxAge: -1})
		http.Redirect(w, r, "/login", http.StatusSeeOther)
		return
	}

	ch, err := channel.GetByID(gs.ChannelID)
	if err != nil {
		http.Error(w, "Channel not found", http.StatusNotFound)
		return
	}

	// ICE servers
	iceServers := buildICEServers()

	templates["guest-app.html"].ExecuteTemplate(w, "layout.html", map[string]any{
		"GuestName":   gs.GuestName,
		"GuestToken":  guestToken,
		"ChannelID":   ch.ID,
		"ChannelName": ch.Name,
		"ICEServers":  iceServers,
		"CacheBust":   cacheBust,
	})
}

func handleCreateGuestInvite(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
		return
	}

	chID, err := strconv.ParseInt(r.FormValue("channel_id"), 10, 64)
	if err != nil {
		http.Error(w, "invalid channel_id", http.StatusBadRequest)
		return
	}

	hours, _ := strconv.Atoi(r.FormValue("hours"))
	if hours <= 0 || hours > 168 { // max 7 days
		hours = 24
	}

	user := userFromContext(r)

	// Verify user has access to this channel
	if !channel.CanJoin(chID, user.ID, user.IsAdmin) {
		http.Error(w, "forbidden", http.StatusForbidden)
		return
	}

	token, err := channel.CreateGuestInvite(chID, user.ID, hours)
	if err != nil {
		http.Error(w, "Failed to create guest invite", http.StatusInternalServerError)
		return
	}

	w.Header().Set("Content-Type", "application/json")
	json.NewEncoder(w).Encode(map[string]any{
		"token": token,
		"url":   fmt.Sprintf("/guest/%s", token),
		"hours": hours,
	})
}

// --- OAuth2 ---

func getOAuthProvider(name string) *config.OAuthProvider {
	for i := range cfg.OAuth.Providers {
		if cfg.OAuth.Providers[i].Name == name {
			return &cfg.OAuth.Providers[i]
		}
	}
	return nil
}

func oauthConfig(provider *config.OAuthProvider, r *http.Request) *oauth2.Config {
	scheme := "https"
	if r.TLS == nil && r.Header.Get("X-Forwarded-Proto") != "https" {
		scheme = "http"
	}
	redirectURL := fmt.Sprintf("%s://%s/auth/oauth/%s/callback", scheme, r.Host, provider.Name)
	return &oauth2.Config{
		ClientID:     provider.ClientID,
		ClientSecret: provider.ClientSecret,
		Endpoint: oauth2.Endpoint{
			AuthURL:  provider.AuthURL,
			TokenURL: provider.TokenURL,
		},
		RedirectURL: redirectURL,
		Scopes:      provider.Scopes,
	}
}

func handleOAuth(w http.ResponseWriter, r *http.Request) {
	if !cfg.OAuth.Enabled {
		http.Error(w, "OAuth not configured", http.StatusNotFound)
		return
	}

	// Parse path: /auth/oauth/{provider} or /auth/oauth/{provider}/callback
	path := strings.TrimPrefix(r.URL.Path, "/auth/oauth/")
	parts := strings.SplitN(path, "/", 2)
	providerName := parts[0]
	isCallback := len(parts) > 1 && parts[1] == "callback"

	provider := getOAuthProvider(providerName)
	if provider == nil {
		http.Error(w, "unknown OAuth provider", http.StatusNotFound)
		return
	}

	oc := oauthConfig(provider, r)

	if !isCallback {
		// Generate state and redirect to OAuth provider
		state := auth.GenerateToken()
		http.SetCookie(w, &http.Cookie{
			Name:     "oauth_state",
			Value:    state,
			Path:     "/",
			HttpOnly: true,
			Secure:   cfg.Auth.CookieSecure,
			SameSite: http.SameSiteLaxMode,
			MaxAge:   600,
		})
		http.Redirect(w, r, oc.AuthCodeURL(state), http.StatusTemporaryRedirect)
		return
	}

	// Callback — verify state
	stateCookie, err := r.Cookie("oauth_state")
	if err != nil || stateCookie.Value == "" || stateCookie.Value != r.URL.Query().Get("state") {
		http.Error(w, "invalid OAuth state", http.StatusBadRequest)
		return
	}
	// Clear state cookie
	http.SetCookie(w, &http.Cookie{Name: "oauth_state", Value: "", Path: "/", MaxAge: -1})

	// Exchange code for token
	code := r.URL.Query().Get("code")
	if code == "" {
		http.Error(w, "missing OAuth code", http.StatusBadRequest)
		return
	}
	token, err := oc.Exchange(r.Context(), code)
	if err != nil {
		logger.Error("OAuth token exchange failed: %v", err)
		http.Error(w, "OAuth authentication failed", http.StatusInternalServerError)
		return
	}

	// Fetch user info
	client := oc.Client(r.Context(), token)
	resp, err := client.Get(provider.UserInfoURL)
	if err != nil {
		logger.Error("OAuth userinfo fetch failed: %v", err)
		http.Error(w, "Failed to get user info", http.StatusInternalServerError)
		return
	}
	defer resp.Body.Close()

	var userInfo map[string]any
	if err := json.NewDecoder(resp.Body).Decode(&userInfo); err != nil {
		logger.Error("OAuth userinfo decode failed: %v", err)
		http.Error(w, "Failed to parse user info", http.StatusInternalServerError)
		return
	}

	// Extract user data (compatible with Google, GitHub, most OIDC providers)
	oauthID := ""
	email := ""
	displayName := ""

	if v, ok := userInfo["sub"].(string); ok {
		oauthID = v // OIDC standard
	} else if v, ok := userInfo["id"].(float64); ok {
		oauthID = fmt.Sprintf("%.0f", v) // GitHub uses numeric ID
	} else if v, ok := userInfo["id"].(string); ok {
		oauthID = v
	}
	if v, ok := userInfo["email"].(string); ok {
		email = v
	}
	if v, ok := userInfo["name"].(string); ok {
		displayName = v
	} else if v, ok := userInfo["login"].(string); ok {
		displayName = v // GitHub
	}

	if oauthID == "" {
		http.Error(w, "OAuth provider did not return user ID", http.StatusBadRequest)
		return
	}

	autoActivate := provider.AutoActivate
	user, err := auth.FindOrCreateOAuthUser(providerName, oauthID, email, displayName, autoActivate)
	if err != nil {
		logger.Error("OAuth user creation failed: %v", err)
		http.Error(w, "Failed to create user", http.StatusInternalServerError)
		return
	}

	if !user.IsActive {
		csrfToken := setCSRFCookie(w, r)
		templates["login.html"].ExecuteTemplate(w, "layout.html", map[string]any{
			"Info":                "Account pending activation. Contact an administrator.",
			"CSRFToken":           csrfToken,
			"RegistrationEnabled": cfg.Auth.RegistrationEnabled,
			"OAuthProviders":      cfg.OAuth.Providers,
		})
		return
	}

	// Create session
	sessionToken, err := auth.CreateSession(user.ID)
	if err != nil {
		http.Error(w, "Failed to create session", http.StatusInternalServerError)
		return
	}
	http.SetCookie(w, sessionCookie(sessionToken, 86400*cfg.Auth.SessionDays))
	http.Redirect(w, r, "/", http.StatusSeeOther)
}
