package main

import (
	"context"
	"errors"
	"strings"
	"testing"
	"time"
)

func TestEnsurePingSysctlWithRetry_AllowsEmptyNamespace(t *testing.T) {
	t.Parallel()

	calls := 0
	gotNamespace := "unexpected"
	err := ensurePingSysctlWithRetry(
		context.Background(),
		"",
		10*time.Millisecond,
		1*time.Millisecond,
		func(_ context.Context, namespace string) error {
			calls++
			gotNamespace = namespace
			return nil
		},
	)
	if err != nil {
		t.Fatalf("ensurePingSysctlWithRetry() error = %v", err)
	}
	if calls != 1 {
		t.Fatalf("ensure should be called once, got %d", calls)
	}
	if gotNamespace != "" {
		t.Fatalf("namespace = %q, want empty namespace", gotNamespace)
	}
}

func TestEnsurePingSysctlWithRetry_RetriesUntilSuccess(t *testing.T) {
	t.Parallel()

	calls := 0
	err := ensurePingSysctlWithRetry(
		context.Background(),
		"tenant-blue",
		50*time.Millisecond,
		1*time.Millisecond,
		func(_ context.Context, _ string) error {
			calls++
			if calls < 3 {
				return errors.New("temporary failure")
			}
			return nil
		},
	)
	if err != nil {
		t.Fatalf("ensurePingSysctlWithRetry() error = %v", err)
	}
	if calls != 3 {
		t.Fatalf("ensure should be called 3 times, got %d", calls)
	}
}

func TestEnsurePingSysctlWithRetry_FailsAfterTimeout(t *testing.T) {
	t.Parallel()

	calls := 0
	err := ensurePingSysctlWithRetry(
		context.Background(),
		"tenant-blue",
		5*time.Millisecond,
		1*time.Millisecond,
		func(_ context.Context, _ string) error {
			calls++
			return errors.New("still failing")
		},
	)
	if err == nil {
		t.Fatalf("ensurePingSysctlWithRetry() expected error, got nil")
	}
	if calls < 2 {
		t.Fatalf("ensure should be retried, calls = %d", calls)
	}
	if !strings.Contains(err.Error(), "within") {
		t.Fatalf("error = %q, expected timeout detail", err.Error())
	}
}

func TestEnsurePingSysctlWithRetry_StopsOnContextCancel(t *testing.T) {
	t.Parallel()

	ctx, cancel := context.WithCancel(context.Background())
	calls := 0
	err := ensurePingSysctlWithRetry(
		ctx,
		"tenant-blue",
		100*time.Millisecond,
		10*time.Millisecond,
		func(_ context.Context, _ string) error {
			calls++
			if calls == 1 {
				cancel()
			}
			return errors.New("temporary failure")
		},
	)
	if err == nil {
		t.Fatalf("ensurePingSysctlWithRetry() expected error, got nil")
	}
	if !errors.Is(err, context.Canceled) {
		t.Fatalf("error = %v, expected context cancellation", err)
	}
	if calls != 1 {
		t.Fatalf("expected one call before cancellation, got %d", calls)
	}
}
