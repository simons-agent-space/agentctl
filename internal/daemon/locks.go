package daemon

import (
	"sync"
)

// appLocker serializes conflicting operations per app while
// allowing independent operations for different apps to run in
// parallel. The map is protected by mu; each per-app lock is a
// pointer stored once and reused for the lifetime of the
// daemon. Acquire blocks until the per-app lock is held; it
// returns a release function the caller must defer. The release
// function does NOT delete the map entry so the lock pointer
// stays stable and other waiters can be unblocked.
//
// TryAcquire is the non-blocking variant; it returns
// ErrConcurrentOperation immediately when another caller holds
// the per-app lock. The daemon uses TryAcquire for handlers so
// concurrent requests fail fast with 409 Conflict instead of
// queueing indefinitely.
type appLocker struct {
	mu    sync.Mutex
	locks map[string]*sync.Mutex
}

func newAppLocker() *appLocker {
	return &appLocker{locks: make(map[string]*sync.Mutex)}
}

// lockFor returns the per-app mutex, creating it on first use.
// It must be called with l.mu held.
func (a *appLocker) lockFor(app string) *sync.Mutex {
	if m, ok := a.locks[app]; ok {
		return m
	}
	m := &sync.Mutex{}
	a.locks[app] = m
	return m
}

// TryAcquire attempts to lock the per-app slot for app without
// blocking. Returns ErrConcurrentOperation when another caller
// holds the lock; otherwise returns a release function the
// caller must defer.
func (a *appLocker) TryAcquire(app string) (func(), error) {
	a.mu.Lock()
	m := a.lockFor(app)
	a.mu.Unlock()
	if !m.TryLock() {
		return nil, ErrConcurrentOperation
	}
	return m.Unlock, nil
}

// Acquire blocks until the per-app slot for app is locked. Used
// by tests that want to verify serialization rather than the
// 409 behaviour. Production handlers use TryAcquire.
func (a *appLocker) Acquire(app string) func() {
	a.mu.Lock()
	m := a.lockFor(app)
	a.mu.Unlock()
	m.Lock()
	return m.Unlock
}
