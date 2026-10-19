package mq

import (
	"errors"
	"regexp"
	"strings"
	"testing"
	"time"
	"unicode/utf8"
)

func TestBackoffForAttemptIsCapped(t *testing.T) {
	base, maximum := time.Second, 5*time.Second
	cases := []struct {
		attempt uint32
		want    time.Duration
	}{
		{0, base}, {1, base}, {2, 2 * time.Second}, {3, 4 * time.Second}, {4, maximum}, {100, maximum},
	}
	for _, test := range cases {
		if got := BackoffForAttempt(test.attempt, base, maximum); got != test.want {
			t.Fatalf("attempt %d: got %s, want %s", test.attempt, got, test.want)
		}
	}
}

func TestOutboxLimitsAvoidLeasedBatchBacklogAndAttemptOverflow(t *testing.T) {
	for _, batch := range []int{0, 1, 32, 1000} {
		config := (OutboxConfig{MaxBatchSize: batch}).withDefaults()
		if config.MaxBatchSize != 1 || config.MaxAttempts != 20 {
			t.Fatalf("unsafe defaults: %+v", config)
		}
	}
	if config := (OutboxConfig{MaxAttempts: 5000}).withDefaults(); config.MaxAttempts != 1000 {
		t.Fatalf("attempts exceed SQL constraint: %d", config.MaxAttempts)
	}
	if config := (OutboxConfig{MaxAttempts: 2}).withDefaults(); config.MaxAttempts != 2 {
		t.Fatalf("explicit attempt budget ignored: %d", config.MaxAttempts)
	}
}

func TestLeaseOwnerIsIndependentUUID(t *testing.T) {
	pattern := regexp.MustCompile(`^[0-9a-f]{8}-[0-9a-f]{4}-4[0-9a-f]{3}-[89ab][0-9a-f]{3}-[0-9a-f]{12}$`)
	seen := make(map[string]bool)
	for range 100 {
		owner, err := newLeaseOwner()
		if err != nil || !pattern.MatchString(owner) || seen[owner] {
			t.Fatalf("invalid/repeated lease owner: %v", err)
		}
		seen[owner] = true
	}
}

func TestTruncateErrorKeepsValidUTF8AndBound(t *testing.T) {
	value := errors.New(strings.Repeat("界", 100))
	result := truncateError(value, 17)
	if len(result) > 17 || !utf8.ValidString(result) {
		t.Fatalf("truncated error is unsafe: %q (%d bytes)", result, len(result))
	}
}
