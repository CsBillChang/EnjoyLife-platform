package coupon

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"strconv"
	"strings"
	"time"

	"github.com/CsBillChang/local-life-platform/internal/auth"
	"github.com/gin-gonic/gin"
)

type PublicHandler struct{ repository Repository }

func NewPublicHandler(repository Repository) *PublicHandler {
	return &PublicHandler{repository: repository}
}

// RegisterPublicRoutes is intentionally separate from API composition. The
// caller chooses where public routes are mounted.
func (h *PublicHandler) RegisterPublicRoutes(router gin.IRouter) {
	// Gin requires one wildcard name for the same path segment. The shop detail
	// route already uses :id, so this child route must use the same name.
	router.GET("/shops/:id/activities", h.list)
	router.GET("/activities/:id", h.detail)
}
func (h *PublicHandler) list(c *gin.Context) {
	shopID, err := positiveID(c.Param("id"))
	if err != nil {
		h.writeError(c, http.StatusBadRequest, "INVALID_REQUEST", err)
		return
	}
	page, err := positiveInt(c.Query("page"), 1)
	if err != nil {
		h.writeError(c, http.StatusBadRequest, "INVALID_REQUEST", err)
		return
	}
	size, err := positiveInt(c.Query("page_size"), 20)
	if err != nil || size > 100 {
		h.writeError(c, http.StatusBadRequest, "INVALID_REQUEST", ErrInvalidPagination)
		return
	}
	maxInt := int(^uint(0) >> 1)
	if page-1 > maxInt/size {
		h.writeError(c, http.StatusBadRequest, "INVALID_REQUEST", ErrInvalidPagination)
		return
	}
	items, total, err := h.repository.ListByShop(c.Request.Context(), shopID, (page-1)*size, size)
	if err != nil {
		h.writeRepositoryError(c, err)
		return
	}
	data := make([]DisplayActivity, 0, len(items))
	for _, item := range items {
		data = append(data, item.Display())
	}
	c.JSON(http.StatusOK, gin.H{"data": data, "page": page, "page_size": size, "total": total, "available_stock_note": "available_stock_hint is display-only; purchase is confirmed by the order transaction"})
}
func (h *PublicHandler) detail(c *gin.Context) {
	id, err := positiveID(c.Param("id"))
	if err != nil {
		h.writeError(c, http.StatusBadRequest, "INVALID_REQUEST", err)
		return
	}
	item, err := h.repository.FindByID(c.Request.Context(), id)
	if err != nil {
		h.writeRepositoryError(c, err)
		return
	}
	c.JSON(http.StatusOK, gin.H{"data": item.Display(), "available_stock_note": "available_stock_hint is display-only; purchase is confirmed by the order transaction"})
}
func (h *PublicHandler) writeRepositoryError(c *gin.Context, err error) {
	if errors.Is(err, ErrActivityNotFound) {
		h.writeError(c, http.StatusNotFound, "ACTIVITY_NOT_FOUND", err)
		return
	}
	if errors.Is(err, ErrInvalidActivityID) || errors.Is(err, ErrInvalidShopID) || errors.Is(err, ErrInvalidPagination) {
		h.writeError(c, http.StatusBadRequest, "INVALID_REQUEST", err)
		return
	}
	h.writeError(c, http.StatusServiceUnavailable, "ACTIVITY_READ_UNAVAILABLE", errors.New("activity read unavailable"))
}
func (h *PublicHandler) writeError(c *gin.Context, status int, code string, err error) {
	c.JSON(status, gin.H{"error": gin.H{"code": code, "message": err.Error()}})
}

type OwnedUpdater interface {
	UpdateOwned(context.Context, uint64, uint64, UpdateInput) (Activity, error)
}
type MerchantHandler struct {
	updater     OwnedUpdater
	invalidator Invalidator
}

