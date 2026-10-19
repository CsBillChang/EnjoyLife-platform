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

// SyncPurchaseService is the synchronous purchase boundary used by HTTP handlers.
// MySQLService satisfies this interface; keeping it small makes handler behavior
// independently testable.
type SyncPurchaseService interface {
	Purchase(ctx context.Context, userID, activityID uint64, idempotencyKey string, now time.Time) (Order, error)
	GetOwnedOrder(ctx context.Context, userID, orderID uint64) (Order, error)
}

// HTTPHandler adapts the synchronous purchase service to authenticated HTTP routes.
type HTTPHandler struct {
	service SyncPurchaseService
	now     func() time.Time
}

// NewHTTPHandler creates a synchronous purchase HTTP handler.
func NewHTTPHandler(service SyncPurchaseService) *HTTPHandler {
	return &HTTPHandler{service: service, now: time.Now}
}

// RegisterRoutes installs routes inside an authentication-protected route group.
// It still checks for an actor so mounting it outside that group fails closed.
func (h *HTTPHandler) RegisterRoutes(router gin.IRouter) {
	router.POST("/sync/purchases", h.purchase)
	router.GET("/orders/:id", h.getOwnedOrder)
}

type purchaseRequest struct {
	ActivityID uint64 `json:"activity_id" binding:"required"`
}

func (h *HTTPHandler) purchase(c *gin.Context) {
	actor, ok := auth.CurrentActor(c)
	if !ok || actor.UserID == 0 {
		h.writeError(c, http.StatusUnauthorized, "UNAUTHORIZED", "authentication required")
		return
	}
	key := c.GetHeader("Idempotency-Key")
	if strings.TrimSpace(key) == "" {
		h.writeError(c, http.StatusBadRequest, "INVALID_REQUEST", "Idempotency-Key is required")
		return
	}

	var request purchaseRequest
	if err := c.ShouldBindJSON(&request); err != nil {
		h.writeError(c, http.StatusBadRequest, "INVALID_REQUEST", "activity_id must be a positive integer")
		return
	}

	item, err := h.service.Purchase(c.Request.Context(), actor.UserID, request.ActivityID, key, h.now())
	if err != nil {
		h.writeServiceError(c, err)
		return
	}
	c.JSON(http.StatusCreated, gin.H{"data": item})
}

func (h *HTTPHandler) getOwnedOrder(c *gin.Context) {
	actor, ok := auth.CurrentActor(c)
	if !ok || actor.UserID == 0 {
		h.writeError(c, http.StatusUnauthorized, "UNAUTHORIZED", "authentication required")
		return
	}
	orderID, err := strconv.ParseUint(c.Param("id"), 10, 64)
	if err != nil || orderID == 0 {
		h.writeError(c, http.StatusBadRequest, "INVALID_REQUEST", "order id must be a positive integer")
		return
	}

	item, err := h.service.GetOwnedOrder(c.Request.Context(), actor.UserID, orderID)
	if err != nil {
		h.writeServiceError(c, err)
		return
	}
	c.JSON(http.StatusOK, gin.H{"data": item})
}

func (h *HTTPHandler) writeServiceError(c *gin.Context, err error) {
	switch {
	case errors.Is(err, ErrInvalidPurchaseInput):
		h.writeError(c, http.StatusBadRequest, "INVALID_REQUEST", "invalid purchase request")
	case errors.Is(err, ErrActivityNotFound), errors.Is(err, ErrOrderNotFound):
		h.writeError(c, http.StatusNotFound, "NOT_FOUND", "requested resource was not found")
	case errors.Is(err, ErrAlreadyPurchased),
		errors.Is(err, ErrIdempotencyConflict),
		errors.Is(err, ErrStorageConflict),
		errors.Is(err, ErrActivityUnavailable),
		errors.Is(err, ErrActivityNotStarted),
		errors.Is(err, ErrActivityEnded),
		errors.Is(err, ErrSoldOut):
		h.writeError(c, http.StatusConflict, "PURCHASE_CONFLICT", "purchase cannot be completed in its current state")
	default:
		h.writeError(c, http.StatusServiceUnavailable, "PURCHASE_UNAVAILABLE", "purchase service unavailable")
	}
}

func (h *HTTPHandler) writeError(c *gin.Context, status int, code, message string) {
	c.JSON(status, gin.H{"error": gin.H{"code": code, "message": message}})
}
