// Package retry provides bounded startup retries for external dependencies.
package retry

import (
	"context"
	"fmt"
	"time"
)

// Do retries fn with exponential backoff until it succeeds or ctx ends.
func Do(ctx context.Context, initialDelay, maximumDelay time.Duration, fn func() error) error {
	delay := initialDelay
	var lastErr error

	for {
		err := fn()
		if err == nil {
			return nil
		}
		lastErr = err

		timer := time.NewTimer(delay)
		select {
		case <-ctx.Done():
			timer.Stop()
			return fmt.Errorf("retry deadline reached: last error: %w; context: %w", lastErr, ctx.Err())
		case <-timer.C:
		}

		delay *= 2
		if delay > maximumDelay {
			delay = maximumDelay
		}
	}
}
