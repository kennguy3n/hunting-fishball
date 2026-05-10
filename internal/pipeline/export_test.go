package pipeline

import (
	"context"

	"github.com/IBM/sarama"
)

// ObserveMessageForTest exposes the unexported observeMessage method
// to the external pipeline_test package so unit tests can drive the
// DLQ observer without standing up Kafka. Build-tag-free: this file
// is only compiled into the test binary because of the _test.go
// suffix.
func ObserveMessageForTest(o *DLQObserver, msg *sarama.ConsumerMessage) {
	o.observeMessage(msg)
}

// DLQConsumerPersistForTest exposes the unexported persist method on
// DLQConsumer so unit tests can drive the consumer's storage path
// without spinning up Kafka.
func DLQConsumerPersistForTest(c *DLQConsumer, msg *sarama.ConsumerMessage) {
	c.persist(context.Background(), msg)
}
