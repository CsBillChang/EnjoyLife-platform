package order

import (
	"bytes"
	"context"
	"encoding/base64"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"strconv"
	"time"

	"github.com/CsBillChang/local-life-platform/internal/auth"
	"github.com/gin-gonic/gin"
)

type RecordQueryBoundary interface {
	ListPaymentRecords(context.Context, uint64, RecordPage) (PaymentRecordPage, error)
	ListMerchantRedemptionRecords(context.Context, uint64, uint64, RecordPage) (RedemptionRecordPage, error)
}

type RecordHTTPHandler struct {
	service RecordQueryBoundary
}

func NewRecordHTTPHandler(service RecordQueryBoundary) *RecordHTTPHandler {
	return &RecordHTTPHandler{service: service}
}

// RegisterRoutes is deliberately separate from the existing order handler so
// route wiring can be added as one explicit composition change. These routes
// must be mounted beneath auth middleware.
func (h *RecordHTTPHandler) RegisterRoutes(router gin.IRouter) {
	router.GET("/me/payment-records", h.listPaymentRecords)
	router.GET("/merchant/redemption-records", h.listMerchantRedemptionRecords)
}

func (h *RecordHTTPHandler) listPaymentRecords(c *gin.Context) {
	actor, ok := auth.CurrentActor(c)
	if !ok || actor.UserID == 0 {
		h.writeError(c, http.StatusUnauthorized, "UNAUTHORIZED", "authentication required")
		return
	}
	page, err := parseRecordPage(c)
	if err != nil {
		h.writeError(c, http.StatusBadRequest, "INVALID_REQUEST", "invalid pagination")
		return
	}
	result, err := h.service.ListPaymentRecords(c.Request.Context(), actor.UserID, page)
	if err != nil {
		h.writeServiceError(c, err)
		return
	}
	c.JSON(http.StatusOK, gin.H{"data": result})
}

func (h *RecordHTTPHandler) listMerchantRedemptionRecords(c *gin.Context) {
	actor, ok := auth.CurrentActor(c)
	if !ok || actor.UserID == 0 {
		h.writeError(c, http.StatusUnauthorized, "UNAUTHORIZED", "authentication required")
		return
	}
	if actor.Role != auth.RoleMerchant {
		h.writeError(c, http.StatusForbidden, "FORBIDDEN", "merchant role required")
		return
	}
	shopID, err := strconv.ParseUint(c.Query("shop_id"), 10, 64)
	if err != nil || shopID == 0 {
		h.writeError(c, http.StatusBadRequest, "INVALID_REQUEST", "shop_id must be a positive integer")
		return
	}
	page, err := parseRecordPage(c)
	if err != nil {
		h.writeError(c, http.StatusBadRequest, "INVALID_REQUEST", "invalid pagination")
		return
	}
	result, err := h.service.ListMerchantRedemptionRecords(c.Request.Context(), actor.UserID, shopID, page)
	if err != nil {
		h.writeServiceError(c, err)
		return
	}
	c.JSON(http.StatusOK, gin.H{"data": result})
}

func (h *RecordHTTPHandler) writeServiceError(c *gin.Context, err error) {
	switch {
	case errors.Is(err, ErrInvalidRecordPage):
		h.writeError(c, http.StatusBadRequest, "INVALID_REQUEST", "invalid pagination")
	case errors.Is(err, ErrMerchantShopNotFound):
		// This endpoint intentionally hides whether the shop exists. It differs
		// from POST redemption's historical 403 membership error because this is
		// a read/list resource that would otherwise disclose another shop's data.
		h.writeError(c, http.StatusNotFound, "NOT_FOUND", "requested resource was not found")
	default:
		h.writeError(c, http.StatusServiceUnavailable, "RECORDS_UNAVAILABLE", "record service unavailable")
	}
}

func (h *RecordHTTPHandler) writeError(c *gin.Context, status int, code, message string) {
	c.JSON(status, gin.H{"error": gin.H{"code": code, "message": message}})
}

func parseRecordPage(c *gin.Context) (RecordPage, error) {
	page := RecordPage{Limit: DefaultRecordPageSize}
	if rawLimit := c.Query("limit"); rawLimit != "" {
		limit, err := strconv.Atoi(rawLimit)
		if err != nil || limit < 1 || limit > MaxRecordPageSize {
			return RecordPage{}, ErrInvalidRecordPage
		}
		page.Limit = limit
	}
	if rawCursor := c.Query("cursor"); rawCursor != "" {
		cursor, err := decodeRecordCursor(rawCursor)
		if err != nil {
			return RecordPage{}, ErrInvalidRecordPage
		}
		page.cursor = &cursor
	}
	return page, nil
}

type cursorWire struct {
	At string `json:"t"`
	ID uint64 `json:"i"`
}

func encodeRecordCursor(cursor recordCursor) string {
	payload, err := json.Marshal(cursorWire{At: cursor.At.UTC().Format(time.RFC3339Nano), ID: cursor.ID})
	if err != nil {
		return ""
	}
	return base64.RawURLEncoding.EncodeToString(payload)
}

func decodeRecordCursor(raw string) (recordCursor, error) {
	payload, err := base64.RawURLEncoding.DecodeString(raw)
	if err != nil || len(payload) == 0 || len(payload) > 256 {
		return recordCursor{}, ErrInvalidRecordPage
	}
	decoder := json.NewDecoder(bytes.NewReader(payload))
	decoder.DisallowUnknownFields()
	var wire cursorWire
	if err := decoder.Decode(&wire); err != nil {
		return recordCursor{}, ErrInvalidRecordPage
	}
	if err := decoder.Decode(&struct{}{}); !errors.Is(err, io.EOF) {
		return recordCursor{}, ErrInvalidRecordPage
	}
	at, err := time.Parse(time.RFC3339Nano, wire.At)
	if err != nil || at.IsZero() || wire.ID == 0 {
		return recordCursor{}, ErrInvalidRecordPage
	}
	return recordCursor{At: at.UTC(), ID: wire.ID}, nil
}
