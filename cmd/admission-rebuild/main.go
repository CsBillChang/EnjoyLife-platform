package main

import (
	"context"
	"encoding/json"
	"flag"
	"fmt"
	"log/slog"
	"os"
	"time"

	"github.com/CsBillChang/local-life-platform/internal/admission"
	"github.com/CsBillChang/local-life-platform/internal/platform/config"
	"github.com/CsBillChang/local-life-platform/internal/platform/database"
	"github.com/CsBillChang/local-life-platform/internal/recovery"
	"github.com/redis/go-redis/v9"
)

func main() {
	activityID := flag.Uint64("activity-id", 0, "start and complete a rebuild for this activity")
	taskID := flag.Uint64("task-id", 0, "resume this durable rebuild task")
	timeout := flag.Duration("timeout", 2*time.Minute, "overall command timeout")
	flag.Parse()
	if (*activityID == 0) == (*taskID == 0) || *timeout <= 0 {
		fmt.Fprintln(os.Stderr, "exactly one of --activity-id or --task-id is required; --timeout must be positive")
		os.Exit(2)
	}

	cfg, err := config.Load()
	if err != nil {
		fatal("load configuration", err)
	}
	ctx, cancel := context.WithTimeout(context.Background(), *timeout)
	defer cancel()
	db, err := database.OpenMySQL(ctx, cfg.MySQLDSN)
	if err != nil {
		fatal("connect MySQL", err)
	}
	defer db.Close()
	redisClient := redis.NewClient(&redis.Options{Addr: cfg.RedisAddress})
	defer redisClient.Close()
	if err := redisClient.Ping(ctx).Err(); err != nil {
		fatal("connect Redis", err)
	}
	store, err := admission.New(redisClient, "local-life:admission")
	if err != nil {
		fatal("create admission store", err)
	}
	service := recovery.NewService(db, store, nil)
	if *activityID != 0 {
		task, err := service.Start(ctx, *activityID)
		if err != nil {
			fatal("start rebuild", err)
		}
		printTask("started", task)
		// The started record is printed before work begins so an interrupted or
		// ambiguous run can always be retried with --task-id.
		completed, err := service.Resume(ctx, task.ID)
		if err != nil {
			fatal(fmt.Sprintf("resume rebuild task %d", task.ID), err)
		}
		printTask("completed", completed)
		return
	}
	completed, err := service.Resume(ctx, *taskID)
	if err != nil {
		fatal(fmt.Sprintf("resume rebuild task %d", *taskID), err)
	}
	printTask("completed", completed)
}

func printTask(event string, task recovery.Task) {
	if err := json.NewEncoder(os.Stdout).Encode(struct {
		Event string `json:"event"`
		recovery.Task
	}{Event: event, Task: task}); err != nil {
		fatal("encode task", err)
	}
}

func fatal(message string, err error) {
	slog.Error(message, "error", err)
	os.Exit(1)
}
