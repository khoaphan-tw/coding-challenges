package broker

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"math/rand/v2"
	"ridehail/pkg/messaging"
	"strings"
	"time"

	"github.com/twmb/franz-go/pkg/kadm"
	"github.com/twmb/franz-go/pkg/kerr"
	"github.com/twmb/franz-go/pkg/kgo"
	"go.opentelemetry.io/otel"
	"go.opentelemetry.io/otel/attribute"
	"go.opentelemetry.io/otel/codes"
	"go.opentelemetry.io/otel/metric"
	"go.opentelemetry.io/otel/propagation"
	"go.opentelemetry.io/otel/trace"

	"ridehail/pkg/obs"
)

// NewProducer returns an idempotent producer (acks=all, key-hash partitioning).
func NewProducer(brokers []string) (*kgo.Client, error) {
	return kgo.NewClient(kgo.SeedBrokers(brokers...), kgo.ProducerLinger(0))
}

// EnsureTopics creates topics (and their DLQs) if they do not exist.
func EnsureTopics(ctx context.Context, cl *kgo.Client, partitions int32, topics ...string) error {
	adm := kadm.NewClient(cl)
	all := make([]string, 0, len(topics)*2)
	for _, t := range topics {
		all = append(all, t, messaging.DLQ(t))
	}
	var lastErr error
	for i := 0; i < 30; i++ {
		resp, err := adm.CreateTopics(ctx, partitions, 1, nil, all...)
		if err == nil {
			lastErr = nil
			for _, r := range resp {
				if r.Err != nil && !errors.Is(r.Err, kerr.TopicAlreadyExists) {
					lastErr = r.Err
				}
			}
			if lastErr == nil {
				return nil
			}
		} else {
			lastErr = err
		}
		slog.WarnContext(ctx, "waiting for broker to create topics", "error", lastErr)
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-time.After(2 * time.Second):
		}
	}
	return fmt.Errorf("ensure topics: %w", lastErr)
}

// Consumer reads topics in a consumer group and calls Handler for each record,
// in partition order. Offsets are committed only after the handler succeeded
// (or the record went to the DLQ): at-least-once delivery.
type Consumer struct {
	Name        string // consumer group, also the inbox "consumer" value
	Brokers     []string
	Topics      []string
	Handler     messaging.Handler
	DLQProducer *kgo.Client
	MaxAttempts int
}

// Run consumes until ctx is cancelled.
func (c *Consumer) Run(ctx context.Context) error {
	if c.MaxAttempts == 0 {
		c.MaxAttempts = 5
	}
	cl, err := kgo.NewClient(
		kgo.SeedBrokers(c.Brokers...),
		kgo.ConsumerGroup(c.Name),
		kgo.ConsumeTopics(c.Topics...),
		kgo.DisableAutoCommit(),
		kgo.BlockRebalanceOnPoll(),
		kgo.ConsumeResetOffset(kgo.NewOffset().AtStart()),
		// A crashed consumer's partitions move to a live one after 10 s.
		kgo.SessionTimeout(10*time.Second),
		kgo.HeartbeatInterval(time.Second),
	)
	if err != nil {
		return err
	}
	// With BlockRebalanceOnPoll, AllowRebalance must run before Close, or
	// Close blocks and the member never leaves the group cleanly.
	defer func() {
		cl.AllowRebalance()
		cl.Close()
	}()

	meter := otel.Meter("ridehail/messaging")
	consumed, _ := meter.Int64Counter("messages_consumed_total")
	dlqCount, _ := meter.Int64Counter("dlq_messages_total")

	for {
		fetches := cl.PollFetches(ctx)
		if ctx.Err() != nil {
			return nil
		}
		fetches.EachError(func(t string, p int32, err error) {
			slog.ErrorContext(ctx, "fetch error", "topic", t, "partition", p, "error", err)
		})
		fetches.EachRecord(func(rec *kgo.Record) {
			outcome := c.process(ctx, rec)
			consumed.Add(ctx, 1, metric.WithAttributes(
				attribute.String("consumer", c.Name), attribute.String("topic", rec.Topic), attribute.String("outcome", outcome)))
			if outcome == "dlq" {
				dlqCount.Add(ctx, 1, metric.WithAttributes(attribute.String("topic", rec.Topic)))
			}
		})
		if err := cl.CommitUncommittedOffsets(ctx); err != nil && ctx.Err() == nil {
			slog.ErrorContext(ctx, "commit offsets failed", "error", err)
		}
		cl.AllowRebalance()
	}
}

