package main

import (
	"context"
	"sync/atomic"
	"testing"
	"time"

	"github.com/leleasley/rex-alt/internal/sync"
)

func TestSyncDue(t *testing.T) {
	now := time.Date(2026, 11, 23, 14, 30, 0, 0, time.UTC)
	if !syncDue(now, -1) {
		t.Fatal("no gate should always be due")
	}
	if syncDue(now, 3) {
		t.Fatal("hour 14 is not hour 3")
	}
	if !syncDue(time.Date(2026, 11, 24, 3, 5, 0, 0, time.UTC), 3) {
		t.Fatal("hour 3 should be due")
	}
}

func TestRetryDelay(t *testing.T) {
	if got := retryDelay(6 * time.Hour); got != time.Hour {
		t.Errorf("coarse interval: got %s, want 1h", got)
	}
	if got := retryDelay(20 * time.Minute); got != 20*time.Minute {
		t.Errorf("fine interval: got %s, want 20m", got)
	}
	if got := retryDelay(0); got != time.Hour {
		t.Errorf("zero interval: got %s, want 1h", got)
	}
}

// fakeSyncService is a deterministic stand-in for the two calls the background
// loops make. Counters are atomic because the loops run in goroutines.
type fakeSyncService struct {
	changed func(context.Context) (bool, error)
	run     func(context.Context) (sync.Stats, error)
	changes atomic.Int64
	syncs   atomic.Int64
}

func (f *fakeSyncService) HasChanges(ctx context.Context) (bool, error) {
	f.changes.Add(1)
	return f.changed(ctx)
}

func (f *fakeSyncService) SyncOnce(ctx context.Context) (sync.Stats, error) {
	f.syncs.Add(1)
	return f.run(ctx)
}

func waitForCount(t *testing.T, ch <-chan struct{}, want int64, counter *atomic.Int64) {
	t.Helper()
	for counter.Load() < want {
		select {
		case <-ch:
		case <-time.After(time.Second):
			t.Fatalf("timed out waiting for %d calls, got %d", want, counter.Load())
		}
	}
}

func TestWatchLoopSyncsWhenCatalogueChanged(t *testing.T) {
	ticks := make(chan struct{}, 16)
	synced := make(chan struct{}, 16)
	f := &fakeSyncService{
		changed: func(context.Context) (bool, error) {
			ticks <- struct{}{}
			return true, nil
		},
		run: func(context.Context) (sync.Stats, error) {
			synced <- struct{}{}
			return sync.Stats{}, nil
		},
	}
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan struct{})
	var guard atomic.Bool
	go func() {
		runWatchLoop(ctx, f, 5*time.Millisecond, &guard)
		close(done)
	}()

	waitForCount(t, ticks, 1, &f.changes)
	waitForCount(t, synced, 1, &f.syncs)
	cancel()
	select {
	case <-done:
	case <-time.After(time.Second):
		t.Fatal("watch loop did not exit on cancellation")
	}
}

func TestWatchLoopDoesNotSyncUnchangedCatalogue(t *testing.T) {
	ticks := make(chan struct{}, 16)
	f := &fakeSyncService{
		changed: func(context.Context) (bool, error) {
			ticks <- struct{}{}
			return false, nil
		},
		run: func(context.Context) (sync.Stats, error) {
			t.Error("unchanged catalogue must not trigger a sync")
			return sync.Stats{}, nil
		},
	}
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan struct{})
	var guard atomic.Bool
	go func() {
		runWatchLoop(ctx, f, 5*time.Millisecond, &guard)
		close(done)
	}()

	waitForCount(t, ticks, 3, &f.changes)
	cancel()
	select {
	case <-done:
	case <-time.After(time.Second):
		t.Fatal("watch loop did not exit on cancellation")
	}
	if got := f.syncs.Load(); got != 0 {
		t.Errorf("got %d syncs for an unchanged catalogue, want 0", got)
	}
}

func TestTrySyncSkipsWhileGuardHeldAndReleasesIt(t *testing.T) {
	var guard atomic.Bool
	guard.Store(true)
	f := &fakeSyncService{run: func(context.Context) (sync.Stats, error) { return sync.Stats{}, nil }}

	if ran := trySync(context.Background(), &guard, f); ran {
		t.Error("trySync ran while the guard was held")
	}
	if got := f.syncs.Load(); got != 0 {
		t.Errorf("got %d syncs while the guard was held, want 0", got)
	}

	guard.Store(false)
	if ran := trySync(context.Background(), &guard, f); !ran {
		t.Error("trySync did not run once the guard was free")
	}
	if got := f.syncs.Load(); got != 1 {
		t.Errorf("got %d syncs after release, want 1", got)
	}
	if !trySync(context.Background(), &guard, f) {
		t.Error("trySync did not release the guard after running")
	}
	if got := f.syncs.Load(); got != 2 {
		t.Errorf("got %d syncs on the second free call, want 2", got)
	}
}

func TestWatchLoopRespectsSharedGuard(t *testing.T) {
	ticks := make(chan struct{}, 16)
	f := &fakeSyncService{
		changed: func(context.Context) (bool, error) {
			ticks <- struct{}{}
			return true, nil
		},
		run: func(context.Context) (sync.Stats, error) { return sync.Stats{}, nil },
	}
	var guard atomic.Bool
	guard.Store(true) // as if the scheduled loop is already syncing
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan struct{})
	go func() {
		runWatchLoop(ctx, f, 5*time.Millisecond, &guard)
		close(done)
	}()

	waitForCount(t, ticks, 3, &f.changes)
	cancel()
	select {
	case <-done:
	case <-time.After(time.Second):
		t.Fatal("watch loop did not exit on cancellation")
	}
	if got := f.syncs.Load(); got != 0 {
		t.Errorf("got %d syncs while another pass held the guard, want 0", got)
	}
}

func TestScheduledLoopRespectsSharedGuard(t *testing.T) {
	f := &fakeSyncService{run: func(context.Context) (sync.Stats, error) { return sync.Stats{}, nil }}
	var guard atomic.Bool
	guard.Store(true) // as if the watcher is already syncing
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan struct{})
	go func() {
		runSyncLoop(ctx, f, 5*time.Millisecond, -1, &guard)
		close(done)
	}()

	time.Sleep(30 * time.Millisecond)
	cancel()
	select {
	case <-done:
	case <-time.After(time.Second):
		t.Fatal("scheduled loop did not exit on cancellation")
	}
	if got := f.syncs.Load(); got != 0 {
		t.Errorf("got %d scheduled syncs while another pass held the guard, want 0", got)
	}
}
