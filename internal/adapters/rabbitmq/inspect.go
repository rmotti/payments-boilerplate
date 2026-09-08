package rabbitmq

import (
	"context"
	"fmt"

	amqp "github.com/rabbitmq/amqp091-go"
)

// QueueDepths reports how many messages are ready in each named queue.
//
// It opens a channel of its own for the whole inspection and closes it before
// returning, because a passive declaration of a queue that does not exist
// closes the channel it ran on. The caller's context must carry a deadline:
// the connection refuses an unbounded operation, and this inspection is meant
// to run from a sampler with a budget, never from a production channel.
//
// The count is what the broker reports as ready. Messages already delivered
// to a consumer and not yet acknowledged are not in it.
func QueueDepths(ctx context.Context, connection *Connection, queues []string) (map[string]int, error) {
	depths := make(map[string]int, len(queues))
	err := connection.withChannel(ctx, func(channel *amqp.Channel) error {
		for _, queue := range queues {
			state, err := channel.QueueDeclarePassive(queue, true, false, false, false, nil)
			if err != nil {
				return fmt.Errorf("inspect queue %s: %w", queue, err)
			}
			depths[queue] = state.Messages
		}
		return nil
	})
	if err != nil {
		return nil, fmt.Errorf("inspect queue depths: %w", err)
	}
	return depths, nil
}
