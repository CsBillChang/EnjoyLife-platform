package main

import (
	"context"
	"errors"
	"log/slog"
	"os"
	"strconv"
	"time"

	"github.com/CsBillChang/local-life-platform/internal/admission"
	"github.com/CsBillChang/local-life-platform/internal/platform/config"
	"github.com/CsBillChang/local-life-platform/internal/platform/database"
	"github.com/redis/go-redis/v9"
)

func main() {
	activityID, err := strconv.ParseUint(os.Getenv("ACTIVITY_ID"), 10, 64)
	if err != nil || activityID == 0 {
		slog.Error("ACTIVITY_ID must be a positive integer")
		os.Exit(2)
	}
	cfg, err := config.Load()
	if err != nil {
		slog.Error("load configuration", "error", err)
		os.Exit(1)
	}
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	db, err := database.OpenMySQL(ctx, cfg.MySQLDSN)
	if err != nil {
		slog.Error("connect database", "error", err)
		os.Exit(1)
	}
	defer db.Close()

	var generation, stock uint64
	var state string
	var startsAt, endsAt time.Time
	var orderCount, liveRequestCount uint64
	err = db.QueryRowContext(ctx, `SELECT generation, available_stock, admission_state, starts_at, ends_at,
		(SELECT COUNT(*) FROM orders WHERE activity_id = coupon_activities.id),
		(SELECT COUNT(*) FROM purchase_requests WHERE activity_id = coupon_activities.id
		 AND status IN ('RECEIVED', 'ADMITTED'))
		FROM coupon_activities WHERE id = ?`, activityID).
		Scan(&generation, &stock, &state, &startsAt, &endsAt, &orderCount, &liveRequestCount)
	if err != nil {
		slog.Error("read activity", "error", err)
		os.Exit(1)
	}
	// Initializing an empty generation is safe. Reconstructing a generation
	// with orders or live reservations requires the fenced L12 rebuild path,
	// because user ownership markers must be restored from MySQL facts.
	if orderCount != 0 || liveRequestCount != 0 {
		slog.Error("refuse unsafe initialization; use fenced rebuild", "activity_id", activityID,
			"orders", orderCount, "live_requests", liveRequestCount)
		os.Exit(3)
	}
	client := redis.NewClient(&redis.Options{Addr: cfg.RedisAddress})
	defer client.Close()
	if err := client.Ping(ctx).Err(); err != nil {
		slog.Error("connect redis", "error", err)
		os.Exit(1)
	}
	store, err := admission.New(client, "local-life:admission")
	if err != nil {
		slog.Error("create admission store", "error", err)
		os.Exit(1)
	}
	err = store.Initialize(ctx, admission.ActivityState{
		ActivityID: activityID,
		Generation: generation,
		StockHint:  stock,
		Open:       state == "OPEN",
		StartsAt:   startsAt,
		EndsAt:     endsAt,
	})
	if errors.Is(err, admission.ErrStateAlreadyExists) {
		slog.Info("admission generation already initialized; left unchanged", "activity_id", activityID, "generation", generation)
		return
	}
	if err != nil {
		slog.Error("initialize admission generation", "error", err)
		os.Exit(1)
	}
	slog.Info("admission generation initialized", "activity_id", activityID, "generation", generation, "stock_hint", stock)
}
