// Package rabbitmq contains the AMQP adapter used by the worker.
package rabbitmq

import (
	"context"
	"errors"
	"fmt"
	"net"
	"sync"
	"time"

	amqp "github.com/rabbitmq/amqp091-go"
)

// handshakeTimeout bounds connection setup when the caller passes no deadline.
const handshakeTimeout = 10 * time.Second

// Connection wraps an AMQP connection and exposes health semantics. It
// redials on demand, because the driver never does it for us.
type Connection struct {
	url  string
	name string

	operationMu sync.Mutex
	mu          sync.Mutex
	connection  *amqp.Connection
	transport   *deadlineConn
}

// New describes a connection without dialing it. The first Channel call, or an
// explicit Connect, establishes it.
func New(url, connectionName string) *Connection {
	return &Connection{url: url, name: connectionName}
}

// Open establishes an AMQP connection eagerly, for callers that want startup
// to fail when the broker is unreachable.
func Open(ctx context.Context, url, connectionName string) (*Connection, error) {
	connection := New(url, connectionName)
	if err := connection.Connect(ctx); err != nil {
		return nil, err
	}
	return connection, nil
}

// Connect establishes the connection if it is not live, within ctx.
func (c *Connection) Connect(ctx context.Context) error {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.connect(ctx)
}

// connect requires the caller to hold mu.
func (c *Connection) connect(ctx context.Context) error {
	if c.connection != nil && !c.connection.IsClosed() {
		return nil
	}
	if err := ctx.Err(); err != nil {
		return err
	}
	conn, transport, err := dial(ctx, c.url, c.name)
	if err != nil {
		return err
	}
	c.connection = conn
	c.transport = transport
	return nil
}

// dial honours the caller's deadline for the whole setup, not just the TCP
// connect.
//
// Bounding only the connect is not enough: a host that accepts the socket and
// then says nothing leaves the driver blocked reading the AMQP handshake, with
// no deadline of its own. The driver expects the Dial function to set one on
// the connection — that is exactly what its own DefaultDial does — so the
// deadline is applied here and cleared once the handshake is through.
func dial(ctx context.Context, url, connectionName string) (*amqp.Connection, *deadlineConn, error) {
	dialer := &net.Dialer{}
	var transport *deadlineConn
	conn, err := amqp.DialConfig(url, amqp.Config{
		Heartbeat:  10 * time.Second,
		Properties: amqp.Table{"connection_name": connectionName},
		Dial: func(network, addr string) (net.Conn, error) {
			dialed, err := dialer.DialContext(ctx, network, addr)
			if err != nil {
				return nil, err
			}
			bounded := newDeadlineConn(dialed)
			deadline, ok := ctx.Deadline()
			if !ok {
				// No caller deadline: fall back to a bounded handshake rather
				// than allowing an indefinite one.
				deadline = time.Now().Add(handshakeTimeout)
			}
			if err := bounded.SetDeadline(deadline); err != nil {
				_ = dialed.Close()
				return nil, err
			}
			transport = bounded
			return bounded, nil
		},
	})
	if err != nil {
		return nil, nil, fmt.Errorf("dial rabbitmq: %w", err)
	}
	if transport == nil {
		_ = conn.Close()
		return nil, nil, errors.New("dial rabbitmq: transport was not initialized")
	}
	return conn, transport, nil
}

// withDeadline keeps a hard deadline on the underlying socket while fn uses
// the AMQP connection. The driver's Context variants only check cancellation
// before some writes and while waiting for confirms; synchronous RPCs such as
// Channel, Confirm and ExchangeDeclare otherwise have no per-call deadline.
//
// The operation mutex is intentional. A socket has one pair of deadlines, so
// two callers cannot safely impose different budgets on the same connection.
func (c *Connection) withDeadline(ctx context.Context, fn func() error) (err error) {
	deadline, ok := ctx.Deadline()
	if !ok {
		return errors.New("rabbitmq operation requires a deadline")
	}

	c.operationMu.Lock()
	defer c.operationMu.Unlock()

	if err := c.Connect(ctx); err != nil {
		return err
	}

	c.mu.Lock()
	transport := c.transport
	c.mu.Unlock()
	if transport == nil {
		return errors.New("rabbitmq transport is unavailable")
	}
	if err := transport.limit(deadline); err != nil {
		return fmt.Errorf("set rabbitmq operation deadline: %w", err)
	}
	defer func() {
		if clearErr := transport.clearLimit(); err == nil && clearErr != nil {
			err = fmt.Errorf("clear rabbitmq operation deadline: %w", clearErr)
		}
	}()

	return fn()
}

