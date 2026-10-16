package kit

import (
	"bytes"
	"context"
	"encoding/json"
	"log/slog"
	"strings"
	"testing"
)

func TestRedisLoggerWritesOneJSONLineAtDebug(t *testing.T) {
	var buf bytes.Buffer
	l := RedisLogger{Logger: slog.New(slog.NewJSONHandler(&buf, &slog.HandlerOptions{Level: slog.LevelDebug}))}
	l.Printf(context.Background(), "redis: connection pool: failed to dial after %d attempts: %s", 5, "no such host")

	lines := strings.Split(strings.TrimSpace(buf.String()), "\n")
	if len(lines) != 1 {
		t.Fatalf("got %d lines, want 1: %q", len(lines), buf.String())
	}
	var entry map[string]any
	if err := json.Unmarshal([]byte(lines[0]), &entry); err != nil {
		t.Fatalf("not JSON: %q (%v)", lines[0], err)
	}
	if entry["level"] != "DEBUG" || entry["msg"] != "redis client" ||
		entry["detail"] != "redis: connection pool: failed to dial after 5 attempts: no such host" {
		t.Errorf("entry = %v", entry)
	}
}

func TestRedisLoggerIsSilentAtInfo(t *testing.T) {
	var buf bytes.Buffer
	l := RedisLogger{Logger: slog.New(slog.NewJSONHandler(&buf, &slog.HandlerOptions{Level: slog.LevelInfo}))}
	l.Printf(context.Background(), "redis: dial failed")
	if buf.Len() != 0 {
		t.Errorf("logged at the default level: %q", buf.String())
	}
}
