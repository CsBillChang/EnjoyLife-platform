package main

import (
	"context"
	"log/slog"
	"os"

	"github.com/CsBillChang/local-life-platform/internal/platform/config"
	"github.com/CsBillChang/local-life-platform/internal/platform/database"
	"github.com/CsBillChang/local-life-platform/internal/platform/migrate"
)

func main() {
	cfg, err := config.Load()
	if err != nil {
		slog.Error("load configuration", "error", err)
		os.Exit(1)
	}
	db, err := database.OpenMySQL(context.Background(), cfg.MySQLDSN)
	if err != nil {
		slog.Error("connect database", "error", err)
		os.Exit(1)
	}
	defer db.Close()
	if err := migrate.ApplyDirectory(context.Background(), db, "migrations"); err != nil {
		slog.Error("apply migrations", "error", err)
		os.Exit(1)
	}
	slog.Info("migrations applied")
}
