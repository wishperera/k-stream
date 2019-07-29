package consumer

import (
	"context"
	"fmt"
	"github.com/Shopify/sarama"
	"github.com/pickme-go/errors"
	"github.com/pickme-go/metrics"
)

func init() {
	//sarama.Logger = log.New(os.Stdout, ``, log.Lmicroseconds)
}

type TopicPartition struct {
	Topic     string
	Partition int32
}

func (tp TopicPartition) String() string {
	return fmt.Sprintf(`%s-%d`, tp.Topic, tp.Partition)
}

type Consumer interface {
	Consume(tps []string, handler ReBalanceHandler) (chan Partition, error)
	Errors() <-chan *Error
	Close() error
}

type Offset int64

const (
	Earliest Offset = -2
	Latest   Offset = -1
)

func (o Offset) String() string {
	switch o {
	case -2:
		return `Earliest`
	case -1:
		return `Latest`
	default:
		return `Unknown`
	}
}

type consumer struct {
	config  *Config
	context struct {
		ctx    context.Context
		cancel context.CancelFunc
	}
	saramaGroup        sarama.ConsumerGroup
	saramaGroupHandler *groupHandler
	consumerErrors     chan *Error
	stopping           chan bool
	stopped            chan bool
}

func NewConsumer(config *Config) (Consumer, error) {

	if err := config.validate(); err != nil {
		return nil, err
	}

	c := &consumer{
		config:         config,
		consumerErrors: make(chan *Error, 1),
		stopping:       make(chan bool, 1),
		stopped:        make(chan bool, 1),
	}

	ctx, cancel := context.WithCancel(context.Background())
	c.context.ctx = ctx
	c.context.cancel = cancel

	return c, nil
}

func (c *consumer) Consume(tps []string, handler ReBalanceHandler) (chan Partition, error) {

	c.saramaGroupHandler = &groupHandler{
		reBalanceHandler: handler,
		partitions:       make(chan Partition, 1000),
		partitionMap:     make(map[string]*partition),
	}
	group, err := sarama.NewConsumerGroup(c.config.BootstrapServers, c.config.GroupId, c.config.Config)
	if err != nil {
		return nil, errors.WithPrevious(err, `k-stream.consumer`, "Failed to create consumer")
	}

	c.saramaGroup = group
	c.setUpMetrics()

	// Subscribe for all InputTopics,
	c.config.Logger.Info(`k-stream.consumer`, fmt.Sprintf(`subscribing to topics %v`, tps))

	go func() {
		for err := range group.Errors() {
			c.config.Logger.Error(`k-stream.consumer`, fmt.Sprintf("Error: %+v", err))
			c.consumerErrors <- &Error{err}
		}
	}()

	go c.consume(c.context.ctx, tps, c.saramaGroupHandler)

	return c.saramaGroupHandler.partitions, nil
}

func (c *consumer) consume(ctx context.Context, tps []string, h sarama.ConsumerGroupHandler) {
CLoop:
	for {
		if err := c.saramaGroup.Consume(ctx, tps, h); err != nil {
			c.config.Logger.Error(`k-stream.consumer`, err)
			break
		}

		select {
		case <-c.context.ctx.Done():
			c.config.Logger.Info(`k-stream.consumer`, fmt.Sprintf(`stopping consumer due to %s`, c.context.ctx.Err()))
			break CLoop
		default:
			continue CLoop
		}
	}

	c.stopped <- true
}

func (c *consumer) extractTps(kafkaTps map[string][]int32) []TopicPartition {
	tps := make([]TopicPartition, 0)
	for topic, partitions := range kafkaTps {
		for _, p := range partitions {
			tps = append(tps, TopicPartition{
				Topic:     topic,
				Partition: p,
			})
		}
	}
	return tps
}

func (c *consumer) Errors() <-chan *Error {
	return c.consumerErrors
}

func (c *consumer) Close() error {

	c.config.Logger.Info(`k-stream.consumer`, `upstream consumer is closing...`)
	defer c.config.Logger.Info(`k-stream.consumer`, `upstream consumer closed`)

	c.context.cancel()

	// close sarama consumer so application will leave from the consumer group
	if err := c.saramaGroup.Close(); err != nil {
		c.config.Logger.Error(`k-stream.consumer`,
			fmt.Sprintf(`cannot close consumer due to %+v`, err))
	}

	close(c.saramaGroupHandler.partitions)

	return nil
}

func (c *consumer) setUpMetrics() {
	c.saramaGroupHandler.metrics.commitLatency = c.config.MetricsReporter.Observer(metrics.MetricConf{
		Path:        `k_stream_consumer_commit_latency_microseconds`,
		ConstLabels: map[string]string{`group`: c.config.GroupId},
	})

	c.saramaGroupHandler.metrics.endToEndLatency = c.config.MetricsReporter.Observer(metrics.MetricConf{
		Path:        `k_stream_consumer_end_to_latency_latency_microseconds`,
		Labels:      []string{`topic`, `partition`},
		ConstLabels: map[string]string{`group`: c.config.GroupId},
	})
	c.saramaGroupHandler.metrics.reBalanceLatency = c.config.MetricsReporter.Observer(metrics.MetricConf{
		Path:        `k_stream_consumer_re_balance_latency_microseconds`,
		ConstLabels: map[string]string{`group`: c.config.GroupId},
	})
	c.saramaGroupHandler.metrics.reBalancing = c.config.MetricsReporter.Gauge(metrics.MetricConf{
		Path:        `k_stream_consumer_rebalancing`,
		ConstLabels: map[string]string{`group`: c.config.GroupId},
	})
	c.saramaGroupHandler.metrics.reBalancing = c.config.MetricsReporter.Gauge(metrics.MetricConf{
		Path:        `k_stream_consumer_rebalancing`,
		ConstLabels: map[string]string{`group`: c.config.GroupId},
	})
}
