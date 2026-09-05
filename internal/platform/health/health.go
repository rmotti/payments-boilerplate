// Package health exposes dependency readiness without leaking error details.
package health

import (
	"context"
	"time"
)

// Checker verifies one required dependency.
type Checker func(context.Context) error

// Service evaluates process readiness.
type Service struct {
	serviceName string
	version     string
	checks      map[string]Checker
	timeout     time.Duration
}

// Result describes whether the process can accept work and which dependencies
// contributed to that decision.
type Result struct {
	Checks map[string]bool
	Ready  bool
}

// New creates a readiness service.
func New(serviceName, version string, checks map[string]Checker) *Service {
	return &Service{
		serviceName: serviceName,
		version:     version,
		checks:      checks,
		timeout:     2 * time.Second,
	}
}

// Check evaluates every required dependency.
func (s *Service) Check(ctx context.Context) Result {
	statuses := make(map[string]bool, len(s.checks))
	ready := true
	for name, check := range s.checks {
		checkCtx, cancel := context.WithTimeout(ctx, s.timeout)
		err := check(checkCtx)
		cancel()
		if err != nil {
			statuses[name] = false
			ready = false
			continue
		}
		statuses[name] = true
	}

	return Result{Checks: statuses, Ready: ready}
}

// ServiceName identifies the process represented by this readiness service.
func (s *Service) ServiceName() string { return s.serviceName }

// Version returns the application version represented by this readiness service.
func (s *Service) Version() string { return s.version }
