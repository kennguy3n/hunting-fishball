package pipeline

import "github.com/IBM/sarama"

// ObserveMessageForTest exposes the unexported observeMessage method
// to the external pipeline_test package so unit tests can drive the
// DLQ observer without standing up Kafka. Build-tag-free: this file
// is only compiled into the test binary because of the _test.go
// suffix.
func ObserveMessageForTest(o *DLQObserver, msg *sarama.ConsumerMessage) {
	o.observeMessage(msg)
}
