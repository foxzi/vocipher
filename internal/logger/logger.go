package logger

import (
	"log"
	"os"
)

type Level int

const (
	LevelDebug Level = iota
	LevelInfo
	LevelWarn
	LevelError
)

var level = LevelInfo

func ParseLevel(s string) Level {
	switch s {
	case "debug":
		return LevelDebug
	case "info":
		return LevelInfo
	case "warn", "warning":
		return LevelWarn
	case "error":
		return LevelError
	default:
		return LevelInfo
	}
}

func SetLevel(l Level) {
	level = l
}

func init() {
	log.SetOutput(os.Stderr)
	log.SetFlags(log.LstdFlags)
}

func Debug(format string, args ...any) {
	if level <= LevelDebug {
		log.Printf("[DEBUG] "+format, args...)
	}
}

func Info(format string, args ...any) {
	if level <= LevelInfo {
		log.Printf("[INFO] "+format, args...)
	}
}

func Warn(format string, args ...any) {
	if level <= LevelWarn {
		log.Printf("[WARN] "+format, args...)
	}
}

func Error(format string, args ...any) {
	if level <= LevelError {
		log.Printf("[ERROR] "+format, args...)
	}
}

func Fatal(format string, args ...any) {
	log.Fatalf("[FATAL] "+format, args...)
}
