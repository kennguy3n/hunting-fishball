package pipeline_test

import (
	"testing"
	"time"

	"github.com/IBM/sarama"

	"github.com/kennguy3n/hunting-fishball/internal/pipeline"
)

func TestSaramaConfigDefaults(t *testing.T) {
	t.Parallel()
	cfg := pipeline.SaramaConfig()
	if cfg.Consumer.Group.Session.Timeout != 30*time.Second {
		t.Fatalf("session timeout = %v", cfg.Consumer.Group.Session.Timeout)
	}
	if len(cfg.Consumer.Group.Rebalance.GroupStrategies) == 0 ||
		cfg.Consumer.Group.Rebalance.GroupStrategies[0].Name() != sarama.NewBalanceStrategySticky().Name() {
		t.Fatalf("default strategy not sticky: %+v", cfg.Consumer.Group.Rebalance.GroupStrategies)
	}
}

func TestSaramaConfigWithRange(t *testing.T) {
	t.Parallel()
	cfg := pipeline.SaramaConfigWith(pipeline.ConsumerTuning{
		SessionTimeout:    45 * time.Second,
		MaxPollInterval:   10 * time.Minute,
		RebalanceStrategy: "range",
	})
	if cfg.Consumer.Group.Session.Timeout != 45*time.Second {
		t.Fatalf("session timeout = %v", cfg.Consumer.Group.Session.Timeout)
	}
	if cfg.Consumer.MaxProcessingTime != 10*time.Minute {
		t.Fatalf("max processing time = %v", cfg.Consumer.MaxProcessingTime)
	}
	if cfg.Consumer.Group.Rebalance.GroupStrategies[0].Name() != sarama.NewBalanceStrategyRange().Name() {
		t.Fatalf("strategy not range: %s", cfg.Consumer.Group.Rebalance.GroupStrategies[0].Name())
	}
}
