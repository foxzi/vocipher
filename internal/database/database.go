package database

import (
	"database/sql"
	"log"
	"time"

	_ "github.com/mattn/go-sqlite3"
)

// ChatMessage represents a stored chat message.
type ChatMessage struct {
	ID        string `json:"id"`
	ChannelID int64  `json:"channel_id"`
	UserID    int64  `json:"user_id"`
	Username  string `json:"username"`
	Text      string `json:"text"`
	CreatedAt int64  `json:"timestamp"`
}

var DB *sql.DB

func Init(dbPath string) {
	var err error
	DB, err = sql.Open("sqlite3", dbPath+"?_journal_mode=WAL&_foreign_keys=on")
	if err != nil {
		log.Fatal("failed to open database:", err)
	}

	if err = DB.Ping(); err != nil {
		log.Fatal("failed to ping database:", err)
	}

	migrate()
}

func migrate() {
	queries := []string{
		`CREATE TABLE IF NOT EXISTS users (
			id INTEGER PRIMARY KEY AUTOINCREMENT,
			username TEXT UNIQUE NOT NULL,
			password_hash TEXT NOT NULL,
			is_admin INTEGER NOT NULL DEFAULT 0,
			is_active INTEGER NOT NULL DEFAULT 0,
			created_at DATETIME DEFAULT CURRENT_TIMESTAMP
		)`,
		`CREATE TABLE IF NOT EXISTS channels (
			id INTEGER PRIMARY KEY AUTOINCREMENT,
			name TEXT UNIQUE NOT NULL,
			created_by INTEGER REFERENCES users(id),
			created_at DATETIME DEFAULT CURRENT_TIMESTAMP
		)`,
		`CREATE TABLE IF NOT EXISTS sessions (
			token TEXT PRIMARY KEY,
			user_id INTEGER NOT NULL REFERENCES users(id) ON DELETE CASCADE,
			created_at DATETIME DEFAULT CURRENT_TIMESTAMP,
			expires_at DATETIME NOT NULL DEFAULT (datetime('now', '+30 days'))
		)`,
		`CREATE TABLE IF NOT EXISTS chat_messages (
			id TEXT PRIMARY KEY,
			channel_id INTEGER NOT NULL,
			user_id INTEGER NOT NULL,
			username TEXT NOT NULL,
			text TEXT NOT NULL,
			created_at INTEGER NOT NULL
		)`,
		`CREATE INDEX IF NOT EXISTS idx_chat_messages_channel ON chat_messages(channel_id, created_at)`,
		`CREATE TABLE IF NOT EXISTS channel_members (
			channel_id INTEGER NOT NULL REFERENCES channels(id) ON DELETE CASCADE,
			user_id INTEGER NOT NULL REFERENCES users(id) ON DELETE CASCADE,
			PRIMARY KEY (channel_id, user_id)
		)`,
		`CREATE TABLE IF NOT EXISTS channel_invites (
			token TEXT PRIMARY KEY,
			channel_id INTEGER NOT NULL REFERENCES channels(id) ON DELETE CASCADE,
			created_by INTEGER NOT NULL,
			created_at INTEGER NOT NULL,
			expires_at INTEGER NOT NULL,
			max_uses INTEGER NOT NULL DEFAULT 0,
			uses INTEGER NOT NULL DEFAULT 0
		)`,
		`CREATE TABLE IF NOT EXISTS guest_invites (
			token TEXT PRIMARY KEY,
			channel_id INTEGER NOT NULL REFERENCES channels(id) ON DELETE CASCADE,
			created_by INTEGER NOT NULL,
			created_at INTEGER NOT NULL,
			expires_at INTEGER NOT NULL
		)`,
		`CREATE TABLE IF NOT EXISTS guest_sessions (
			token TEXT PRIMARY KEY,
			guest_name TEXT NOT NULL,
			channel_id INTEGER NOT NULL,
			invite_token TEXT NOT NULL,
			created_at INTEGER NOT NULL,
			expires_at INTEGER NOT NULL
		)`,
	}

	for _, q := range queries {
		if _, err := DB.Exec(q); err != nil {
			log.Fatal("migration failed:", err)
		}
	}

	// Migrations for existing databases
	migrations := []string{
		`ALTER TABLE users ADD COLUMN is_admin INTEGER NOT NULL DEFAULT 0`,
		`ALTER TABLE users ADD COLUMN is_active INTEGER NOT NULL DEFAULT 0`,
		`ALTER TABLE channels ADD COLUMN is_private INTEGER NOT NULL DEFAULT 0`,
	}
	for _, q := range migrations {
		DB.Exec(q) // ignore errors if columns already exist
	}
}

// SaveChatMessage stores a chat message in the database.
func SaveChatMessage(msg ChatMessage) error {
	_, err := DB.Exec(
		`INSERT INTO chat_messages (id, channel_id, user_id, username, text, created_at) VALUES (?, ?, ?, ?, ?, ?)`,
		msg.ID, msg.ChannelID, msg.UserID, msg.Username, msg.Text, msg.CreatedAt,
	)
	return err
}

// GetChatHistory returns the last N messages for a channel, oldest first.
func GetChatHistory(channelID int64, limit int) ([]ChatMessage, error) {
	rows, err := DB.Query(
		`SELECT id, channel_id, user_id, username, text, created_at FROM chat_messages
		 WHERE channel_id = ? ORDER BY created_at DESC LIMIT ?`,
		channelID, limit,
	)
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	var msgs []ChatMessage
	for rows.Next() {
		var m ChatMessage
		if err := rows.Scan(&m.ID, &m.ChannelID, &m.UserID, &m.Username, &m.Text, &m.CreatedAt); err != nil {
			continue
		}
		msgs = append(msgs, m)
	}
	// Reverse to get oldest first
	for i, j := 0, len(msgs)-1; i < j; i, j = i+1, j-1 {
		msgs[i], msgs[j] = msgs[j], msgs[i]
	}
	return msgs, nil
}

// ClearChannelMessages deletes all messages in a channel.
func ClearChannelMessages(channelID int64) {
	DB.Exec(`DELETE FROM chat_messages WHERE channel_id = ?`, channelID)
}

// CleanupOldMessages removes messages older than the given retention period.
func CleanupOldMessages(retentionDays int) (int64, error) {
	cutoff := time.Now().AddDate(0, 0, -retentionDays).Unix()
	result, err := DB.Exec(`DELETE FROM chat_messages WHERE created_at < ?`, cutoff)
	if err != nil {
		return 0, err
	}
	return result.RowsAffected()
}
