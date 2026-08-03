// Package daemon exposes the host-side agentctl control plane over a
// local Unix domain socket. It is a thin transport layer over the
// existing deploy package: handlers validate request shapes, acquire
// per-app locks, and delegate to the deploy / rollback entry points.
// The daemon never accepts arbitrary host paths, Docker arguments,
// shell commands, Caddy fragments, or container names; every
// filesystem path is derived from trusted host configuration, and
// every candidate identity is checked against the fixed naming
// rules before any side effect.
//
// Sentinel errors returned by the daemon layer. errors.Is works for
// every one of these. The deploy and rollback layers' sentinels
// (ErrDeploymentFailed, ErrRollbackFailed, ErrInvalidDeployInput,
// ErrInvalidInput, ErrDeploymentStateNotFound, ...) are preserved
// when the daemon wraps the underlying error; callers can branch on
// either layer's sentinels.
package daemon

import (
	"errors"
)

// Sentinel errors owned by the daemon layer.
var (
	// ErrInvalidRequest is returned for malformed requests (bad
	// JSON, missing fields, app-name regex failure, commit not 40
	// lowercase hex characters, manifest parse failure, method
	// mismatch, body too large). The underlying validation error
	// is preserved via %w so callers can errors.Is on it.
	ErrInvalidRequest = errors.New("invalid request")

	// ErrAppNotManaged is returned when an operation targets an
	// app whose state file does not exist in the configured state
	// directory. Callers should treat this as "unknown app".
	ErrAppNotManaged = errors.New("app is not managed by this daemon")

	// ErrConcurrentOperation is returned when another request is
	// already holding the per-app lock for a mutating operation
	// (deploy, rollback, inspect that resolves sources). The
	// daemon serializes conflicting operations per app; an
	// independent operation for a different app proceeds in
	// parallel.
	ErrConcurrentOperation = errors.New("another operation is in progress for this app")

	// ErrShuttingDown is returned by handlers after the server
	// has begun graceful shutdown. In-flight operations are
	// allowed to complete; new requests are rejected.
	ErrShuttingDown = errors.New("daemon is shutting down")

	// ErrNoPreviousDeployment is returned when a rollback request
	// targets an app whose state file has no Previous slot. This
	// is a wrapper around the deploy-layer sentinel of the same
	// name so callers can branch via errors.Is in either layer.
	ErrNoPreviousDeployment = errors.New("no previous deployment to roll back to")

	// ErrHealthPathRequired is returned when a rollback request
	// omits the health_path field. The deploy-state schema
	// deliberately does not persist health_path (it is a
	// manifest value, not a deployment identity value), so the
	// caller must supply it on rollback.
	ErrHealthPathRequired = errors.New("health_path is required for rollback")
)
