package main

import (
	"context"
	"log/slog"
	"net/http"
	_ "net/http/pprof"
	"os"
	"os/signal"
	"runtime"
	"sync"
	"syscall"
	"time"

	"github.com/CsBillChang/local-life-platform/internal/admission"
	"github.com/CsBillChang/local-life-platform/internal/auth"
	"github.com/CsBillChang/local-life-platform/internal/coupon"
	"github.com/CsBillChang/local-life-platform/internal/order"
	"github.com/CsBillChang/local-life-platform/internal/payment"
	"github.com/CsBillChang/local-life-platform/internal/platform/config"
	"github.com/CsBillChang/local-life-platform/internal/platform/database"
	"github.com/CsBillChang/local-life-platform/internal/ratelimit"
	"github.com/CsBillChang/local-life-platform/internal/shop"
	"github.com/gin-gonic/gin"
	"github.com/redis/go-redis/v9"
)

func main() {
	cfg, err := config.Load()
	if err != nil {
		slog.Error("load configuration", "error", err)
		os.Exit(1)
	}
	// Contention profiling is intentionally opt-in. A zero value keeps the
	// runtime defaults and avoids continuous diagnostic overhead in normal runs.
	runtime.SetMutexProfileFraction(cfg.PprofMutexRate)
	runtime.SetBlockProfileRate(cfg.PprofBlockRate)

	rootCtx, stop := signal.NotifyContext(context.Background(), syscall.SIGINT, syscall.SIGTERM)
	defer stop()
	gormDB, db, err := database.OpenGORM(rootCtx, cfg.MySQLDSN)
	if err != nil {
		slog.Error("connect database", "error", err)
		os.Exit(1)
	}
	defer db.Close()

	authService := auth.NewService(auth.NewMySQLRepository(db), cfg.SessionTTL)
	authHandler := auth.NewHandler(authService)
	shopRepository := shop.NewMySQLRepository(gormDB)
	redisClient := redis.NewClient(&redis.Options{Addr: cfg.RedisAddress})
	defer redisClient.Close()
	redisAvailable := false
	redisCtx, redisCancel := context.WithTimeout(rootCtx, time.Second)
	if err := redisClient.Ping(redisCtx).Err(); err != nil {
		slog.Warn("redis unavailable; shop reads degrade according to cache mode and purchase admission fails closed", "error", err)
	} else {
		redisAvailable = true
	}
	redisCancel()
	var shopReads shop.Repository = shopRepository
	var cachedShops *shop.CachedRepository
	var backgroundWorkers sync.WaitGroup
	if cfg.ShopCacheMode != "mysql" {
		var sharedCache shop.CacheStore
		if cfg.ShopCacheMode == "two-level" && redisAvailable {
			sharedCache = shop.NewRedisStore(redisClient)
		}
		cachedShops, err = shop.NewCachedRepository(shopRepository, sharedCache, shop.CacheConfig{})
		if err != nil {
			slog.Error("create shop cache", "error", err)
			os.Exit(1)
		}
		defer cachedShops.Close()
		shopReads = cachedShops
	}
	shopHandler := shop.NewHandler(shop.NewService(shopReads))
	merchantHandler := shop.NewMerchantHandler(shopRepository)
	if cachedShops != nil {
		merchantHandler = shop.NewMerchantHandler(shopRepository, cachedShops)
		invalidationDispatcher, dispatcherErr := shop.NewInvalidationDispatcher(gormDB, cachedShops, shop.InvalidationConfig{})
		if dispatcherErr != nil {
			slog.Error("create shop cache invalidation dispatcher", "error", dispatcherErr)
			os.Exit(1)
		}
		backgroundWorkers.Add(1)
		go func() {
			defer backgroundWorkers.Done()
			runShopInvalidationDispatcher(rootCtx, invalidationDispatcher)
		}()
	}
	couponRepository := coupon.NewMySQLRepository(db)
	var couponStore coupon.CacheStore
	if cfg.ShopCacheMode == "two-level" && redisAvailable {
		couponStore = coupon.NewRedisStore(redisClient)
	}
	cachedCoupons, err := coupon.NewCachedRepository(couponRepository, couponStore, coupon.CacheConfig{})
	if err != nil {
		slog.Error("create coupon cache", "error", err)
		os.Exit(1)
	}
	defer cachedCoupons.Close()
	var couponReads coupon.Repository = couponRepository
	if cfg.ShopCacheMode != "mysql" {
		couponReads = cachedCoupons
	}
	couponHandler := coupon.NewPublicHandler(couponReads)
	couponMerchantHandler := coupon.NewMerchantHandler(couponRepository, cachedCoupons)
	couponInvalidationDispatcher, err := coupon.NewInvalidationDispatcher(db, cachedCoupons, coupon.InvalidationConfig{})
	if err != nil {
		slog.Error("create coupon cache invalidation dispatcher", "error", err)
		os.Exit(1)
	}
	backgroundWorkers.Add(1)
	go func() {
		defer backgroundWorkers.Done()
		runCouponInvalidationDispatcher(rootCtx, couponInvalidationDispatcher)
	}()
	orderService := order.NewMySQLService(db)
	asyncService := order.NewAsyncService(db)
	reservationStore, err := admission.New(redisClient, "local-life:admission")
	if err != nil {
		slog.Error("create Redis admission store", "error", err)
		os.Exit(1)
	}
	admissionCoordinator := order.NewAdmissionCoordinator(asyncService, reservationStore)
	rateLimitStore, err := ratelimit.New(redisClient, "local-life:ratelimit")
	if err != nil {
		slog.Error("create Redis rate limiter", "error", err)
		os.Exit(1)
	}
	limitedAdmission, err := order.NewRateLimitedAdmission(admissionCoordinator, rateLimitStore, order.PurchaseLimitConfig{
		Total: ratelimit.Bucket{Capacity: uint64(cfg.RateTotalCap), RefillPerSecond: uint64(cfg.RateTotalRPS)},
		User:  ratelimit.Bucket{Capacity: uint64(cfg.RateUserCap), RefillPerSecond: uint64(cfg.RateUserRPS)},
	})
	if err != nil {
		slog.Error("create rate-limited admission", "error", err)
		os.Exit(1)
	}
	fulfillmentService := order.NewFulfillmentService(db)
	orderHandler := order.NewHTTPHandler(orderService)
	asyncHandler := order.NewAsyncHTTPHandler(limitedAdmission)
	paymentHandler := payment.NewHTTPHandler(orderService, fulfillmentService, payment.NewSimulator())
	recordHandler := order.NewRecordHTTPHandler(order.NewRecordQueryService(db))

	router := gin.New()
	if cfg.HTTPAccessLog {
		router.Use(gin.Logger())
	}
	router.Use(gin.Recovery())
	router.GET("/health", func(c *gin.Context) {
		ctx, cancel := context.WithTimeout(c.Request.Context(), time.Second)
		defer cancel()
		if err := db.PingContext(ctx); err != nil {
			c.JSON(http.StatusServiceUnavailable, gin.H{"status": "unhealthy"})
			return
		}
		c.JSON(http.StatusOK, gin.H{"status": "ok"})
	})
	router.GET("/debug/cache-metrics", func(c *gin.Context) {
		metrics := shop.CacheMetrics{}
		if cachedShops != nil {
			metrics = cachedShops.Metrics()
		}
		c.JSON(http.StatusOK, gin.H{"mode": cfg.ShopCacheMode, "data": metrics, "coupon_data": cachedCoupons.Metrics()})
	})
	shopHandler.RegisterRoutes(router)
	couponHandler.RegisterPublicRoutes(router)
	router.POST("/auth/login", authHandler.Login)
	authorized := router.Group("/")
	authorized.Use(authHandler.Middleware())
	authorized.GET("/me", func(c *gin.Context) {
		actor, _ := auth.CurrentActor(c)
		c.JSON(http.StatusOK, gin.H{"data": actor})
	})
	authorized.PATCH("/merchant/shops/:id", merchantHandler.Update)
	couponMerchantHandler.RegisterMerchantRoutes(authorized)
	orderHandler.RegisterRoutes(authorized)
	asyncHandler.RegisterRoutes(authorized)
	paymentHandler.RegisterRoutes(authorized)
	recordHandler.RegisterRoutes(authorized)

	server := &http.Server{
		Addr:              cfg.Address,
		Handler:           router,
		ReadHeaderTimeout: 5 * time.Second,
		ReadTimeout:       10 * time.Second,
		WriteTimeout:      10 * time.Second,
		IdleTimeout:       60 * time.Second,
	}
	pprofServer := &http.Server{
		Addr:              cfg.PprofAddress,
		Handler:           http.DefaultServeMux,
		ReadHeaderTimeout: 5 * time.Second,
	}

	go func() {
		slog.Info("api listening", "address", cfg.Address)
		if err := server.ListenAndServe(); err != nil && err != http.ErrServerClosed {
			slog.Error("api stopped unexpectedly", "error", err)
			stop()
		}
	}()
	go func() {
		slog.Info("pprof listening", "address", cfg.PprofAddress, "mutex_fraction", cfg.PprofMutexRate, "block_rate", cfg.PprofBlockRate)
		if err := pprofServer.ListenAndServe(); err != nil && err != http.ErrServerClosed {
			slog.Error("pprof stopped unexpectedly", "error", err)
			stop()
		}
	}()
	<-rootCtx.Done()
	backgroundWorkers.Wait()

	shutdownCtx, cancel := context.WithTimeout(context.Background(), cfg.ShutdownPeriod)
	defer cancel()
	if err := server.Shutdown(shutdownCtx); err != nil {
		slog.Error("api shutdown", "error", err)
	}
	if err := pprofServer.Shutdown(shutdownCtx); err != nil {
		slog.Error("pprof shutdown", "error", err)
	}
}

func runShopInvalidationDispatcher(ctx context.Context, dispatcher *shop.InvalidationDispatcher) {
	ticker := time.NewTicker(250 * time.Millisecond)
	defer ticker.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case now := <-ticker.C:
			for i := 0; i < 32; i++ {
				worked, err := dispatcher.DispatchOne(ctx, now)
				if err != nil {
					if ctx.Err() == nil {
						slog.Warn("dispatch shop cache invalidation", "error", err)
					}
					break
				}
				if !worked {
					break
				}
			}
		}
	}
}

func runCouponInvalidationDispatcher(ctx context.Context, dispatcher *coupon.InvalidationDispatcher) {
	ticker := time.NewTicker(250 * time.Millisecond)
	defer ticker.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case now := <-ticker.C:
			for i := 0; i < 32; i++ {
				worked, err := dispatcher.DispatchOne(ctx, now)
				if err != nil {
					if ctx.Err() == nil {
						slog.Warn("dispatch coupon cache invalidation", "error", err)
					}
					break
				}
				if !worked {
					break
				}
			}
		}
	}
}
