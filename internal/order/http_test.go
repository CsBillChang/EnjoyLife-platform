package order

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/CsBillChang/local-life-platform/internal/auth"
	"github.com/gin-gonic/gin"
)

type fakeSyncPurchaseService struct {
	purchaseOrder Order
	purchaseErr   error
	getOrder      Order
	getErr        error
	userID        uint64
	activityID    uint64
	key           string
	orderID       uint64
}

func (f *fakeSyncPurchaseService) Purchase(_ context.Context, userID, activityID uint64, key string, _ time.Time) (Order, error) {
	f.userID, f.activityID, f.key = userID, activityID, key
	return f.purchaseOrder, f.purchaseErr
}

func (f *fakeSyncPurchaseService) GetOwnedOrder(_ context.Context, userID, orderID uint64) (Order, error) {
	f.userID, f.orderID = userID, orderID
	return f.getOrder, f.getErr
}

func newOrderRouter(service SyncPurchaseService, authenticated bool) *gin.Engine {
	gin.SetMode(gin.TestMode)
	router := gin.New()
	if authenticated {
		router.Use(func(c *gin.Context) {
			c.Set("authenticated_actor", auth.Actor{UserID: 7, Role: auth.RoleCustomer})
		})
	}
	NewHTTPHandler(service).RegisterRoutes(router)
	return router
}

func TestPurchaseRequiresAuthenticationAndIdempotencyKey(t *testing.T) {
	service := &fakeSyncPurchaseService{}
	unauthenticated := newOrderRouter(service, false)
	recorder := httptest.NewRecorder()
	unauthenticated.ServeHTTP(recorder, httptest.NewRequest(http.MethodPost, "/sync/purchases", strings.NewReader(`{"activity_id": 1}`)))
	if recorder.Code != http.StatusUnauthorized {
		t.Fatalf("unauthenticated status = %d, body = %s", recorder.Code, recorder.Body.String())
	}

	authenticated := newOrderRouter(service, true)
	recorder = httptest.NewRecorder()
	authenticated.ServeHTTP(recorder, httptest.NewRequest(http.MethodPost, "/sync/purchases", strings.NewReader(`{"activity_id": 1}`)))
	if recorder.Code != http.StatusBadRequest {
		t.Fatalf("missing key status = %d, body = %s", recorder.Code, recorder.Body.String())
	}
}

func TestPurchaseSuccessUsesAuthenticatedUser(t *testing.T) {
	service := &fakeSyncPurchaseService{purchaseOrder: Order{ID: 12, UserID: 7, ActivityID: 3, Status: PendingPayment}}
	router := newOrderRouter(service, true)
	recorder := httptest.NewRecorder()
	request := httptest.NewRequest(http.MethodPost, "/sync/purchases", strings.NewReader(`{"activity_id": 3}`))
	request.Header.Set("Content-Type", "application/json")
	request.Header.Set("Idempotency-Key", "purchase-3")
	router.ServeHTTP(recorder, request)

	if recorder.Code != http.StatusCreated {
		t.Fatalf("purchase status = %d, body = %s", recorder.Code, recorder.Body.String())
	}
	if service.userID != 7 || service.activityID != 3 || service.key != "purchase-3" {
		t.Fatalf("Purchase called with user=%d activity=%d key=%q", service.userID, service.activityID, service.key)
	}
	var body struct {
		Data Order `json:"data"`
	}
	if err := json.Unmarshal(recorder.Body.Bytes(), &body); err != nil || body.Data.ID != 12 {
		t.Fatalf("response body = %s, unmarshal error = %v", recorder.Body.String(), err)
	}
}

func TestPurchaseMapsServiceErrors(t *testing.T) {
	cases := []struct {
		name string
		err  error
		want int
	}{
		{"invalid input", ErrInvalidPurchaseInput, http.StatusBadRequest},
		{"activity missing", ErrActivityNotFound, http.StatusNotFound},
		{"sold out", ErrSoldOut, http.StatusConflict},
		{"duplicate purchase", ErrAlreadyPurchased, http.StatusConflict},
		{"idempotency conflict", ErrIdempotencyConflict, http.StatusConflict},
		{"retryable dependency", fmtWrapped(ErrTransactionRetryable), http.StatusServiceUnavailable},
	}
	for _, test := range cases {
		t.Run(test.name, func(t *testing.T) {
			router := newOrderRouter(&fakeSyncPurchaseService{purchaseErr: test.err}, true)
			recorder := httptest.NewRecorder()
			request := httptest.NewRequest(http.MethodPost, "/sync/purchases", strings.NewReader(`{"activity_id": 1}`))
			request.Header.Set("Content-Type", "application/json")
			request.Header.Set("Idempotency-Key", "key-1")
			router.ServeHTTP(recorder, request)
			if recorder.Code != test.want {
				t.Fatalf("status = %d, want %d, body = %s", recorder.Code, test.want, recorder.Body.String())
			}
			if strings.Contains(recorder.Body.String(), test.err.Error()) {
				t.Fatalf("response leaked service error: %s", recorder.Body.String())
			}
		})
	}
}

func TestGetOwnedOrderMapsInputMissingAndDependencyFailures(t *testing.T) {
	router := newOrderRouter(&fakeSyncPurchaseService{}, true)
	invalid := httptest.NewRecorder()
	router.ServeHTTP(invalid, httptest.NewRequest(http.MethodGet, "/orders/nope", nil))
	if invalid.Code != http.StatusBadRequest {
		t.Fatalf("invalid id status = %d", invalid.Code)
	}

	missing := httptest.NewRecorder()
	router = newOrderRouter(&fakeSyncPurchaseService{getErr: ErrOrderNotFound}, true)
	router.ServeHTTP(missing, httptest.NewRequest(http.MethodGet, "/orders/9", nil))
	if missing.Code != http.StatusNotFound {
		t.Fatalf("missing order status = %d", missing.Code)
	}

	dependency := httptest.NewRecorder()
	router = newOrderRouter(&fakeSyncPurchaseService{getErr: errors.New("database connection refused")}, true)
	router.ServeHTTP(dependency, httptest.NewRequest(http.MethodGet, "/orders/9", nil))
	if dependency.Code != http.StatusServiceUnavailable || strings.Contains(dependency.Body.String(), "database connection refused") {
		t.Fatalf("dependency response = %d %s", dependency.Code, dependency.Body.String())
	}
}

func fmtWrapped(err error) error {
	return errors.Join(errors.New("wrapped dependency error"), err)
}
