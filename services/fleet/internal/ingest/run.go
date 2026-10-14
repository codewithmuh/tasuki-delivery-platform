package ingest

import (
	"context"
	"fmt"
	"log/slog"
	"time"

	"github.com/twmb/franz-go/pkg/kgo"

	"github.com/codewithmuh/tasuki-delivery-platform/services/fleet/internal/auth"
	"github.com/codewithmuh/tasuki-delivery-platform/services/fleet/internal/events"
	"github.com/codewithmuh/tasuki-delivery-platform/services/fleet/internal/kit"
)

// Service is the binary name, used in logs and as the Kafka client ID.
const Service = "fleet-ingest"

// Run wires and runs fleet-ingest until ctx is cancelled.
//
// Readiness does not depend on Kafka on purpose: if every replica turned unready while Kafka is
// down, the load balancer would refuse connections, and courier apps would get connection
// errors instead of a 503 that tells them when to come back.
func Run(ctx context.Context, getenv func(string) string, logger *slog.Logger, version string) error {
	env := kit.NewEnv(getenv)
	base := kit.LoadConfig(env, Service, version)
	kafka := kit.LoadKafkaConfig(env, Service)
	cfg := LoadConfig(env)
	var verifier *auth.Verifier
	if secret := env.Required("COURIER_TOKEN_SECRET"); secret != "" {
		var err error
		if verifier, err = auth.NewVerifier(secret, auth.RoleCourier, events.PrefixCourier); err != nil {
			env.Fail(err)
		}
	}
	if err := env.Err(); err != nil {
		return fmt.Errorf("configuration: %w", err)
	}

	client, err := kafka.NewClient(
		kgo.DefaultProduceTopic(cfg.Topic),
		kgo.MaxBufferedRecords(cfg.MaxBufferedRecords),
		// Backstop for records that are buffered but never acknowledged: fail them instead of
		// holding memory forever while Kafka is away.
		kgo.RecordDeliveryTimeout(cfg.PublishTimeout+10*time.Second),
	)
	if err != nil {
		return err
	}

	srv := kit.New(base, logger)
	srv.Handle("POST /v1/locations", NewHandler(cfg, verifier, NewKafkaPublisher(client, cfg.Topic), logger, srv.Registry()))
	srv.OnStop("kafka producer", func(ctx context.Context) error {
		defer client.Close()
		return client.Flush(ctx) // deliver what is still buffered before exiting
	})
	logger.Info("starting", "brokers", kafka.Brokers, "topic", cfg.Topic)
	return srv.Run(ctx)
}
