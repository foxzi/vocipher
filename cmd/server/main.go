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
	"log"
	"net/http"
	"os"
	"os/signal"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"syscall"
	"time"

	"github.com/kidandcat/vocipher/internal/auth"
	"github.com/kidandcat/vocipher/internal/channel"
	"github.com/kidandcat/vocipher/internal/config"
	"github.com/kidandcat/vocipher/internal/database"
	"github.com/kidandcat/vocipher/internal/signaling"
	embeddedTurn "github.com/kidandcat/vocipher/internal/turn"
	rtc "github.com/kidandcat/vocipher/internal/webrtc"
	"golang.org/x/time/rate"
)

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
	pages := []string{"login.html", "register.html", "app.html", "admin.html"}
	t := make(map[string]*template.Template, len(pages))
	for _, page := range pages {
		t[page] = template.Must(
			template.New("").Funcs(funcMap).ParseFiles(layoutFile, filepath.Join("web", "templates", page)),
		)
	}
	return t
}

// --- Rate limiter (#6) ---

type ipLimiter struct {
	mu       sync.Mutex
	limiters map[string]*rate.Limiter
}

func newIPLimiter() *ipLimiter {
	return &ipLimiter{limiters: make(map[string]*rate.Limiter)}
}

func (l *ipLimiter) get(ip string) *rate.Limiter {
	l.mu.Lock()
	defer l.mu.Unlock()
	if lim, ok := l.limiters[ip]; ok {
		return lim
	}
	rps := 10
	burst := 20
	if cfg != nil {
		rps = cfg.Auth.RateLimitRPS
		burst = cfg.Auth.RateLimitBurst
	}
	lim := rate.NewLimiter(rate.Limit(rps), burst)
	l.limiters[ip] = lim
	return lim
}

var limiter = newIPLimiter()

func rateLimitMiddleware(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		ip := r.RemoteAddr
		if fwd := r.Header.Get("X-Forwarded-For"); fwd != "" {
			ip = fwd
		}
		if !limiter.get(ip).Allow() {
			http.Error(w, "too many requests", http.StatusTooManyRequests)
			return
		}
		next.ServeHTTP(w, r)
	})
}

// --- Security headers (#9) ---

func securityHeaders(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("X-Content-Type-Options", "nosniff")
		w.Header().Set("X-Frame-Options", "DENY")
		w.Header().Set("Referrer-Policy", "strict-origin-when-cross-origin")
		w.Header().Set("Permissions-Policy", "microphone=(self), camera=()")
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
	http.SetCookie(w, &http.Cookie{
		Name:     "csrf_token",
		Value:    token,
		Path:     "/",
		HttpOnly: false, // JS needs to read it for HTMX
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

func main() {
	configPath := flag.String("config", "config.yaml", "path to config file")
	flag.Parse()

	cfg = config.Load(*configPath)

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
			log.Fatal("failed to start TURN server:", err)
		}
		defer turnServer.Close()

		username, password, uris := turnServer.Credentials()
		rtc.SetTURNCredentials(uris, username, password)
		log.Printf("TURN credentials: uris=%v user=%s", uris, username)
	}

	// Periodic session cleanup
	go func() {
		for {
			auth.CleanExpiredSessions()
			time.Sleep(1 * time.Hour)
		}
	}()

	templates = loadTemplates()

	mux := http.NewServeMux()

	// Static files
	mux.Handle("/static/", http.StripPrefix("/static/", http.FileServer(http.Dir("web/static"))))

	// Auth routes
	mux.HandleFunc("/login", handleLogin)
	mux.HandleFunc("/register", handleRegister)
	mux.HandleFunc("/logout", handleLogout)

	// App routes
	mux.HandleFunc("/", requireAuth(handleApp))
	mux.HandleFunc("/channels", requireAuth(csrfProtect(handleChannels)))
	mux.HandleFunc("/channels/delete", requireAuth(csrfProtect(handleDeleteChannel)))

	// Admin routes
	mux.HandleFunc("/admin", requireAdmin(handleAdmin))
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
		log.Printf("Vocipher server starting on http://localhost%s", cfg.Server.Addr)
		if err := server.ListenAndServe(); err != http.ErrServerClosed {
			log.Fatal("server error:", err)
		}
	}()

	quit := make(chan os.Signal, 1)
	signal.Notify(quit, syscall.SIGINT, syscall.SIGTERM)
	<-quit

	log.Println("Shutting down server...")
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	if err := server.Shutdown(ctx); err != nil {
		log.Fatal("server shutdown error:", err)
	}
	log.Println("Server stopped")
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
		templates["login.html"].ExecuteTemplate(w, "layout.html", map[string]any{"CSRFToken": csrfToken})
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
			"Error":     "Invalid username or password",
			"CSRFToken": csrfToken,
		})
		return
	}

	token, err := auth.CreateSession(user.ID)
	if err != nil {
		log.Printf("failed to create session for user %d: %v", user.ID, err)
		csrfToken := setCSRFCookie(w, r)
		templates["login.html"].ExecuteTemplate(w, "layout.html", map[string]any{
			"Error":     "Something went wrong",
			"CSRFToken": csrfToken,
		})
		return
	}

	http.SetCookie(w, sessionCookie(token, 86400*30))
	http.Redirect(w, r, "/", http.StatusSeeOther)
}

func handleRegister(w http.ResponseWriter, r *http.Request) {
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
		log.Printf("failed to create session for user %d: %v", user.ID, err)
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
	channels, err := channel.List()
	if err != nil {
		log.Printf("failed to list channels: %v", err)
		http.Error(w, "internal server error", http.StatusInternalServerError)
		return
	}

	csrfToken := setCSRFCookie(w, r)

	// Build ICE servers config for client
	iceServers := []map[string]any{
		{"urls": "stun:stun.l.google.com:19302"},
		{"urls": "stun:stun1.l.google.com:19302"},
	}
	if creds := rtc.GetTURNCredentials(); creds != nil {
		iceServers = append(iceServers, map[string]any{
			"urls":       creds.URIs,
			"username":   creds.Username,
			"credential": creds.Password,
		})
	}

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
	if name != "" {
		if _, err := channel.Create(name, user.ID); err != nil {
			log.Printf("failed to create channel: %v", err)
		}
	}

	channels, err := channel.List()
	if err != nil {
		log.Printf("failed to list channels: %v", err)
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
		log.Printf("failed to delete channel %d: %v", id, err)
	}
	signaling.ClearChannelPreview(id)

	channels, err := channel.List()
	if err != nil {
		log.Printf("failed to list channels: %v", err)
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
		log.Printf("failed to list users: %v", err)
		http.Error(w, "internal server error", http.StatusInternalServerError)
		return
	}

	csrfToken := setCSRFCookie(w, r)
	data := map[string]any{
		"Users":     users,
		"CSRFToken": csrfToken,
		"Flash":     r.URL.Query().Get("flash"),
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
		log.Printf("admin action failed for user %d: %v", userID, err)
	}

	http.Redirect(w, r, "/admin?flash="+flash, http.StatusSeeOther)
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
		log.Printf("admin: failed to reset password for user %d: %v", userID, err)
		http.Redirect(w, r, "/admin?flash=Failed+to+reset+password", http.StatusSeeOther)
		return
	}

	http.Redirect(w, r, "/admin?flash=Password+reset+successfully", http.StatusSeeOther)
}
