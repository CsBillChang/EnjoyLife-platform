package order

import (
	"context"
	"errors"
	"fmt"
	"time"

	"github.com/CsBillChang/local-life-platform/internal/ratelimit"
)

var ErrRateLimitUnavailable = errors.New("purchase rate limit unavailable")

type RateLimitExceeded struct {
	RetryAfter time.Duration
}

func (e *RateLimitExceeded) Error() string { return "purchase rate limit exceeded" }

type PurchaseRateLimiter interface {
	Allow(context.Context, ratelimit.Request) (ratelimit.Decision, error)
}

type PurchaseLimitConfig struct {
	Total ratelimit.Bucket
	User  ratelimit.Bucket
}

type RateLimitedAdmission struct {
	next   AsyncPurchaseBoundary
	limits PurchaseRateLimiter
	config PurchaseLimitConfig
}

func NewRateLimitedAdmission(next AsyncPurchaseBoundary, limits PurchaseRateLimiter, config PurchaseLimitConfig) (*RateLimitedAdmission, error) {
	if next == nil || limits == nil || config.Total.Capacity == 0 || config.Total.RefillPerSecond == 0 || config.User.Capacity == 0 || config.User.RefillPerSecond == 0 {
		return nil, errors.New("purchase admission and rate limit configuration are required")
	}
	return &RateLimitedAdmission{next: next, limits: limits, config: config}, nil
}

func (s *RateLimitedAdmission) Accept(ctx context.Context, userID, activityID uint64, key string, now time.Time) (PurchaseRequest, error) {
	decision, err := s.limits.Allow(ctx, ratelimit.Request{
		ActivityID: activityID,
		UserID:     userID,
		Total:      s.config.Total,
		User:       s.config.User,
		Cost:       1,
	})
	if err != nil {
		return PurchaseRequest{}, fmt.Errorf("%w: %v", ErrRateLimitUnavailable, err)
	}
	if !decision.Allowed {
		return PurchaseRequest{}, &RateLimitExceeded{RetryAfter: decision.RetryAfter}
	}
	return s.next.Accept(ctx, userID, activityID, key, now)
}

func (s *RateLimitedAdmission) GetOwnedRequest(ctx context.Context, userID uint64, requestID string) (PurchaseRequest, error) {
	return s.next.GetOwnedRequest(ctx, userID, requestID)
}
