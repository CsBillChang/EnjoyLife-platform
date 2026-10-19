package shop

import (
	"strings"
	"testing"
	"time"
)

func TestInvalidationConfigBounds(t *testing.T) {
	got := (InvalidationConfig{}).withDefaults()
	if got.LeaseDuration <= 0 || got.BackoffBase <= 0 || got.BackoffMax < got.BackoffBase || got.MaxAttempts == 0 {
		t.Fatalf("invalid defaults: %+v", got)
	}
	got = (InvalidationConfig{BackoffBase: 5 * time.Second, BackoffMax: time.Second, MaxAttempts: 1001}).withDefaults()
	if got.BackoffMax != 5*time.Second || got.MaxAttempts != 20 {
		t.Fatalf("invalid bounded config: %+v", got)
	}
}

func TestInvalidationBackoffIsCapped(t *testing.T) {
	base, max := time.Second, 5*time.Second
	if got := invalidationBackoff(1, base, max); got != base {
		t.Fatalf("attempt one = %s", got)
	}
	if got := invalidationBackoff(3, base, max); got != 4*time.Second {
		t.Fatalf("attempt three = %s", got)
	}
	if got := invalidationBackoff(99, base, max); got != max {
		t.Fatalf("unbounded backoff = %s", got)
	}
}

func TestInvalidationLeaseToken(t *testing.T) {
	first, err := invalidationLeaseToken()
	if err != nil || len(first) != 32 {
		t.Fatalf("first token %q: %v", first, err)
	}
	second, err := invalidationLeaseToken()
	if err != nil || second == first || strings.Trim(second, "0123456789abcdef") != "" {
		t.Fatalf("second token %q: %v", second, err)
	}
}
