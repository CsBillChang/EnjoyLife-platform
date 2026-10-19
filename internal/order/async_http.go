package order

import (
	"context"
	"errors"
	"net/http"
	"strconv"
	"strings"
	"time"

	"github.com/CsBillChang/local-life-platform/internal/auth"
	"github.com/gin-gonic/gin"
)

type AsyncPurchaseBoundary interface {
	Accept(context.Context, uint64, uint64, string, time.Time) (PurchaseRequest, error)
	GetOwnedRequest(context.Context, uint64, string) (PurchaseRequest, error)
}

type AsyncHTTPHandler struct {
	service AsyncPurchaseBoundary
	now     func() time.Time
}

func NewAsyncHTTPHandler(service AsyncPurchaseBoundary) *AsyncHTTPHandler {
	return &AsyncHTTPHandler{service: service, now: time.Now}
}

func (h *AsyncHTTPHandler) RegisterRoutes(router gin.IRouter) {
	router.POST("/purchase-requests", h.accept)
	router.GET("/purchase-requests/:id", h.get)
}

func (h *AsyncHTTPHandler) accept(c *gin.Context) {
	actor, ok := auth.CurrentActor(c)
	if !ok || actor.UserID == 0 {
		c.JSON(http.StatusUnauthorized, gin.H{"error": gin.H{"code": "UNAUTHORIZED", "message": "authentication required"}})
		return
	}
	key := c.GetHeader("Idempotency-Key")
	if strings.TrimSpace(key) == "" {
		c.JSON(http.StatusBadRequest, gin.H{"error": gin.H{"code": "INVALID_REQUEST", "message": "Idempotency-Key is required"}})
		return
	}
	var input struct {
		ActivityID uint64 `json:"activity_id" binding:"required"`
	}
	if err := c.ShouldBindJSON(&input); err != nil {
		c.JSON(http.StatusBadRequest, gin.H{"error": gin.H{"code": "INVALID_REQUEST", "message": "activity_id is required"}})
		return
	}
	item, err := h.service.Accept(c.Request.Context(), actor.UserID, input.ActivityID, key, h.now())
	if err != nil {
		h.writeError(c, err)
		return
	}
	c.JSON(http.StatusAccepted, gin.H{"data": item})
}

func (h *AsyncHTTPHandler) get(c *gin.Context) {
	actor, ok := auth.CurrentActor(c)
	if !ok || actor.UserID == 0 {
		c.JSON(http.StatusUnauthorized, gin.H{"error": gin.H{"code": "UNAUTHORIZED", "message": "authentication required"}})
		return
	}
	item, err := h.service.GetOwnedRequest(c.Request.Context(), actor.UserID, c.Param("id"))
	if err != nil {
		h.writeError(c, err)
		return
	}
	c.JSON(http.StatusOK, gin.H{"data": item})
}

func (h *AsyncHTTPHandler) writeError(c *gin.Context, err error) {
	var limited *RateLimitExceeded
	switch {
	case errors.As(err, &limited):
		retrySeconds := int64((limited.RetryAfter + time.Second - 1) / time.Second)
		if retrySeconds < 1 {
			retrySeconds = 1
		}
		c.Header("Retry-After", strconv.FormatInt(retrySeconds, 10))
		c.JSON(http.StatusTooManyRequests, gin.H{"error": gin.H{"code": "RATE_LIMITED", "message": "purchase request rate limit exceeded"}})
	case errors.Is(err, ErrInvalidPurchaseInput):
		c.JSON(http.StatusBadRequest, gin.H{"error": gin.H{"code": "INVALID_REQUEST", "message": "invalid purchase request"}})
	case errors.Is(err, ErrActivityNotFound), errors.Is(err, ErrRequestNotFound):
		c.JSON(http.StatusNotFound, gin.H{"error": gin.H{"code": "NOT_FOUND", "message": "requested resource was not found"}})
	case errors.Is(err, ErrAlreadyPurchased), errors.Is(err, ErrIdempotencyConflict), errors.Is(err, ErrRequestInProgress), errors.Is(err, ErrActivityUnavailable), errors.Is(err, ErrActivityNotStarted), errors.Is(err, ErrActivityEnded), errors.Is(err, ErrSoldOut):
		c.JSON(http.StatusConflict, gin.H{"error": gin.H{"code": "PURCHASE_CONFLICT", "message": "purchase cannot be accepted"}})
	default:
		c.JSON(http.StatusServiceUnavailable, gin.H{"error": gin.H{"code": "PURCHASE_UNAVAILABLE", "message": "purchase service unavailable"}})
	}
}
