package payment

import (
	"context"
	"errors"
	"fmt"
	"math"
	"net/http"
	"strconv"
	"strings"
	"time"

	"github.com/CsBillChang/local-life-platform/internal/auth"
	"github.com/CsBillChang/local-life-platform/internal/order"
	"github.com/gin-gonic/gin"
)

// OwnedOrderReader exposes only the authoritative order lookup required before
// a local provider charge. It prevents the HTTP request from supplying money.
type OwnedOrderReader interface {
	GetOwnedOrder(ctx context.Context, userID, orderID uint64) (order.Order, error)
}

// FulfillmentBoundary contains transaction-only operations. Provider calls are
// deliberately made by the handler between these method calls, never inside a
// database transaction.
type FulfillmentBoundary interface {
	ApplySuccessfulPayment(ctx context.Context, input order.SuccessfulPayment) (order.PaymentResult, error)
	RequestNormalRefund(ctx context.Context, userID, orderID uint64, key string, now time.Time) (order.Refund, error)
	ApplyRefundResult(ctx context.Context, requestID string, result order.RefundResult, now time.Time) (order.Refund, error)
	Redeem(ctx context.Context, merchantID uint64, code string, now time.Time) (order.Order, error)
	ReissueOwnedVoucher(ctx context.Context, userID, orderID uint64, now time.Time) (order.VoucherResult, error)
}

// Provider is intentionally small so HTTP behavior can be tested without a
// live service. *Simulator implements it.
type Provider interface {
	Charge(request ChargeRequest) (ChargeResult, error)
	Refund(request RefundRequest) (RefundResult, error)
}

// HTTPHandler coordinates authenticated payment, refund, and redemption HTTP
// requests. It should be mounted inside auth middleware; it still fails closed
// when an actor is absent.
type HTTPHandler struct {
	orders      OwnedOrderReader
	fulfillment FulfillmentBoundary
	provider    Provider
	now         func() time.Time
}

func NewHTTPHandler(orders OwnedOrderReader, fulfillment FulfillmentBoundary, provider Provider) *HTTPHandler {
	return &HTTPHandler{orders: orders, fulfillment: fulfillment, provider: provider, now: time.Now}
}

func (h *HTTPHandler) RegisterRoutes(router gin.IRouter) {
	router.POST("/orders/:id/payment-attempts", h.paymentAttempt)
	router.POST("/orders/:id/refunds", h.refund)
	router.POST("/orders/:id/voucher/reissue", h.reissueVoucher)
	router.POST("/merchant/redemptions", h.redeem)
}

func (h *HTTPHandler) paymentAttempt(c *gin.Context) {
	actor, ok := currentActor(c)
	if !ok {
		return
	}
	orderID, key, ok := h.orderIDAndKey(c)
	if !ok {
		return
	}
	item, err := h.orders.GetOwnedOrder(c.Request.Context(), actor.UserID, orderID)
	if err != nil {
		h.writeOrderError(c, err)
		return
	}
	if item.PriceCents > math.MaxInt64 {
		h.writeError(c, http.StatusServiceUnavailable, "PAYMENT_UNAVAILABLE", "payment service unavailable")
		return
	}

	providerResult, err := h.provider.Charge(ChargeRequest{
		AttemptID:     key,
		OrderID:       strconv.FormatUint(item.ID, 10),
		Amount:        int64(item.PriceCents),
		Currency:      item.Currency,
		InitialStatus: StatusSuccess,
	})
	if err != nil {
		h.writeProviderError(c, err)
		return
	}
	if providerResult.Status != StatusSuccess {
		h.writeError(c, http.StatusServiceUnavailable, "PAYMENT_UNAVAILABLE", "payment result is not available")
		return
	}

	result, err := h.fulfillment.ApplySuccessfulPayment(c.Request.Context(), order.SuccessfulPayment{
		UserID: actor.UserID, OrderID: item.ID, AttemptID: key, Provider: "local-simulator",
		TransactionID: providerResult.ProviderTransactionID, AmountCents: item.PriceCents,
		Currency: item.Currency, Now: h.now(),
	})
	if err != nil {
		h.writeFulfillmentError(c, err)
		return
	}
	c.JSON(http.StatusOK, gin.H{"data": gin.H{"payment": result, "provider_transaction_id": providerResult.ProviderTransactionID}})
}

func (h *HTTPHandler) refund(c *gin.Context) {
	actor, ok := currentActor(c)
	if !ok {
		return
	}
	orderID, key, ok := h.orderIDAndKey(c)
	if !ok {
		return
	}

	// This persists the normal-refund request before the provider boundary.
	requested, err := h.fulfillment.RequestNormalRefund(c.Request.Context(), actor.UserID, orderID, key, h.now())
	if err != nil {
		h.writeFulfillmentError(c, err)
		return
	}
	if requested.AmountCents > math.MaxInt64 {
		h.writeError(c, http.StatusServiceUnavailable, "REFUND_UNAVAILABLE", "refund service unavailable")
		return
	}
	providerResult, err := h.provider.Refund(RefundRequest{
		RefundRequestID: requested.RequestID,
		// The local simulator needs a stable reference. PaymentID is immutable
		// within the persisted refund record and is never client supplied.
		ProviderTransactionID: fmt.Sprintf("payment-%d", requested.PaymentID),
		Amount:                int64(requested.AmountCents),
		Currency:              requested.Currency,
		InitialStatus:         StatusSuccess,
	})
	if err != nil {
		h.writeProviderError(c, err)
		return
	}

	settled, err := h.fulfillment.ApplyRefundResult(c.Request.Context(), requested.RequestID, refundResult(providerResult.Status), h.now())
	if err != nil {
		h.writeFulfillmentError(c, err)
		return
	}
	c.JSON(http.StatusOK, gin.H{"data": gin.H{"refund": settled, "provider_refund_id": providerResult.ProviderRefundID}})
}