type headerCarrier []kgo.RecordHeader

func (h headerCarrier) Get(k string) string {
	for _, x := range h {
		if strings.EqualFold(x.Key, k) {
			return string(x.Value)
		}
	}
	return ""
}
func (h headerCarrier) Set(string, string) {}
func (h headerCarrier) Keys() []string {
	out := make([]string, 0, len(h))
	for _, x := range h {
		out = append(out, x.Key)
	}
	return out
}

var _ propagation.TextMapCarrier = headerCarrier(nil)

func (c *Consumer) process(ctx context.Context, rec *kgo.Record) string {
	ctx = otel.GetTextMapPropagator().Extract(ctx, headerCarrier(rec.Headers))
	var env messaging.Envelope
	if err := json.Unmarshal(rec.Value, &env); err != nil || env.EventID == "" {
		slog.ErrorContext(ctx, "malformed message, sending to DLQ", "topic", rec.Topic, "error", err)
		c.toDLQ(ctx, rec, fmt.Errorf("malformed envelope: %v", err), 0)
		return "dlq"
	}
	ctx, span := otel.Tracer("ridehail/messaging").Start(ctx, "consume "+env.Type,
		trace.WithSpanKind(trace.SpanKindConsumer),
		trace.WithAttributes(attribute.String("messaging.destination.name", rec.Topic),
			attribute.String("messaging.message.id", env.EventID),
			attribute.String("ride.id", env.RideID), attribute.String("driver.id", env.DriverID)))
	defer span.End()
	ctx = obs.With(ctx, obs.RideID, env.RideID, obs.DriverID, env.DriverID, obs.OfferID, env.OfferID,
		obs.EventID, env.EventID, obs.CausationID, env.CausationID, obs.MessageType, env.Type)
	slog.InfoContext(ctx, "message consumed", "direction", "consume", "consumer", c.Name,
		"topic", rec.Topic, "partition", rec.Partition, "offset", rec.Offset, "key", string(rec.Key),
		"value", obs.NewBody(rec.Value))

	backoff := 100 * time.Millisecond
	var err error
	for attempt := 1; attempt <= c.MaxAttempts; attempt++ {
		if err = c.Handler(ctx, env); err == nil {
			return "ok"
		}
		if ctx.Err() != nil {
			return "cancelled"
		}
		slog.WarnContext(ctx, "message handling failed, will retry", "attempt", attempt, "error", err)
		jitter := time.Duration(rand.Int64N(int64(backoff / 2)))
		select {
		case <-ctx.Done():
			return "cancelled"
		case <-time.After(backoff + jitter):
		}
		backoff = min(backoff*2, 5*time.Second)
	}
	span.SetStatus(codes.Error, err.Error())
	slog.ErrorContext(ctx, "message failed after retries, sent to DLQ", "attempts", c.MaxAttempts, "error", err)
	c.toDLQ(ctx, rec, err, c.MaxAttempts)
	return "dlq"
}

func (c *Consumer) toDLQ(ctx context.Context, rec *kgo.Record, cause error, attempts int) {
	if c.DLQProducer == nil {
		return
	}
	out := &kgo.Record{Topic: messaging.DLQ(rec.Topic), Key: rec.Key, Value: rec.Value, Headers: append([]kgo.RecordHeader{},
		rec.Headers...)}
	out.Headers = append(out.Headers,
		kgo.RecordHeader{Key: "dlq.error", Value: []byte(cause.Error())},
		kgo.RecordHeader{Key: "dlq.consumer", Value: []byte(c.Name)},
		kgo.RecordHeader{Key: "dlq.attempts", Value: []byte(fmt.Sprint(attempts))})
	if err := c.DLQProducer.ProduceSync(ctx, out).FirstErr(); err != nil {
		slog.ErrorContext(ctx, "DLQ produce failed", "error", err)
	}
}
