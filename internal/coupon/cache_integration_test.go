package coupon

import (
	"context"
	"fmt"
	"os"
	"testing"
	"time"

	"github.com/CsBillChang/local-life-platform/internal/platform/config"
	"github.com/redis/go-redis/v9"
)

func TestRedisListGenerationAdvanceIsMonotonicIntegration(t *testing.T) {
	if os.Getenv("LOCAL_LIFE_INTEGRATION") != "1" {
		t.Skip("set LOCAL_LIFE_INTEGRATION=1 for Redis coupon cache integration")
	}
	cfg, err := config.Load()
	if err != nil {
		t.Fatal(err)
	}
	client := redis.NewClient(&redis.Options{Addr: cfg.RedisAddress})
	defer client.Close()
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	if err := client.Ping(ctx).Err(); err != nil {
		t.Fatal(err)
	}
	key := fmt.Sprintf("coupon:test:generation:%d", time.Now().UnixNano())
	defer client.Del(context.Background(), key)
	store := NewRedisStore(client)
	if err := store.Advance(ctx, key, 20, time.Minute); err != nil {
		t.Fatal(err)
	}
	if err := store.Advance(ctx, key, 10, time.Minute); err != nil {
		t.Fatal(err)
	}
	if got, err := client.Get(ctx, key).Result(); err != nil || got != "20" {
		t.Fatalf("generation regressed: got=%q err=%v", got, err)
	}
	if err := client.Set(ctx, key, "corrupt", time.Minute).Err(); err != nil {
		t.Fatal(err)
	}
	if err := store.Advance(ctx, key, 30, time.Minute); err != nil {
		t.Fatal(err)
	}
	if got, err := client.Get(ctx, key).Result(); err != nil || got != "30" {
		t.Fatalf("corrupt generation was not repaired: got=%q err=%v", got, err)
	}
}
