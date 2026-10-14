package kit

import (
	"errors"
	"fmt"
	"strconv"
	"strings"
	"time"
)

// Env reads typed settings from environment variables. It remembers every bad value instead of
// stopping at the first one, so a misconfigured service reports all of its mistakes in one log line.
type Env struct {
	getenv func(string) string
	errs   []error
}

// NewEnv returns an Env that reads through getenv: os.Getenv in production, a map lookup in tests.
func NewEnv(getenv func(string) string) *Env {
	return &Env{getenv: getenv}
}

// String returns the value of key, or def when the variable is unset or empty.
func (e *Env) String(key, def string) string {
	if v := strings.TrimSpace(e.getenv(key)); v != "" {
		return v
	}
	return def
}

// Required returns the value of key and records an error when it is unset or empty.
func (e *Env) Required(key string) string {
	v := strings.TrimSpace(e.getenv(key))
	if v == "" {
		e.errs = append(e.errs, fmt.Errorf("%s is required", key))
	}
	return v
}

// Duration parses key with time.ParseDuration ("500ms", "5s", "2m"). Negative values are rejected.
func (e *Env) Duration(key string, def time.Duration) time.Duration {
	raw := e.String(key, "")
	if raw == "" {
		return def
	}
	d, err := time.ParseDuration(raw)
	if err != nil || d < 0 {
		e.errs = append(e.errs, fmt.Errorf("%s must be a non-negative duration such as 5s, got %q", key, raw))
		return def
	}
	return d
}

// Int parses key as a base-10 integer and records an error when it is below minimum.
func (e *Env) Int(key string, def, minimum int) int {
	raw := e.String(key, "")
	if raw == "" {
		return def
	}
	n, err := strconv.Atoi(raw)
	if err != nil || n < minimum {
		e.errs = append(e.errs, fmt.Errorf("%s must be an integer >= %d, got %q", key, minimum, raw))
		return def
	}
	return n
}

// Bool parses key with strconv.ParseBool (true, false, 1, 0, ...).
func (e *Env) Bool(key string, def bool) bool {
	raw := e.String(key, "")
	if raw == "" {
		return def
	}
	b, err := strconv.ParseBool(raw)
	if err != nil {
		e.errs = append(e.errs, fmt.Errorf("%s must be true or false, got %q", key, raw))
		return def
	}
	return b
}

// List splits key on commas and drops empty items: "a, b,,c" gives [a b c].
func (e *Env) List(key string, def []string) []string {
	raw := e.String(key, "")
	if raw == "" {
		return def
	}
	var out []string
	for item := range strings.SplitSeq(raw, ",") {
		if item = strings.TrimSpace(item); item != "" {
			out = append(out, item)
		}
	}
	return out
}

// Fail records a problem that the typed getters cannot see, such as a secret that is too short.
func (e *Env) Fail(err error) {
	e.errs = append(e.errs, err)
}

// Err returns every recorded problem joined into one error, or nil when the configuration is valid.
func (e *Env) Err() error {
	return errors.Join(e.errs...)
}
