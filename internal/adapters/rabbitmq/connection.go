// Package rabbitmq contains the AMQP adapter used by the worker.
package rabbitmq

import (
	"context"
	"errors"
	"fmt"
	"time"

	amqp "github.com/rabbitmq/amqp091-go"
)

// Connection wraps an AMQP connection and exposes health semantics.
type Connection struct {
	connection *amqp.Connection
}

// Open establishes an AMQP connection for a named process.
func Open(url, connectionName string) (*Connection, error) {
	conn, err := amqp.DialConfig(url, amqp.Config{
		Heartbeat:  10 * time.Second,
		Properties: amqp.Table{"connection_name": connectionName},
	})
	if err != nil {
		return nil, fmt.Errorf("dial rabbitmq: %w", err)
	}
	return &Connection{connection: conn}, nil
}

// Ping reports whether the AMQP connection is still open.
func (c *Connection) Ping(ctx context.Context) error {
	select {
	case <-ctx.Done():
		return ctx.Err()
	default:
	}
	if c == nil || c.connection == nil || c.connection.IsClosed() {
		return errors.New("rabbitmq connection is closed")
	}
	return nil
}

// Close closes the AMQP connection.
func (c *Connection) Close() error {
	if c == nil || c.connection == nil || c.connection.IsClosed() {
		return nil
	}
	return c.connection.Close()
}