type redemptionRequest struct {
	VoucherCode string `json:"voucher_code"`
}

func (h *HTTPHandler) reissueVoucher(c *gin.Context) {
	actor, ok := currentActor(c)
	if !ok {
		return
	}
	orderID, err := strconv.ParseUint(c.Param("id"), 10, 64)
	if err != nil || orderID == 0 {
		h.writeError(c, http.StatusBadRequest, "INVALID_REQUEST", "order id must be a positive integer")
		return
	}
	item, err := h.fulfillment.ReissueOwnedVoucher(c.Request.Context(), actor.UserID, orderID, h.now())
	if err != nil {
		h.writeFulfillmentError(c, err)
		return
	}
	c.Header("Cache-Control", "no-store")
	c.JSON(http.StatusOK, gin.H{"data": item})
}

func (h *HTTPHandler) redeem(c *gin.Context) {
	actor, ok := currentActor(c)
	if !ok {
		return
	}
	if actor.Role != auth.RoleMerchant {
		h.writeError(c, http.StatusForbidden, "FORBIDDEN", "merchant role required")
		return
	}
	var request redemptionRequest
	if err := c.ShouldBindJSON(&request); err != nil || strings.TrimSpace(request.VoucherCode) == "" {
		h.writeError(c, http.StatusBadRequest, "INVALID_REQUEST", "voucher_code is required")
		return
	}
	item, err := h.fulfillment.Redeem(c.Request.Context(), actor.UserID, strings.TrimSpace(request.VoucherCode), h.now())
	if err != nil {
		h.writeFulfillmentError(c, err)
		return
	}
	c.JSON(http.StatusOK, gin.H{"data": item})
}

func (h *HTTPHandler) orderIDAndKey(c *gin.Context) (uint64, string, bool) {
	orderID, err := strconv.ParseUint(c.Param("id"), 10, 64)
	if err != nil || orderID == 0 {
		h.writeError(c, http.StatusBadRequest, "INVALID_REQUEST", "order id must be a positive integer")
		return 0, "", false
	}
	key := c.GetHeader("Idempotency-Key")
	if order.ValidateOperationKey(key) != nil {
		h.writeError(c, http.StatusBadRequest, "INVALID_REQUEST", "Idempotency-Key must contain 1 to 128 printable ASCII characters without whitespace")
		return 0, "", false
	}
	return orderID, key, true
}

func currentActor(c *gin.Context) (auth.Actor, bool) {
	actor, ok := auth.CurrentActor(c)
	if !ok || actor.UserID == 0 {
		c.JSON(http.StatusUnauthorized, gin.H{"error": gin.H{"code": "UNAUTHORIZED", "message": "authentication required"}})
		return auth.Actor{}, false
	}
	return actor, true
}

func refundResult(status Status) order.RefundResult {
	switch status {
	case StatusSuccess:
		return order.RefundSucceeded
	case StatusUnknown:
		return order.RefundUnknown
	default:
		return order.RefundFailedFinal
	}
}

func (h *HTTPHandler) writeProviderError(c *gin.Context, err error) {
	if errors.Is(err, ErrIdempotencyConflict) {
		h.writeError(c, http.StatusConflict, "IDEMPOTENCY_CONFLICT", "idempotency key conflicts with an existing request")
		return
	}
	h.writeError(c, http.StatusServiceUnavailable, "PAYMENT_UNAVAILABLE", "payment service unavailable")
}

func (h *HTTPHandler) writeOrderError(c *gin.Context, err error) {
	switch {
	case errors.Is(err, order.ErrInvalidPurchaseInput):
		h.writeError(c, http.StatusBadRequest, "INVALID_REQUEST", "invalid order request")
	case errors.Is(err, order.ErrOrderNotFound):
		h.writeError(c, http.StatusNotFound, "NOT_FOUND", "requested resource was not found")
	default:
		h.writeError(c, http.StatusServiceUnavailable, "ORDER_UNAVAILABLE", "order service unavailable")
	}
}

func (h *HTTPHandler) writeFulfillmentError(c *gin.Context, err error) {
	switch {
	case errors.Is(err, order.ErrInvalidFulfillmentInput), errors.Is(err, order.ErrInvalidPurchaseInput):
		h.writeError(c, http.StatusBadRequest, "INVALID_REQUEST", "invalid request")
	case errors.Is(err, order.ErrOrderNotFound), errors.Is(err, order.ErrVoucherNotFound):
		h.writeError(c, http.StatusNotFound, "NOT_FOUND", "requested resource was not found")
	case errors.Is(err, order.ErrMerchantForbidden):
		h.writeError(c, http.StatusForbidden, "FORBIDDEN", "operation is not permitted")
	case errors.Is(err, order.ErrOrderStateConflict), errors.Is(err, order.ErrPaymentConflict),
		errors.Is(err, order.ErrRefundConflict), errors.Is(err, order.ErrVoucherUnavailable),
		errors.Is(err, order.ErrInventoryInvariant), errors.Is(err, order.ErrActivityUnavailable):
		h.writeError(c, http.StatusConflict, "STATE_CONFLICT", "operation cannot be completed in its current state")
	default:
		h.writeError(c, http.StatusServiceUnavailable, "FULFILLMENT_UNAVAILABLE", "fulfillment service unavailable")
	}
}

func (h *HTTPHandler) writeError(c *gin.Context, status int, code, message string) {
	c.JSON(status, gin.H{"error": gin.H{"code": code, "message": message}})
}
