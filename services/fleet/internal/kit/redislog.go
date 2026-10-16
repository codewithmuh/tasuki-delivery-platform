package kit

import (
	"context"
	"fmt"
	"log/slog"
)

// RedisLogger routes the go-redis client's own messages (dial failures, pool errors) into the
// service's JSON log at debug level. Without it the library prints plain-text lines to stderr,
// several a second while Redis is down (game day GD-01 counted 289 in a 20-second outage), which
// breaks the one-JSON-line-per-event contract exactly when someone is reading the logs. The
// services already log the same failures at warn, with context ("not ready", "retrying").
//
//	redis.SetLogger(kit.RedisLogger{Logger: logger})
type RedisLogger struct {
	Logger *slog.Logger
}

// Printf implements go-redis's logging interface.
func (l RedisLogger) Printf(ctx context.Context, format string, v ...any) {
	l.Logger.DebugContext(ctx, "redis client", "detail", fmt.Sprintf(format, v...))
}
