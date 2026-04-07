package auth

import (
	"crypto/rand"
	"encoding/hex"
	"errors"
	"net/http"
	"regexp"
	"time"

	"github.com/foxzi/vocala/internal/database"
	"golang.org/x/crypto/bcrypt"
)

var validUsername = regexp.MustCompile(`^[a-zA-Z0-9_.\- ]{2,30}$`)

type User struct {
	ID        int64
	Username  string
	IsAdmin   bool
	IsActive  bool
	CreatedAt time.Time
}

var (
	ErrUserExists  = errors.New("username already taken")
	ErrInvalidAuth = errors.New("invalid username or password")
	ErrNotActive   = errors.New("account is not activated")
)

func ValidateUsername(username string) error {
	if !validUsername.MatchString(username) {
		return errors.New("username must be 2-30 chars: letters, digits, _ . - or spaces")
	}
	return nil
}

func Register(username, password string) (*User, error) {
	if err := ValidateUsername(username); err != nil {
		return nil, err
	}
	hash, err := bcrypt.GenerateFromPassword([]byte(password), bcrypt.DefaultCost)
	if err != nil {
		return nil, err
	}

	// First user is auto-activated and made admin
	var count int
	database.DB.QueryRow("SELECT COUNT(*) FROM users").Scan(&count)

	isAdmin := 0
	isActive := 0
	if count == 0 {
		isAdmin = 1
		isActive = 1
	}

	res, err := database.DB.Exec(
		"INSERT INTO users (username, password_hash, is_admin, is_active) VALUES (?, ?, ?, ?)",
		username, string(hash), isAdmin, isActive,
	)
	if err != nil {
		return nil, ErrUserExists
	}

	id, _ := res.LastInsertId()
	return &User{
		ID: id, Username: username,
		IsAdmin: isAdmin == 1, IsActive: isActive == 1,
	}, nil
}

func CheckPassword(userID int64, password string) error {
	var hash string
	err := database.DB.QueryRow("SELECT password_hash FROM users WHERE id = ?", userID).Scan(&hash)
	if err != nil {
		return ErrInvalidAuth
	}
	return bcrypt.CompareHashAndPassword([]byte(hash), []byte(password))
}

func Login(username, password string) (*User, error) {
	var user User
	var hash string
	var isAdmin, isActive int
	err := database.DB.QueryRow(
		"SELECT id, username, password_hash, is_admin, is_active FROM users WHERE username = ?",
		username,
	).Scan(&user.ID, &user.Username, &hash, &isAdmin, &isActive)
	if err != nil {
		return nil, ErrInvalidAuth
	}

	if err := bcrypt.CompareHashAndPassword([]byte(hash), []byte(password)); err != nil {
		return nil, ErrInvalidAuth
	}

	user.IsAdmin = isAdmin == 1
	user.IsActive = isActive == 1

	if !user.IsActive {
		return &user, ErrNotActive
	}

	return &user, nil
}

func CreateSession(userID int64) (string, error) {
	b := make([]byte, 32)
	if _, err := rand.Read(b); err != nil {
		return "", err
	}
	token := hex.EncodeToString(b)

	_, err := database.DB.Exec(
		"INSERT INTO sessions (token, user_id, expires_at) VALUES (?, ?, datetime('now', '+30 days'))",
		token, userID,
	)
	if err != nil {
		return "", err
	}

	return token, nil
}

// GenerateToken creates a random hex token.
func GenerateToken() string {
	b := make([]byte, 32)
	rand.Read(b)
	return hex.EncodeToString(b)
}

// CleanExpiredSessions removes expired sessions from the database.
func CleanExpiredSessions() {
	database.DB.Exec("DELETE FROM sessions WHERE expires_at < datetime('now')")
}

func DeleteSession(token string) {
	database.DB.Exec("DELETE FROM sessions WHERE token = ?", token)
}

func UserFromRequest(r *http.Request) *User {
	cookie, err := r.Cookie("session")
	if err != nil {
		return nil
	}
	return UserFromToken(cookie.Value)
}

func UserFromToken(token string) *User {
	var user User
	var isAdmin, isActive int
	err := database.DB.QueryRow(
		"SELECT u.id, u.username, u.is_admin, u.is_active FROM users u JOIN sessions s ON s.user_id = u.id WHERE s.token = ? AND s.expires_at > datetime('now')",
		token,
	).Scan(&user.ID, &user.Username, &isAdmin, &isActive)
	if err != nil {
		return nil
	}
	user.IsAdmin = isAdmin == 1
	user.IsActive = isActive == 1
	return &user
}

// --- Admin functions ---

// ListUsers returns all users ordered by creation date.
func GetUserByUsername(username string) (*User, error) {
	var u User
	var isAdmin, isActive int
	err := database.DB.QueryRow(
		"SELECT id, username, is_admin, is_active, created_at FROM users WHERE username = ?",
		username,
	).Scan(&u.ID, &u.Username, &isAdmin, &isActive, &u.CreatedAt)
	if err != nil {
		return nil, err
	}
	u.IsAdmin = isAdmin == 1
	u.IsActive = isActive == 1
	return &u, nil
}

