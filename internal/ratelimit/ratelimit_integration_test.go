package ratelimit

import (
	"context"
	"os"
	"strconv"
	"testing"
	"time"

	"github.com/CsBillChang/local-life-platform/internal/platform/config"
	"github.com/redis/go-redis/v9"
)

func TestRedisTwoLevelLimit(t *testing.T) {
	if os.Getenv("LOCAL_LIFE_INTEGRATION") != "1" {
		t.Skip("set LOCAL_LIFE_INTEGRATION=1 to run against project Redis")
	}
	cfg, err := config.Load()
	if err != nil {
		t.Fatal(err)
	}
	client := redis.NewClient(&redis.Options{Addr: cfg.RedisAddress})
	t.Cleanup(func() { _ = client.Close() })
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	if err := client.Ping(ctx).Err(); err != nil {
		t.Fatalf("local Redis unavailable at %s: %v", cfg.RedisAddress, err)
	}
	prefix := "local-life:test:ratelimit:" + strconv.FormatInt(time.Now().UnixNano(), 10)
	s, err := New(client, prefix)
	if err != nil {
		t.Fatal(err)
	}
	request := Request{ActivityID: 1, UserID: 2, Total: Bucket{Capacity: 2, RefillPerSecond: 1}, User: Bucket{Capacity: 1, RefillPerSecond: 1}, Cost: 1}
	t.Cleanup(func() { _ = client.Del(context.Background(), s.keys(request.ActivityID, request.UserID)...).Err() })
	if got, err := s.Allow(ctx, request); err != nil || !got.Allowed {
		t.Fatalf("first = %+v, %v", got, err)
	}
	if got, err := s.Allow(ctx, request); err != nil || got.Allowed || got.RetryAfter <= 0 {
		t.Fatalf("user limit = %+v, %v", got, err)
	}
	request.UserID = 3
	if got, err := s.Allow(ctx, request); err != nil || !got.Allowed {
		t.Fatalf("second user = %+v, %v", got, err)
	}
	request.UserID = 4
	if got, err := s.Allow(ctx, request); err != nil || got.Allowed || got.RetryAfter <= 0 {
		t.Fatalf("total limit = %+v, %v", got, err)
	}
	for _, userID := range []uint64{2, 3, 4} {
		_ = client.Del(context.Background(), s.keys(request.ActivityID, userID)...).Err()
	}
}
