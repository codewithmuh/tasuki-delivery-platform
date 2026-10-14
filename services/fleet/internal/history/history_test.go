package history

import (
	"bufio"
	"compress/gzip"
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/codewithmuh/tasuki-delivery-platform/services/fleet/internal/events"
)

func point(receivedAt time.Time) events.CourierLocation {
	return events.CourierLocation{
		Schema: events.SchemaCourierLocation, EventID: events.NewID(events.PrefixEvent),
		CourierID: events.NewID(events.PrefixCourier), Lat: 35.66, Lng: 139.70,
		DeviceTime: receivedAt.Add(-time.Second), ReceivedAt: receivedAt, Status: events.CourierIdle,
	}
}

// readAll decodes every complete file under dir and returns the event IDs by partition.
func readAll(t *testing.T, dir string) map[string][]string {
	t.Helper()
	out := map[string][]string{}
	files, _ := filepath.Glob(filepath.Join(dir, "locations", "dt=*", "hour=*", "part-*.ndjson.gz"))
	for _, path := range files {
		f, err := os.Open(path)
		if err != nil {
			t.Fatal(err)
		}
		gz, err := gzip.NewReader(f)
		if err != nil {
			t.Fatalf("%s is not a complete gzip file: %v", path, err)
		}
		sc := bufio.NewScanner(gz)
		for sc.Scan() {
			loc, err := events.DecodeCourierLocation(sc.Bytes())
			if err != nil {
				t.Fatalf("%s: %v", path, err)
			}
			rel, _ := filepath.Rel(dir, filepath.Dir(path))
			out[rel] = append(out[rel], loc.EventID)
		}
		_ = f.Close()
	}
	return out
}

func TestLocalPartitionsByHourAndPublishesOnClose(t *testing.T) {
	dir := t.TempDir()
	sink, err := NewLocal(dir, time.Hour)
	if err != nil {
		t.Fatal(err)
	}
	t0 := time.Date(2026, 10, 14, 3, 59, 59, 0, time.UTC)
	jst := time.FixedZone("JST", 9*3600)
	batch := []events.CourierLocation{point(t0), point(t0.Add(2 * time.Second).In(jst)), point(t0)}
	if err := sink.Write(context.Background(), batch); err != nil {
		t.Fatal(err)
	}
	if got := readAll(t, dir); len(got) != 0 {
		t.Fatalf("files must not be visible before they are closed: %v", got)
	}
	if err := sink.Close(); err != nil {
		t.Fatal(err)
	}
	got := readAll(t, dir)
	h03 := filepath.Join("locations", "dt=2026-10-14", "hour=03")
	h04 := filepath.Join("locations", "dt=2026-10-14", "hour=04") // partitions use UTC, not the event's zone
	if len(got[h03]) != 2 || len(got[h04]) != 1 || got[h03][0] != batch[0].EventID {
		t.Fatalf("partitions = %v", got)
	}
	leftovers, _ := filepath.Glob(filepath.Join(dir, "locations", "*", "*", ".*.tmp"))
	if len(leftovers) != 0 {
		t.Fatalf("temporary files left behind: %v", leftovers)
	}
}

func TestLocalRollsFilesAfterRollEvery(t *testing.T) {
	dir := t.TempDir()
	sink, err := NewLocal(dir, time.Minute)
	if err != nil {
		t.Fatal(err)
	}
	clock := time.Date(2026, 10, 14, 3, 0, 0, 0, time.UTC)
	sink.now = func() time.Time { return clock }
	if err := sink.Write(context.Background(), []events.CourierLocation{point(clock)}); err != nil {
		t.Fatal(err)
	}
	clock = clock.Add(2 * time.Minute) // the open file is now older than rollEvery
	if err := sink.Write(context.Background(), []events.CourierLocation{point(clock)}); err != nil {
		t.Fatal(err)
	}
	files, _ := filepath.Glob(filepath.Join(dir, "locations", "*", "*", "part-*.ndjson.gz"))
	if len(files) != 1 {
		t.Fatalf("expected the first file to be rolled, found %v", files)
	}
	_ = sink.Close()
	files, _ = filepath.Glob(filepath.Join(dir, "locations", "*", "*", "part-*.ndjson.gz"))
	if len(files) != 2 {
		t.Fatalf("expected two files after Close, found %v", files)
	}
}

func TestLocalErrors(t *testing.T) {
	blocker := filepath.Join(t.TempDir(), "file")
	if err := os.WriteFile(blocker, nil, 0o600); err != nil {
		t.Fatal(err)
	}
	if _, err := NewLocal(filepath.Join(blocker, "sub"), time.Minute); err == nil {
		t.Fatal("expected an error when the directory cannot be created")
	}
	dir := t.TempDir()
	sink, _ := NewLocal(dir, time.Minute)
	// Make the partition path a file, so the partition directory cannot be created.
	if err := os.MkdirAll(filepath.Join(dir, "locations"), 0o750); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, "locations", "dt=2026-10-14"), nil, 0o600); err != nil {
		t.Fatal(err)
	}
	err := sink.Write(context.Background(), []events.CourierLocation{point(time.Date(2026, 10, 14, 1, 0, 0, 0, time.UTC))})
	if err == nil || !strings.Contains(err.Error(), "history partition") {
		t.Fatalf("got %v", err)
	}
}

func TestDiscard(t *testing.T) {
	var s Sink = Discard{}
	if s.Write(context.Background(), []events.CourierLocation{point(time.Now())}) != nil || s.Close() != nil {
		t.Fatal("Discard must never fail")
	}
}
