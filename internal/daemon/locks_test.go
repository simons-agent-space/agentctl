package daemon

import (
	"errors"
	"sync"
	"sync/atomic"
	"testing"
	"time"
)

// TestAppLocker_TryAcquireSucceeds ensures the basic
// acquire/release path on an uncontended slot.
func TestAppLocker_TryAcquireSucceeds(t *testing.T) {
	l := newAppLocker()
	release, err := l.TryAcquire("agentctl")
	if err != nil {
		t.Fatalf("TryAcquire: %v", err)
	}
	if release == nil {
		t.Fatal("release function is nil")
	}
	release()
}

// TestAppLocker_TryAcquireConflicts verifies the second caller
// against the same app gets ErrConcurrentOperation while the
// first holds the lock.
func TestAppLocker_TryAcquireConflicts(t *testing.T) {
	l := newAppLocker()
	release, err := l.TryAcquire("agentctl")
	if err != nil {
		t.Fatalf("first TryAcquire: %v", err)
	}
	defer release()

	_, err = l.TryAcquire("agentctl")
	if !errors.Is(err, ErrConcurrentOperation) {
		t.Fatalf("second TryAcquire: want ErrConcurrentOperation, got %v", err)
	}
}

// TestAppLocker_DifferentAppsIndependent verifies the per-app
// lock does not block unrelated apps. This is the property the
// "allow independent operations for different apps where
// practical" requirement depends on.
func TestAppLocker_DifferentAppsIndependent(t *testing.T) {
	l := newAppLocker()
	releaseA, err := l.TryAcquire("agentctl")
	if err != nil {
		t.Fatalf("TryAcquire agentctl: %v", err)
	}
	defer releaseA()

	releaseB, err := l.TryAcquire("other-app")
	if err != nil {
		t.Fatalf("TryAcquire other-app: %v", err)
	}
	defer releaseB()
}

// TestAppLocker_ReleaseAllowsNextCaller ensures the lock is
// reusable after release; otherwise a long-running daemon would
// deadlock after the first deploy.
func TestAppLocker_ReleaseAllowsNextCaller(t *testing.T) {
	l := newAppLocker()
	release, err := l.TryAcquire("agentctl")
	if err != nil {
		t.Fatalf("first TryAcquire: %v", err)
	}
	release()

	release2, err := l.TryAcquire("agentctl")
	if err != nil {
		t.Fatalf("second TryAcquire after release: %v", err)
	}
	release2()
}

// TestAppLocker_AcquireBlocksUntilReleased exercises the
// blocking variant used by tests that want to verify
// serialization rather than the 409 path.
func TestAppLocker_AcquireBlocksUntilReleased(t *testing.T) {
	l := newAppLocker()
	release, err := l.TryAcquire("agentctl")
	if err != nil {
		t.Fatalf("first TryAcquire: %v", err)
	}

	gotLock := make(chan struct{})
	go func() {
		r := l.Acquire("agentctl")
		close(gotLock)
		r()
	}()

	select {
	case <-gotLock:
		t.Fatal("Acquire returned while first TryAcquire still held")
	case <-time.After(50 * time.Millisecond):
	}

	release()

	select {
	case <-gotLock:
	case <-time.After(time.Second):
		t.Fatal("Acquire did not unblock after release")
	}
}

// TestAppLocker_ConcurrentSameAppSerialized runs N goroutines
// that all TryAcquire the same app and verifies exactly one
// succeeds at a time. This is the concurrency-safety property
// the requirement "serialize conflicting operations per app so
// two deploy or rollback operations cannot race" depends on.
func TestAppLocker_ConcurrentSameAppSerialized(t *testing.T) {
	l := newAppLocker()
	const goroutines = 16

	var inFlight atomic.Int32
	var maxInFlight atomic.Int32
	var wg sync.WaitGroup
	wg.Add(goroutines)

	for i := 0; i < goroutines; i++ {
		go func() {
			defer wg.Done()
			release, err := l.TryAcquire("agentctl")
			if err != nil {
				return
			}
			cur := inFlight.Add(1)
			// Track the maximum concurrency observed.
			for {
				prev := maxInFlight.Load()
				if cur <= prev || maxInFlight.CompareAndSwap(prev, cur) {
					break
				}
			}
			time.Sleep(5 * time.Millisecond)
			inFlight.Add(-1)
			release()
		}()
	}
	wg.Wait()

	if got := maxInFlight.Load(); got != 1 {
		t.Fatalf("max concurrent holders = %d, want 1", got)
	}
}

// TestAppLocker_ConcurrentDifferentAppsIndependent runs N
// goroutines per app on two different apps and verifies both
// apps can be locked simultaneously. The "independent
// operations for different apps where practical" requirement
// depends on this property.
func TestAppLocker_ConcurrentDifferentAppsIndependent(t *testing.T) {
	l := newAppLocker()
	const perApp = 8

	var inFlightA atomic.Int32
	var inFlightB atomic.Int32
	var maxA atomic.Int32
	var maxB atomic.Int32

	var wg sync.WaitGroup
	wg.Add(perApp * 2)

	for i := 0; i < perApp; i++ {
		go func() {
			defer wg.Done()
			release, err := l.TryAcquire("agentctl")
			if err != nil {
				return
			}
			defer release()
			cur := inFlightA.Add(1)
			for {
				prev := maxA.Load()
				if cur <= prev || maxA.CompareAndSwap(prev, cur) {
					break
				}
			}
			time.Sleep(10 * time.Millisecond)
			inFlightA.Add(-1)
		}()
		go func() {
			defer wg.Done()
			release, err := l.TryAcquire("other-app")
			if err != nil {
				return
			}
			defer release()
			cur := inFlightB.Add(1)
			for {
				prev := maxB.Load()
				if cur <= prev || maxB.CompareAndSwap(prev, cur) {
					break
				}
			}
			time.Sleep(10 * time.Millisecond)
			inFlightB.Add(-1)
		}()
	}
	wg.Wait()

	if got := maxA.Load(); got != 1 {
		t.Errorf("max concurrent holders for agentctl = %d, want 1", got)
	}
	if got := maxB.Load(); got != 1 {
		t.Errorf("max concurrent holders for other-app = %d, want 1", got)
	}
}
