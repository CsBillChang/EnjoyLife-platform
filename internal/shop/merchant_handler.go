package shop

import (
	"context"
	"errors"
	"net/http"
	"strconv"
	"strings"

	"github.com/CsBillChang/local-life-platform/internal/auth"
	"github.com/gin-gonic/gin"
)

type OwnedUpdater interface {
	UpdateOwned(ctx context.Context, merchantID uint64, shopID int64, name, address, status string) (Shop, error)
}

type MerchantHandler struct {
	repository  OwnedUpdater
	invalidator interface {
		Invalidate(context.Context, int64) error
	}
}

func NewMerchantHandler(repository OwnedUpdater, invalidators ...interface {
	Invalidate(context.Context, int64) error
}) *MerchantHandler {
	handler := &MerchantHandler{repository: repository}
	if len(invalidators) > 0 {
		handler.invalidator = invalidators[0]
	}
	return handler
}

type updateShopRequest struct {
	Name    string `json:"name" binding:"required"`
	Address string `json:"address" binding:"required"`
	Status  string `json:"status" binding:"required"`
}

func (h *MerchantHandler) Update(c *gin.Context) {
	actor, ok := auth.CurrentActor(c)
	if !ok {
		c.JSON(http.StatusUnauthorized, gin.H{"error": gin.H{"code": "UNAUTHORIZED", "message": "authentication required"}})
		return
	}
	if actor.Role != auth.RoleMerchant && actor.Role != auth.RoleAdmin {
		c.JSON(http.StatusForbidden, gin.H{"error": gin.H{"code": "FORBIDDEN", "message": "merchant role required"}})
		return
	}
	shopID, err := strconv.ParseInt(c.Param("id"), 10, 64)
	if err != nil || shopID <= 0 {
		c.JSON(http.StatusBadRequest, gin.H{"error": gin.H{"code": "INVALID_REQUEST", "message": ErrInvalidShopID.Error()}})
		return
	}
	var request updateShopRequest
	if err := c.ShouldBindJSON(&request); err != nil {
		c.JSON(http.StatusBadRequest, gin.H{"error": gin.H{"code": "INVALID_REQUEST", "message": ErrInvalidShopUpdate.Error()}})
		return
	}
	request.Name = strings.TrimSpace(request.Name)
	request.Address = strings.TrimSpace(request.Address)
	request.Status = strings.ToUpper(strings.TrimSpace(request.Status))
	if request.Name == "" || request.Address == "" || (request.Status != "OPEN" && request.Status != "CLOSED") {
		c.JSON(http.StatusBadRequest, gin.H{"error": gin.H{"code": "INVALID_REQUEST", "message": ErrInvalidShopUpdate.Error()}})
		return
	}
	item, err := h.repository.UpdateOwned(c.Request.Context(), actor.UserID, shopID, request.Name, request.Address, request.Status)
	if err != nil {
		if errors.Is(err, ErrShopNotFound) {
			c.JSON(http.StatusNotFound, gin.H{"error": gin.H{"code": "SHOP_NOT_FOUND", "message": "shop not found"}})
			return
		}
		c.JSON(http.StatusServiceUnavailable, gin.H{"error": gin.H{"code": "SHOP_UPDATE_UNAVAILABLE", "message": "shop update unavailable"}})
		return
	}
	if h.invalidator != nil {
		// This best-effort fast path reduces the stale window. UpdateOwned also
		// persists a fenced recovery task in the same MySQL transaction, so a
		// process crash or Redis failure is repaired by the dispatcher later.
		_ = h.invalidator.Invalidate(c.Request.Context(), shopID)
	}
	c.JSON(http.StatusOK, gin.H{"data": item})
}
