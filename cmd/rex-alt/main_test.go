package main

import (
	"testing"
	"time"
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
