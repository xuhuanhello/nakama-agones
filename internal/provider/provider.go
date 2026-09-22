// Package provider defines the hosting boundary used by the durable room manager.
package provider

import (
	"context"
	"errors"
	"fmt"
	"time"
)

// Start is idempotent for the same worker and immutable configuration. Hosts must
// reconcile a timed-out create under that same identity, never create a new name.
type Provider interface {
	Start(context.Context, StartRequest) (Instance, error)
	Get(context.Context, string) (Instance, error)
	List(context.Context) ([]Instance, error)
	Stop(context.Context, string) error
}

type CPUResources struct{ Request, Limit string }

type StartRequest struct {
	CPUResources         *CPUResources
	Name                 string
	Region               string
	CustomData           map[string]any
	EnvironmentVariables map[string]string `json:"-"`
}

type Port struct {
	Name         string
	Host         string
	ExternalPort int
	InternalPort int
	Protocol     string
}

// A zero TTL means the host imposes no expiry. InstanceID must include a resource
// generation identity so a reused name cannot redirect a stale get or delete.
type Instance struct {
	InstanceID   string
	Status       string // launching, running, unhealthy, or stopped
	NetworkPorts []Port
	CustomData   map[string]any
	TTL          int
	StartedAt    string
}

// APIError exposes only safe classifications, never payloads, credentials, raw
// HTTP responses or transport errors which might contain credential-bearing URLs.
type APIError struct {
	Operation      string
	StatusCode     int
	Reason         string
	RetryAfter     time.Duration
	OutcomeUnknown bool
	Cause          error // Only context.Canceled or context.DeadlineExceeded.
}

func (e *APIError) Error() string {
	if e.StatusCode != 0 {
		return fmt.Sprintf("agones %s: %s (HTTP %d)", e.Operation, e.Reason, e.StatusCode)
	}
	return fmt.Sprintf("agones %s: %s", e.Operation, e.Reason)
}

func (e *APIError) Unwrap() error {
	if e.Cause == context.Canceled || e.Cause == context.DeadlineExceeded {
		return e.Cause
	}
	return nil
}

func (e *APIError) Retryable() bool {
	return !e.OutcomeUnknown && !errors.Is(e, context.Canceled) &&
		(e.Reason == "transport_error" || e.StatusCode == 408 || e.StatusCode == 429 || e.StatusCode >= 500)
}

func IsOutcomeUnknown(err error) bool {
	var e *APIError
	return errors.As(err, &e) && e.OutcomeUnknown
}

func IsRetryable(err error) bool {
	var e *APIError
	return errors.As(err, &e) && e.Retryable()
}
