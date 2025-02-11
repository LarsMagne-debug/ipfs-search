package amqp

import (
	"context"

	"github.com/streadway/amqp"
	"go.opentelemetry.io/otel/api/trace"
	"go.opentelemetry.io/otel/codes"
	"go.opentelemetry.io/otel/label"

	"github.com/ipfs-search/ipfs-search/instr"
)

// Channel wraps an AMQP channel
type Channel struct {
	ch *amqp.Channel
	*instr.Instrumentation
}

// Queue creates a named queue on a given chennel
func (c *Channel) Queue(ctx context.Context, name string) (*Queue, error) {
	ctx, span := c.Tracer.Start(ctx, "queue.amqp.Channel.Queue", trace.WithAttributes(label.String("queue", name)))
	defer span.End()

	_, err := c.ch.QueueDeclare(
		name,  // name
		true,  // durable
		false, // delete when unused
		false, // exclusive
		false, // no-wait
		amqp.Table{
			"x-max-priority": 9,                       // Enable all 9 priorities
			"x-message-ttl":  1000 * 60 * 60 * 24 * 7, // Expire messages after 1 week
			"x-queue-mode":   "lazy",                  // Allow RabbitMQ to write queue to disk as fast as possible
		},
	)
	if err != nil {
		span.RecordError(ctx, err, trace.WithErrorStatus(codes.Error))
		return nil, err
	}

	return &Queue{
		channel:         c,
		name:            name,
		Instrumentation: c.Instrumentation,
	}, nil
}

// Close closes a Channel
func (c *Channel) Close() error {
	return c.ch.Close()
}
