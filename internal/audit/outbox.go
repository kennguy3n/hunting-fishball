package audit

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"time"

	"github.com/IBM/sarama"
)

// MessageProducer is the narrow interface the outbox needs from a Kafka
// producer. Production code wires this to sarama.SyncProducer; tests
// inject an in-memory fake.
type MessageProducer interface {
	SendMessage(*sarama.ProducerMessage) (partition int32, offset int64, err error)
	Close() error
}

// OutboxConfig configures the background poller.
type OutboxConfig struct {
	// Topic is the Kafka topic that audit events are published to.
	// Required.
	Topic string

	// PollInterval is how often the poller wakes up to drain pending
	// rows. Defaults to 1s when zero.
	PollInterval time.Duration

	// BatchSize is the maximum number of rows pulled per tick.
	// Defaults to 100 when zero.
	BatchSize int

	// Now lets tests inject a deterministic clock. Defaults to time.Now.
	Now func() time.Time
}

func (c *OutboxConfig) defaults() {
	if c.PollInterval == 0 {
		c.PollInterval = time.Second
	}
	if c.BatchSize == 0 {
		c.BatchSize = 100
	}
	if c.Now == nil {
		c.Now = time.Now
	}
}

// Outbox is the background poller that drains pending audit rows into
// Kafka. It is the second half of the transactional outbox pattern:
// business code writes the audit row to PG, and Outbox.Run wakes
// periodically to publish.
type Outbox struct {
	repo     *Repository
	producer MessageProducer
	cfg      OutboxConfig
}

// NewOutbox constructs an Outbox. The producer is owned by the caller —
// Outbox.Run will not Close it on shutdown.
func NewOutbox(repo *Repository, producer MessageProducer, cfg OutboxConfig) (*Outbox, error) {
	if repo == nil {
		return nil, errors.New("audit: nil repository")
	}
	if producer == nil {
		return nil, errors.New("audit: nil producer")
	}
	if cfg.Topic == "" {
		return nil, errors.New("audit: outbox topic required")
	}
	cfg.defaults()

	return &Outbox{repo: repo, producer: producer, cfg: cfg}, nil
}

// Run drives the poller until ctx is cancelled. Each tick drains one
// batch of pending rows; if the batch is full, the loop ticks
// immediately so backlogs drain quickly.
func (o *Outbox) Run(ctx context.Context) error {
	t := time.NewTicker(o.cfg.PollInterval)
	defer t.Stop()

	for {
		drained, err := o.RunOnce(ctx)
		if err != nil && !errors.Is(err, context.Canceled) {
			return err
		}
		if drained >= o.cfg.BatchSize {
			// Backlog: re-tick immediately.
			continue
		}
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-t.C:
		}
	}
}

// RunOnce pulls one batch and publishes it. Returns the number of rows
// successfully published. Exposed so tests can drive the loop
// deterministically.
func (o *Outbox) RunOnce(ctx context.Context) (int, error) {
	rows, err := o.repo.PendingPublish(ctx, o.cfg.BatchSize)
	if err != nil {
		return 0, err
	}
	if len(rows) == 0 {
		return 0, nil
	}

	publishedIDs := make([]string, 0, len(rows))
	for i := range rows {
		body, err := json.Marshal(&rows[i])
		if err != nil {
			return len(publishedIDs), fmt.Errorf("audit: marshal log %s: %w", rows[i].ID, err)
		}

		msg := &sarama.ProducerMessage{
			Topic: o.cfg.Topic,
			Key:   sarama.StringEncoder(rows[i].TenantID),
			Value: sarama.ByteEncoder(body),
		}
		if _, _, err := o.producer.SendMessage(msg); err != nil {
			// Stop on first failure so we retry on the next tick rather
			// than hammering a broken broker.
			if len(publishedIDs) > 0 {
				_ = o.repo.MarkPublished(ctx, publishedIDs, o.cfg.Now())
			}

			return len(publishedIDs), fmt.Errorf("audit: publish log %s: %w", rows[i].ID, err)
		}
		publishedIDs = append(publishedIDs, rows[i].ID)
	}

	if err := o.repo.MarkPublished(ctx, publishedIDs, o.cfg.Now()); err != nil {
		return len(publishedIDs), err
	}

	return len(publishedIDs), nil
}
