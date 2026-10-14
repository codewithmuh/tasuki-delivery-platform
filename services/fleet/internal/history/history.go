// Package history stores every accepted GPS point for analytics and replays (design §7.2 step 3).
//
// Sink is the seam: P1 ships a local-filesystem implementation that writes gzipped NDJSON in the
// same partition layout the S3 sink will use (locations/dt=YYYY-MM-DD/hour=HH/), so swapping in
// S3 + Parquet later changes one constructor, not fleet-state.
package history

import (
	"compress/gzip"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"sync"
	"time"

	"github.com/oklog/ulid/v2"

	"github.com/codewithmuh/tasuki-delivery-platform/services/fleet/internal/events"
)

// Sink appends location points to long-term storage.
type Sink interface {
	// Write appends points. Implementations may buffer; Close makes buffered data durable.
	Write(ctx context.Context, points []events.CourierLocation) error
	// Close flushes and releases everything. Write must not be called afterwards.
	Close() error
}

// Discard is the Sink used when history is disabled (HISTORY_DIR unset) and during a Redis
// rebuild, whose points are already in history.
type Discard struct{}

// Write drops the points.
func (Discard) Write(context.Context, []events.CourierLocation) error { return nil }

// Close does nothing.
func (Discard) Close() error { return nil }

// Local writes gzipped NDJSON files under dir:
//
//	<dir>/locations/dt=2026-10-14/hour=03/part-<ULID>.ndjson.gz
//
// Partitions use received_at in UTC. Each file is written under a hidden ".tmp" name and renamed
// when it is closed, so a reader (or a future uploader) only ever sees complete gzip files.
// A file is closed when it has been open for rollEvery, so a crash loses at most that much.
type Local struct {
	dir       string
	rollEvery time.Duration
	now       func() time.Time

	mu    sync.Mutex
	parts map[string]*part // by partition directory
}

type part struct {
	tmp, final string
	file       *os.File
	gz         *gzip.Writer
	enc        *json.Encoder
	opened     time.Time
}

// NewLocal creates dir if needed and returns a Local sink.
func NewLocal(dir string, rollEvery time.Duration) (*Local, error) {
	if err := os.MkdirAll(dir, 0o750); err != nil {
		return nil, fmt.Errorf("history dir: %w", err)
	}
	return &Local{dir: dir, rollEvery: rollEvery, now: time.Now, parts: map[string]*part{}}, nil
}

// Write first closes files that have been open for rollEvery, then appends points to the file
// of their hour, opening new files as needed.
func (l *Local) Write(_ context.Context, points []events.CourierLocation) error {
	l.mu.Lock()
	defer l.mu.Unlock()
	if err := l.roll(false); err != nil {
		return err
	}
	for i := range points {
		p, err := l.partFor(points[i].ReceivedAt)
		if err != nil {
			return err
		}
		if err := p.enc.Encode(&points[i]); err != nil { // Encode adds the newline
			return fmt.Errorf("write %s: %w", p.tmp, err)
		}
	}
	return nil
}

// Close closes every open file.
func (l *Local) Close() error {
	l.mu.Lock()
	defer l.mu.Unlock()
	return l.roll(true)
}

func (l *Local) partFor(receivedAt time.Time) (*part, error) {
	t := receivedAt.UTC()
	dir := filepath.Join(l.dir, "locations", "dt="+t.Format("2006-01-02"), "hour="+t.Format("15"))
	if p, ok := l.parts[dir]; ok {
		return p, nil
	}
	if err := os.MkdirAll(dir, 0o750); err != nil {
		return nil, fmt.Errorf("history partition: %w", err)
	}
	name := "part-" + ulid.Make().String() + ".ndjson.gz" // unique across replicas and restarts, sorts by time
	p := &part{tmp: filepath.Join(dir, "."+name+".tmp"), final: filepath.Join(dir, name), opened: l.now()}
	f, err := os.OpenFile(p.tmp, os.O_CREATE|os.O_EXCL|os.O_WRONLY, 0o600)
	if err != nil {
		return nil, fmt.Errorf("history file: %w", err)
	}
	p.file, p.gz = f, gzip.NewWriter(f)
	p.enc = json.NewEncoder(p.gz)
	l.parts[dir] = p
	return p, nil
}

// roll closes files that have been open for rollEvery, or all of them when all is true.
func (l *Local) roll(all bool) error {
	var errs []error
	for dir, p := range l.parts {
		if !all && l.now().Sub(p.opened) < l.rollEvery {
			continue
		}
		delete(l.parts, dir)
		if err := p.close(); err != nil {
			errs = append(errs, err)
		}
	}
	return errors.Join(errs...)
}

func (p *part) close() error {
	if err := errors.Join(p.gz.Close(), p.file.Sync(), p.file.Close()); err != nil {
		return fmt.Errorf("close %s: %w", p.tmp, err)
	}
	if err := os.Rename(p.tmp, p.final); err != nil {
		return fmt.Errorf("publish %s: %w", p.final, err)
	}
	return nil
}