func ListUsers() ([]User, error) {
	rows, err := database.DB.Query(
		"SELECT id, username, is_admin, is_active, created_at FROM users ORDER BY created_at DESC",
	)
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	var users []User
	for rows.Next() {
		var u User
		var isAdmin, isActive int
		if err := rows.Scan(&u.ID, &u.Username, &isAdmin, &isActive, &u.CreatedAt); err != nil {
			return nil, err
		}
		u.IsAdmin = isAdmin == 1
		u.IsActive = isActive == 1
		users = append(users, u)
	}
	return users, rows.Err()
}

// SetUserActive activates or deactivates a user.
func SetUserActive(userID int64, active bool) error {
	val := 0
	if active {
		val = 1
	}
	_, err := database.DB.Exec("UPDATE users SET is_active = ? WHERE id = ?", val, userID)
	if err != nil {
		return err
	}
	// If deactivating, remove all their sessions
	if !active {
		database.DB.Exec("DELETE FROM sessions WHERE user_id = ?", userID)
	}
	return nil
}

// SetUserAdmin grants or revokes admin privileges.
func SetUserAdmin(userID int64, admin bool) error {
	val := 0
	if admin {
		val = 1
	}
	_, err := database.DB.Exec("UPDATE users SET is_admin = ? WHERE id = ?", val, userID)
	return err
}

// SetUserPassword changes a user's password and invalidates all sessions.
func SetUserPassword(userID int64, newPassword string) error {
	hash, err := bcrypt.GenerateFromPassword([]byte(newPassword), bcrypt.DefaultCost)
	if err != nil {
		return err
	}
	if _, err := database.DB.Exec("UPDATE users SET password_hash = ? WHERE id = ?", string(hash), userID); err != nil {
		return err
	}
	// Invalidate all sessions so user must re-login
	database.DB.Exec("DELETE FROM sessions WHERE user_id = ?", userID)
	return nil
}

// DeleteUser removes a user and all their sessions.
func DeleteUser(userID int64) error {
	_, err := database.DB.Exec("DELETE FROM users WHERE id = ?", userID)
	return err
}

// FindOrCreateOAuthUser finds user by OAuth provider+id, or creates a new one.
func FindOrCreateOAuthUser(provider, oauthID, email, displayName string, autoActivate bool) (*User, error) {
	// Try to find existing OAuth user
	var u User
	var isAdmin, isActive int
	err := database.DB.QueryRow(
		"SELECT id, username, is_admin, is_active, created_at FROM users WHERE oauth_provider = ? AND oauth_id = ?",
		provider, oauthID,
	).Scan(&u.ID, &u.Username, &isAdmin, &isActive, &u.CreatedAt)
	if err == nil {
		u.IsAdmin = isAdmin == 1
		u.IsActive = isActive == 1
		return &u, nil
	}

	// Try to find by email and link
	if email != "" {
		err = database.DB.QueryRow(
			"SELECT id, username, is_admin, is_active, created_at FROM users WHERE email = ? AND email != ''",
			email,
		).Scan(&u.ID, &u.Username, &isAdmin, &isActive, &u.CreatedAt)
		if err == nil {
			// Link existing account to OAuth
			database.DB.Exec("UPDATE users SET oauth_provider = ?, oauth_id = ? WHERE id = ?", provider, oauthID, u.ID)
			u.IsAdmin = isAdmin == 1
			u.IsActive = isActive == 1
			return &u, nil
		}
	}

	// Create new user
	username := displayName
	if username == "" {
		username = email
	}
	if username == "" {
		username = provider + "-" + oauthID[:8]
	}
	// Ensure unique username
	for i := 0; i < 100; i++ {
		var count int
		database.DB.QueryRow("SELECT COUNT(*) FROM users WHERE username = ?", username).Scan(&count)
		if count == 0 {
			break
		}
		username = displayName + "-" + randomHex(3)
	}

	active := 0
	if autoActivate {
		active = 1
	}
	// Check if first user — auto-admin
	var totalUsers int
	database.DB.QueryRow("SELECT COUNT(*) FROM users").Scan(&totalUsers)
	admin := 0
	if totalUsers == 0 {
		admin = 1
		active = 1
	}

	res, err := database.DB.Exec(
		"INSERT INTO users (username, password_hash, is_admin, is_active, oauth_provider, oauth_id, email) VALUES (?, '', ?, ?, ?, ?, ?)",
		username, admin, active, provider, oauthID, email,
	)
	if err != nil {
		return nil, err
	}
	id, _ := res.LastInsertId()
	return &User{
		ID: id, Username: username,
		IsAdmin: admin == 1, IsActive: active == 1,
	}, nil
}

func randomHex(n int) string {
	b := make([]byte, n)
	rand.Read(b)
	return hex.EncodeToString(b)
}
