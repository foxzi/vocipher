package database

import (
	"database/sql"
	"log"

	_ "github.com/mattn/go-sqlite3"
)

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
	}
	for _, q := range migrations {
		DB.Exec(q) // ignore errors if columns already exist
	}
}
