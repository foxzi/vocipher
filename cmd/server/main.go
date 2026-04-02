package main

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"fmt"
	"html/template"
	"log"
	"net/http"
	"os"
	"os/signal"
	"path/filepath"
	"strconv"
	"sync"
	"syscall"
	"time"

	"github.com/kidandcat/vocipher/internal/auth"
	"github.com/kidandcat/vocipher/internal/channel"
	"github.com/kidandcat/vocipher/internal/database"
	"github.com/kidandcat/vocipher/internal/signaling"
	"golang.org/x/time/rate"
)

var templates map[string]*template.Template
var cacheBust = fmt.Sprintf("%d", time.Now().Unix())

// Context key for authenticated user (#15 — avoid double DB query)
type ctxKey string

const userCtxKey ctxKey = "user"

func loadTemplates() map[string]*template.Template {
	layoutFile := filepath.Join("web", "templates", "layout.html")
	pages := []string{"login.html", "register.html", "app.html"}
	t := make(map[string]*template.Template, len(pages))
	for _, page := range pages {
		t[page] = template.Must(template.ParseFiles(layoutFile, filepath.Join("web", "templates", page)))
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
	// 10 requests per second, burst of 20
	lim := rate.NewLimiter(10, 20)
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
	return &http.Cookie{
		Name:     "session",
		Value:    token,
		Path:     "/",
		HttpOnly: true,
		SameSite: http.SameSiteLaxMode,
		Secure:   false, // Set to true when behind TLS
		MaxAge:   maxAge,
	}
}

// --- Main ---

func main() {
	dbPath := os.Getenv("VOCIPHER_DB_PATH")
	if dbPath == "" {
		dbPath = "vocipher.db"
	}
	database.Init(dbPath)

	// Periodic session cleanup (#5)
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

	// Auth routes (with method check #10)
	mux.HandleFunc("/login", handleLogin)
	mux.HandleFunc("/register", handleRegister)
	mux.HandleFunc("/logout", handleLogout)

	// App routes (auth required, CSRF on POST)
	mux.HandleFunc("/", requireAuth(handleApp))
	mux.HandleFunc("/channels", requireAuth(csrfProtect(handleChannels)))
	mux.HandleFunc("/channels/delete", requireAuth(csrfProtect(handleDeleteChannel)))

	// WebSocket
	mux.HandleFunc("/ws", signaling.HandleWebSocket)

	// Wrap with middleware: security headers -> rate limiting -> mux
	handler := securityHeaders(rateLimitMiddleware(mux))

	addr := os.Getenv("VOCIPHER_ADDR")
	if addr == "" {
		addr = ":8090"
	}
	server := &http.Server{
		Addr:         addr,
		Handler:      handler,
		ReadTimeout:  15 * time.Second,
		WriteTimeout: 30 * time.Second,
		IdleTimeout:  120 * time.Second,
	}

	// Graceful shutdown (#17)
	go func() {
		log.Printf("Vocipher server starting on http://localhost%s", addr)
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
		// Store user in context to avoid double DB query (#15)
		ctx := context.WithValue(r.Context(), userCtxKey, user)
		next(w, r.WithContext(ctx))
	}
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
		if auth.UserFromRequest(r) != nil {
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
		if auth.UserFromRequest(r) != nil {
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
	if r.URL.Path != "/" {
		http.NotFound(w, r)
		return
	}
	if r.Method != http.MethodGet {
		http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
		return
	}

	user := userFromContext(r)
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
		"CacheBust": cacheBust,
		"CSRFToken": csrfToken,
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

	// #3 — authorization: only the creator can delete
	user := userFromContext(r)
	ch, err := channel.GetByID(id)
	if err != nil {
		http.Error(w, "channel not found", http.StatusNotFound)
		return
	}
	if ch.CreatedBy != user.ID {
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
