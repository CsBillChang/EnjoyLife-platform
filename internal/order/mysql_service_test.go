package order

import (
	"context"
	"errors"
	"fmt"
	"regexp"
	"strings"
	"testing"
	"time"

	"github.com/go-sql-driver/mysql"
)

func TestPurchaseInputValidation(t *testing.T) {
	now := time.Date(2026, 9, 22, 10, 0, 0, 0, time.UTC)
	for _, key := range []string{"", " ", "ends-in-space ", "has space", "line\nbreak", "tab\t", "中文", "null\x00", "delete\x7f", strings.Repeat("x", 129)} {
		t.Run(fmt.Sprintf("key_%q", key), func(t *testing.T) {
			if err := validatePurchaseInput(1, 1, key, now); !errors.Is(err, ErrInvalidPurchaseInput) {
				t.Fatalf("accepted invalid key %q: %v", key, err)
			}
		})
	}
	for _, key := range []string{"a", "Case-Sensitive_KEY:123", strings.Repeat("x", 128)} {
		if err := validatePurchaseInput(1, 1, key, now); err != nil {
			t.Fatalf("rejected valid key %q: %v", key, err)
		}
	}
	for _, input := range []struct {
		user, activity uint64
		now            time.Time
	}{
		{0, 1, now}, {1, 0, now}, {1, 1, time.Time{}},
		{1, 1, time.Date(999, 1, 1, 0, 0, 0, 0, time.UTC)},
		{1, 1, time.Date(9999, 12, 31, 23, 59, 0, 0, time.UTC)},
		{1, 1, time.Date(1000, 1, 1, 0, 0, 0, 0, time.FixedZone("UTC+8", 8*60*60))},
	} {
		if err := validatePurchaseInput(input.user, input.activity, "key", input.now); !errors.Is(err, ErrInvalidPurchaseInput) {
			t.Fatalf("invalid input %+v: %v", input, err)
		}
	}
}

func TestServiceRejectsInvalidInputBeforeDatabase(t *testing.T) {
	service := NewMySQLService(nil)
	if _, err := service.Purchase(context.Background(), 0, 1, "key", time.Now()); !errors.Is(err, ErrInvalidPurchaseInput) {
		t.Fatalf("purchase invalid user: %v", err)
	}
	for _, ids := range [][2]uint64{{0, 1}, {1, 0}} {
		if _, err := service.GetOwnedOrder(context.Background(), ids[0], ids[1]); !errors.Is(err, ErrInvalidPurchaseInput) {
			t.Fatalf("owned order invalid IDs: %v", err)
		}
	}
}

func TestPurchaseActivityBoundaries(t *testing.T) {
	start := time.Date(2026, 9, 22, 10, 0, 0, 0, time.UTC)
	base := purchaseActivity{AdmissionState: "OPEN", StartsAt: start, EndsAt: start.Add(time.Hour), AvailableStock: 1}
	cases := []struct {
		name  string
		now   time.Time
		state string
		stock uint64
		want  error
	}{
		{"before_start", start.Add(-time.Microsecond), "OPEN", 1, ErrActivityNotStarted},
		{"at_start", start, "OPEN", 1, nil},
		{"before_end", base.EndsAt.Add(-time.Microsecond), "OPEN", 1, nil},
		{"at_end", base.EndsAt, "OPEN", 1, ErrActivityEnded},
		{"frozen", start, "FROZEN", 1, ErrActivityUnavailable},
		{"building", start, "BUILDING", 1, ErrActivityUnavailable},
		{"verified_not_open", start, "VERIFIED", 1, ErrActivityUnavailable},
		{"unknown_state", start, "", 1, ErrActivityUnavailable},
		{"sold_out", start, "OPEN", 0, ErrSoldOut},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			activity := base
			activity.AdmissionState, activity.AvailableStock = tc.state, tc.stock
			if err := validatePurchaseActivity(activity, tc.now); !errors.Is(err, tc.want) {
				t.Fatalf("got %v, want %v", err, tc.want)
			}
		})
	}
}

func TestRequestIDUUIDV4(t *testing.T) {
	pattern := regexp.MustCompile(`^[0-9a-f]{8}-[0-9a-f]{4}-4[0-9a-f]{3}-[89ab][0-9a-f]{3}-[0-9a-f]{12}$`)
	seen := make(map[string]bool)
	for range 1000 {
		id, err := newRequestID()
		if err != nil || !pattern.MatchString(id) {
			t.Fatalf("invalid UUID %q: %v", id, err)
		}
		if seen[id] {
			t.Fatalf("repeated request ID %s", id)
		}
		seen[id] = true
	}
}

func TestPurchaseDBErrorMapping(t *testing.T) {
	for _, tc := range []struct {
		number uint16
		want   error
	}{{1062, ErrStorageConflict}, {1205, ErrTransactionRetryable}, {1213, ErrTransactionRetryable}} {
		dbErr := &mysql.MySQLError{Number: tc.number, Message: "test error"}
		got := mapPurchaseDBError(fmt.Errorf("wrapped: %w", dbErr))
		if !errors.Is(got, tc.want) {
			t.Fatalf("code %d: %v", tc.number, got)
		}
		var original *mysql.MySQLError
		if !errors.As(got, &original) || original != dbErr {
			t.Fatalf("lost original database error: %v", got)
		}
	}
	for _, original := range []error{nil, context.Canceled, errors.New("connection failed"), &mysql.MySQLError{Number: 1452}} {
		if got := mapPurchaseDBError(original); got != original {
			t.Fatalf("unexpected remapping: %v -> %v", original, got)
		}
	}
}