// Channel returns a channel on a live connection, redialing within ctx when
// the previous connection died.
//
// amqp091-go does not reconnect on its own: once the TCP connection is gone,
// every channel opened from it fails forever. A relay that only reopened
// channels would therefore never recover from a broker restart.
func (c *Connection) Channel(ctx context.Context) (*amqp.Channel, error) {
	var channel *amqp.Channel
	err := c.withDeadline(ctx, func() error {
		var err error
		channel, err = c.channel(ctx)
		return err
	})
	return channel, err
}

// withChannel keeps the caller's deadline installed for the complete lifetime
// of a short-lived channel operation. Channel alone can only bound opening the
// channel: synchronous RPCs performed after it returns would otherwise run
// after withDeadline had cleared the socket deadline.
//
// It is intentionally unexported. Long-lived publishers and consumers manage
// their channels separately; this helper is for bounded inspections such as
// passive queue declarations.
func (c *Connection) withChannel(ctx context.Context, use func(*amqp.Channel) error) error {
	return c.withDeadline(ctx, func() error {
		channel, err := c.channel(ctx)
		if err != nil {
			return err
		}
		defer func() { _ = channel.Close() }()
		return use(channel)
	})
}

// channel opens a channel while the caller already owns operationMu and has a
// deadline installed on the transport.
func (c *Connection) channel(ctx context.Context) (*amqp.Channel, error) {
	c.mu.Lock()
	defer c.mu.Unlock()

	if err := c.connect(ctx); err != nil {
		return nil, err
	}
	channel, err := c.connection.Channel()
	if err != nil {
		// The connection may have died between the check and here; drop it so
		// the next call redials instead of failing the same way forever.
		if c.connection != nil && c.connection.IsClosed() {
			c.connection = nil
		}
		return nil, fmt.Errorf("open channel: %w", err)
	}
	return channel, nil
}

// deadlineConn remembers the deadlines requested by the AMQP driver and caps
// them with the current operation deadline. This matters because the heartbeat
// loop periodically extends the read deadline; without the cap it could undo a
// shorter publish timeout while an RPC was waiting for the broker.
type deadlineConn struct {
	net.Conn

	mu                sync.Mutex
	readDeadline      time.Time
	writeDeadline     time.Time
	operationDeadline time.Time
}

func newDeadlineConn(conn net.Conn) *deadlineConn { return &deadlineConn{Conn: conn} }

func (c *deadlineConn) SetDeadline(deadline time.Time) error {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.readDeadline = deadline
	c.writeDeadline = deadline
	return c.applyLocked()
}

func (c *deadlineConn) SetReadDeadline(deadline time.Time) error {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.readDeadline = deadline
	return c.Conn.SetReadDeadline(earlier(deadline, c.operationDeadline))
}

func (c *deadlineConn) SetWriteDeadline(deadline time.Time) error {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.writeDeadline = deadline
	return c.Conn.SetWriteDeadline(earlier(deadline, c.operationDeadline))
}

func (c *deadlineConn) limit(deadline time.Time) error {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.operationDeadline = deadline
	return c.applyLocked()
}

func (c *deadlineConn) clearLimit() error {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.operationDeadline = time.Time{}
	return c.applyLocked()
}

func (c *deadlineConn) applyLocked() error {
	if err := c.Conn.SetReadDeadline(earlier(c.readDeadline, c.operationDeadline)); err != nil {
		return err
	}
	return c.Conn.SetWriteDeadline(earlier(c.writeDeadline, c.operationDeadline))
}

func earlier(first, second time.Time) time.Time {
	if first.IsZero() {
		return second
	}
	if second.IsZero() || first.Before(second) {
		return first
	}
	return second
}

// Ping reports whether the AMQP connection is still open.
func (c *Connection) Ping(ctx context.Context) error {
	select {
	case <-ctx.Done():
		return ctx.Err()
	default:
	}
	if c == nil {
		return errors.New("rabbitmq connection is closed")
	}
	c.mu.Lock()
	defer c.mu.Unlock()
	if c.connection == nil || c.connection.IsClosed() {
		return errors.New("rabbitmq connection is closed")
	}
	return nil
}

// Close closes the AMQP connection.
func (c *Connection) Close() error {
	if c == nil {
		return nil
	}
	c.mu.Lock()
	defer c.mu.Unlock()
	if c.connection == nil || c.connection.IsClosed() {
		return nil
	}
	return c.connection.Close()
}
