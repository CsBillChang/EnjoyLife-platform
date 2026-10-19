package order

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/CsBillChang/local-life-platform/internal/ratelimit"
)

type rateLimitFake struct {
	decision ratelimit.Decision
	err      error
	calls    int
}

func (f *rateLimitFake) Allow(context.Context, ratelimit.Request) (ratelimit.Decision, error) {
	f.calls++
	return f.decision, f.err
}

type asyncBoundaryFake struct {
	item  PurchaseRequest
	calls int
}

func (f *asyncBoundaryFake) Accept(context.Context, uint64, uint64, string, time.Time) (PurchaseRequest, error) {
	f.calls++
	return f.item, nil
}

func (f *asyncBoundaryFake) GetOwnedRequest(context.Context, uint64, string) (PurchaseRequest, error) {
	return f.item, nil
}

func TestRateLimitedAdmissionFailsClosedBeforePersistence(t *testing.T) {
	config := PurchaseLimitConfig{Total: ratelimit.Bucket{Capacity: 10, RefillPerSecond: 10}, User: ratelimit.Bucket{Capacity: 2, RefillPerSecond: 1}}
	for _, tc := range []struct {
		name     string
		decision ratelimit.Decision
		limitErr error
		wantErr  error
		wantNext int
	}{
		{name: "allowed", decision: ratelimit.Decision{Allowed: true}, wantNext: 1},
		{name: "denied", decision: ratelimit.Decision{RetryAfter: 500 * time.Millisecond}, wantErr: &RateLimitExceeded{}},
		{name: "redis unavailable", limitErr: errors.New("redis offline"), wantErr: ErrRateLimitUnavailable},
	} {
		t.Run(tc.name, func(t *testing.T) {
			limits := &rateLimitFake{decision: tc.decision, err: tc.limitErr}
			next := &asyncBoundaryFake{item: PurchaseRequest{RequestID: "request"}}
			service, err := NewRateLimitedAdmission(next, limits, config)
			if err != nil {
				t.Fatal(err)
			}
			_, err = service.Accept(context.Background(), 1, 2, "key", time.Now())
			if tc.wantErr == nil && err != nil {
				t.Fatal(err)
			}
			if tc.wantErr != nil {
				var exceeded *RateLimitExceeded
				switch tc.wantErr.(type) {
				case *RateLimitExceeded:
					if !errors.As(err, &exceeded) || exceeded.RetryAfter != 500*time.Millisecond {
						t.Fatalf("rate limit error=%v", err)
					}
				default:
					if !errors.Is(err, tc.wantErr) {
						t.Fatalf("error=%v want=%v", err, tc.wantErr)
					}
				}
			}
			if next.calls != tc.wantNext || limits.calls != 1 {
				t.Fatalf("limit calls=%d next calls=%d", limits.calls, next.calls)
			}
		})
	}
}