func (h *MerchantHandler) writeError(c *gin.Context, status int, code string, err error) {
	c.JSON(status, gin.H{"error": gin.H{"code": code, "message": err.Error()}})
}

func NewMerchantHandler(updater OwnedUpdater, invalidator Invalidator) *MerchantHandler {
	return &MerchantHandler{updater, invalidator}
}
func (h *MerchantHandler) RegisterMerchantRoutes(router gin.IRouter) {
	router.PATCH("/merchant/activities/:id", h.update)
}

type updateRequest struct {
	Title             *string    `json:"title"`
	PriceCents        *uint64    `json:"price_cents"`
	StartsAt          *time.Time `json:"starts_at"`
	EndsAt            *time.Time `json:"ends_at"`
	VoucherValidUntil *time.Time `json:"voucher_valid_until"`
}

func (h *MerchantHandler) update(c *gin.Context) {
	actor, ok := auth.CurrentActor(c)
	if !ok || actor.UserID == 0 {
		h.writeError(c, http.StatusUnauthorized, "UNAUTHORIZED", errors.New("authentication required"))
		return
	}
	if actor.Role != auth.RoleMerchant {
		h.writeError(c, http.StatusForbidden, "FORBIDDEN", errors.New("merchant role required"))
		return
	}
	id, err := positiveID(c.Param("id"))
	if err != nil {
		h.writeError(c, http.StatusBadRequest, "INVALID_REQUEST", err)
		return
	}
	var body updateRequest
	decoder := json.NewDecoder(c.Request.Body)
	decoder.DisallowUnknownFields()
	if err = decoder.Decode(&body); err != nil {
		h.writeError(c, http.StatusBadRequest, "INVALID_REQUEST", ErrInvalidUpdate)
		return
	}
	var trailing any
	if err = decoder.Decode(&trailing); err != io.EOF {
		h.writeError(c, http.StatusBadRequest, "INVALID_REQUEST", ErrInvalidUpdate)
		return
	}
	input := UpdateInput{body.Title, body.PriceCents, body.StartsAt, body.EndsAt, body.VoucherValidUntil}
	if !validUpdateInput(input) {
		h.writeError(c, http.StatusBadRequest, "INVALID_REQUEST", ErrInvalidUpdate)
		return
	}
	item, err := h.updater.UpdateOwned(c.Request.Context(), actor.UserID, id, input)
	if err != nil {
		switch {
		case errors.Is(err, ErrActivityNotFound):
			h.writeError(c, http.StatusNotFound, "ACTIVITY_NOT_FOUND", err)
		case errors.Is(err, ErrMerchantForbidden):
			h.writeError(c, http.StatusNotFound, "ACTIVITY_NOT_FOUND", err)
		case errors.Is(err, ErrInvalidUpdate):
			h.writeError(c, http.StatusBadRequest, "INVALID_REQUEST", err)
		case errors.Is(err, ErrActivityImmutable):
			h.writeError(c, http.StatusConflict, "ACTIVITY_IMMUTABLE", err)
		default:
			h.writeError(c, http.StatusServiceUnavailable, "ACTIVITY_UPDATE_UNAVAILABLE", errors.New("activity update unavailable"))
		}
		return
	}
	if h.invalidator != nil {
		_ = h.invalidator.Invalidate(c.Request.Context(), item.ID, item.ShopID, item.CacheGeneration)
	}
	c.JSON(http.StatusOK, gin.H{"data": item.Display()})
}
func positiveID(raw string) (uint64, error) {
	v, e := strconv.ParseUint(raw, 10, 64)
	if e != nil || v == 0 {
		return 0, ErrInvalidActivityID
	}
	return v, nil
}
func positiveInt(raw string, fallback int) (int, error) {
	if strings.TrimSpace(raw) == "" {
		return fallback, nil
	}
	v, e := strconv.Atoi(raw)
	if e != nil || v < 1 {
		return 0, ErrInvalidPagination
	}
	return v, nil
}
