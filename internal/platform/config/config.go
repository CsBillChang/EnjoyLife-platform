package config

import (
	"fmt"
	"net/url"
	"os"
	"strconv"
	"time"
)

type Config struct {
	Address        string
	PprofAddress   string
	PprofMutexRate int
	PprofBlockRate int
	ShopCacheMode  string
	HTTPAccessLog  bool
	MySQLDSN       string
	RedisAddress   string
	RabbitURL      string
	WorkerCount    int
	Prefetch       int
	RateTotalCap   int
	RateTotalRPS   int
	RateUserCap    int
	RateUserRPS    int
	SessionTTL     time.Duration
	ShutdownPeriod time.Duration
}

func Load() (Config, error) {
	cfg := Config{
		Address:        envOr("APP_ADDR", "127.0.0.1:18080"),
		PprofAddress:   envOr("PPROF_ADDR", "127.0.0.1:18081"),
		ShopCacheMode:  envOr("SHOP_CACHE_MODE", "two-level"),
		HTTPAccessLog:  envOr("HTTP_ACCESS_LOG", "1") != "0",
		SessionTTL:     12 * time.Hour,
		ShutdownPeriod: 10 * time.Second,
	}
	switch cfg.ShopCacheMode {
	case "mysql", "l1", "two-level":
	default:
		return Config{}, fmt.Errorf("SHOP_CACHE_MODE must be mysql, l1, or two-level")
	}

	host := envOr("MYSQL_HOST", "127.0.0.1")
	port, err := positivePort(envOr("MYSQL_PORT", "13306"))
	if err != nil {
		return Config{}, fmt.Errorf("MYSQL_PORT: %w", err)
	}
	name := envOr("MYSQL_DATABASE", "huixiang_life")
	user := envOr("MYSQL_USER", "huixiang")
	password := envOr("MYSQL_PASSWORD", "local_dev_only")
	cfg.MySQLDSN = fmt.Sprintf("%s:%s@tcp(%s:%d)/%s?charset=utf8mb4&parseTime=true&loc=UTC&time_zone=%%27%%2B00%%3A00%%27&timeout=3s&readTimeout=3s&writeTimeout=3s", user, password, host, port, name)
	redisPort, err := positivePort(envOr("REDIS_PORT", "16379"))
	if err != nil {
		return Config{}, fmt.Errorf("REDIS_PORT: %w", err)
	}
	cfg.RedisAddress = fmt.Sprintf("%s:%d", envOr("REDIS_HOST", "127.0.0.1"), redisPort)
	rabbitPort, err := positivePort(envOr("RABBITMQ_AMQP_PORT", "15672"))
	if err != nil {
		return Config{}, fmt.Errorf("RABBITMQ_AMQP_PORT: %w", err)
	}
	rabbitUser := envOr("RABBITMQ_USER", "huixiang")
	rabbitPassword := envOr("RABBITMQ_PASSWORD", "local_dev_only")
	rabbitHost := envOr("RABBITMQ_HOST", "127.0.0.1")
	cfg.RabbitURL = (&url.URL{Scheme: "amqp", User: url.UserPassword(rabbitUser, rabbitPassword), Host: fmt.Sprintf("%s:%d", rabbitHost, rabbitPort), Path: "/"}).String()
	cfg.WorkerCount, err = positiveInt(envOr("WORKER_COUNT", "4"), "WORKER_COUNT")
	if err != nil {
		return Config{}, err
	}
	cfg.Prefetch, err = positiveInt(envOr("RABBITMQ_PREFETCH", "4"), "RABBITMQ_PREFETCH")
	if err != nil {
		return Config{}, err
	}
	cfg.RateTotalCap, err = positiveInt(envOr("RATE_LIMIT_TOTAL_CAPACITY", "200"), "RATE_LIMIT_TOTAL_CAPACITY")
	if err != nil {
		return Config{}, err
	}
	cfg.RateTotalRPS, err = positiveInt(envOr("RATE_LIMIT_TOTAL_REFILL_PER_SECOND", "100"), "RATE_LIMIT_TOTAL_REFILL_PER_SECOND")
	if err != nil {
		return Config{}, err
	}
	cfg.RateUserCap, err = positiveInt(envOr("RATE_LIMIT_USER_CAPACITY", "5"), "RATE_LIMIT_USER_CAPACITY")
	if err != nil {
		return Config{}, err
	}
	cfg.RateUserRPS, err = positiveInt(envOr("RATE_LIMIT_USER_REFILL_PER_SECOND", "1"), "RATE_LIMIT_USER_REFILL_PER_SECOND")
	if err != nil {
		return Config{}, err
	}
	cfg.PprofMutexRate, err = nonNegativeInt(envOr("PPROF_MUTEX_FRACTION", "0"), "PPROF_MUTEX_FRACTION")
	if err != nil {
		return Config{}, err
	}
	cfg.PprofBlockRate, err = nonNegativeInt(envOr("PPROF_BLOCK_RATE", "0"), "PPROF_BLOCK_RATE")
	if err != nil {
		return Config{}, err
	}
	return cfg, nil
}

func envOr(key, fallback string) string {
	if value, ok := os.LookupEnv(key); ok && value != "" {
		return value
	}
	return fallback
}

func positivePort(raw string) (int, error) {
	port, err := strconv.Atoi(raw)
	if err != nil || port < 1 || port > 65535 {
		return 0, fmt.Errorf("must be an integer between 1 and 65535")
	}
	return port, nil
}

func positiveInt(raw, name string) (int, error) {
	value, err := strconv.Atoi(raw)
	if err != nil || value < 1 || value > 1024 {
		return 0, fmt.Errorf("%s must be an integer between 1 and 1024", name)
	}
	return value, nil
}

func nonNegativeInt(raw, name string) (int, error) {
	value, err := strconv.Atoi(raw)
	if err != nil || value < 0 || value > 1_000_000_000 {
		return 0, fmt.Errorf("%s must be an integer between 0 and 1000000000", name)
	}
	return value, nil
}
