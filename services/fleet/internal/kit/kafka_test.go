package kit

import (
	"context"
	"errors"
	"strconv"
	"sync"
	"testing"
	"time"

	"github.com/twmb/franz-go/pkg/kadm"
	"github.com/twmb/franz-go/pkg/kfake"
	"github.com/twmb/franz-go/pkg/kgo"
)

func fakeKafka(t *testing.T, topics ...string) KafkaConfig {
	t.Helper()
	c, err := kfake.NewCluster(kfake.NumBrokers(1), kfake.SeedTopics(3, topics...))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(c.Close)
	return KafkaConfig{Brokers: c.ListenAddrs(), ClientID: "test"}
}

func produce(t *testing.T, cfg KafkaConfig, topic string, n int) {
	t.Helper()
	cl, err := cfg.NewClient()
	if err != nil {
		t.Fatal(err)
	}
	defer cl.Close()
	for i := range n {
		r := &kgo.Record{Topic: topic, Key: []byte(strconv.Itoa(i % 5)), Value: []byte(strconv.Itoa(i))}
		if err := cl.ProduceSync(context.Background(), r).FirstErr(); err != nil {
			t.Fatal(err)
		}
	}
}

func TestLoadKafkaConfig(t *testing.T) {
	cfg := LoadKafkaConfig(envFrom(map[string]string{"KAFKA_BROKERS": "a:9092, b:9092"}), "svc")
	if len(cfg.Brokers) != 2 || cfg.Brokers[1] != "b:9092" || cfg.ClientID != "svc" {
		t.Fatalf("got %+v", cfg)
	}
	if d := LoadKafkaConfig(envFrom(nil), "svc"); d.Brokers[0] != "localhost:9092" {
		t.Fatalf("default brokers = %v", d.Brokers)
	}
}

func TestNewClientRejectsBadOptions(t *testing.T) {
	if _, err := (KafkaConfig{}).NewClient(); err == nil {
		t.Fatal("a client without seed brokers should be rejected")
	}
}

func TestConsumeDeliversBatchesAndStopsOnCancel(t *testing.T) {
	cfg := fakeKafka(t, "t")
	produce(t, cfg, "t", 20)
	cl, err := cfg.NewClient(kgo.ConsumeTopics("t"), kgo.ConsumeResetOffset(kgo.NewOffset().AtStart()))
	if err != nil {
		t.Fatal(err)
	}
	defer cl.Close()

	ctx, cancel := context.WithCancel(context.Background())
	var mu sync.Mutex
	got := 0
	done := make(chan error, 1)
	go func() {
		done <- Consume(ctx, cl, discard(), func(_ context.Context, recs []*kgo.Record) error {
			mu.Lock()
			got += len(recs)
			mu.Unlock()
			return nil
		})
	}()
	waitFor(t, "20 records", func() bool { mu.Lock(); defer mu.Unlock(); return got == 20 })
	cancel()
	if err := <-done; err != nil {
		t.Fatalf("Consume returned %v after cancel", err)
	}
}

func TestConsumeReturnsHandlerErrorsAndStopsOnClose(t *testing.T) {
	cfg := fakeKafka(t, "t")
	produce(t, cfg, "t", 3)
	cl, err := cfg.NewClient(kgo.ConsumeTopics("t"), kgo.ConsumeResetOffset(kgo.NewOffset().AtStart()))
	if err != nil {
		t.Fatal(err)
	}
	boom := errors.New("boom")
	if err := Consume(context.Background(), cl, discard(), func(context.Context, []*kgo.Record) error { return boom }); !errors.Is(err, boom) {
		t.Fatalf("got %v, want the handler error", err)
	}
	cl.Close()
	if err := Consume(context.Background(), cl, discard(), nil); err != nil {
		t.Fatalf("closed client: got %v, want nil", err)
	}
}

func TestRetry(t *testing.T) {
	calls := 0
	err := Retry(context.Background(), discard(), "flaky", func(context.Context) error {
		calls++
		if calls < 2 {
			return errors.New("not yet")
		}
		return nil
	})
	if err != nil || calls != 2 {
		t.Fatalf("err=%v calls=%d", err, calls)
	}

	ctx, cancel := context.WithTimeout(context.Background(), 100*time.Millisecond)
	defer cancel()
	if err := Retry(ctx, discard(), "never", func(context.Context) error { return errors.New("down") }); !errors.Is(err, context.DeadlineExceeded) {
		t.Fatalf("got %v, want deadline exceeded", err)
	}
	cancelled, cancelNow := context.WithCancel(context.Background())
	cancelNow()
	if err := Retry(cancelled, discard(), "cancelled", func(ctx context.Context) error { return ctx.Err() }); !errors.Is(err, context.Canceled) {
		t.Fatalf("got %v, want canceled", err)
	}
}

func listed(topic string, offsets map[int32]int64) kadm.ListedOffsets {
	out := kadm.ListedOffsets{topic: {}}
	for p, o := range offsets {
		out[topic][p] = kadm.ListedOffset{Topic: topic, Partition: p, Offset: o}
	}
	return out
}

func TestCatchUp(t *testing.T) {
	start := listed("t", map[int32]int64{0: 0, 1: 5, 2: 3})
	end := listed("t", map[int32]int64{0: 2, 1: 5, 2: 4}) // partition 1 has nothing to read
	end["missing"] = map[int32]kadm.ListedOffset{-1: {Topic: "missing", Partition: -1, Err: errors.New("unknown topic")}}
	c := NewCatchUp(start, end)
	if c.Caught() {
		t.Fatal("caught up before reading anything")
	}
	offs := c.Offsets()
	if len(offs["t"]) != 2 || offs["t"][2].EpochOffset().Offset != 3 {
		t.Fatalf("offsets to read = %v", offs)
	}
	c.Observe(&kgo.Record{Topic: "t", Partition: 0, Offset: 0})
	c.Observe(&kgo.Record{Topic: "t", Partition: 2, Offset: 3})
	c.Observe(&kgo.Record{Topic: "other", Partition: 0, Offset: 9})
	if c.Caught() {
		t.Fatal("partition 0 is not finished yet")
	}
	c.Observe(&kgo.Record{Topic: "t", Partition: 0, Offset: 1})
	select {
	case <-c.Done():
	default:
		t.Fatal("Done should be closed")
	}
	c.Observe(&kgo.Record{Topic: "t", Partition: 0, Offset: 1}) // after completion: no panic
}

func TestCatchUpWithNothingToReplay(t *testing.T) {
	if !NewCatchUp(kadm.ListedOffsets{}, kadm.ListedOffsets{}).Caught() {
		t.Fatal("an empty replay is caught up at once")
	}
	// A partition without a start offset is read from 0.
	c := NewCatchUp(kadm.ListedOffsets{}, listed("t", map[int32]int64{0: 1}))
	if c.Offsets()["t"][0].EpochOffset().Offset != 0 {
		t.Fatalf("offsets = %v", c.Offsets())
	}
}
