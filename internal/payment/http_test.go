package payment

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/CsBillChang/local-life-platform/internal/auth"
	"github.com/CsBillChang/local-life-platform/internal/order"
	"github.com/gin-gonic/gin"
)

type fakeOrders struct {
	item order.Order
	err  error
	seen struct{ userID, orderID uint64 }
}

func (f *fakeOrders) GetOwnedOrder(_ context.Context, userID, orderID uint64) (order.Order, error) {
	f.seen.userID, f.seen.orderID = userID, orderID
	return f.item, f.err
}

type fakeFulfillment struct {
	payment                   order.PaymentResult
	paymentErr                error
	refund                    order.Refund
	requestErr                error
	settledRefund             order.Refund
	settleErr                 error
	redeemOrder               order.Order
	redeemErr                 error
	paymentInput              order.SuccessfulPayment
	refundKey                 string
	resultID                  string
	issued                    order.VoucherResult
	issueErr                  error
	issueUserID, issueOrderID uint64
}

func (f *fakeFulfillment) ReissueOwnedVoucher(_ context.Context, userID, orderID uint64, _ time.Time) (order.VoucherResult, error) {
	f.issueUserID, f.issueOrderID = userID, orderID
	return f.issued, f.issueErr
}

func (f *fakeFulfillment) ApplySuccessfulPayment(_ context.Context, input order.SuccessfulPayment) (order.PaymentResult, error) {
	f.paymentInput = input
	return f.payment, f.paymentErr
}
func (f *fakeFulfillment) RequestNormalRefund(_ context.Context, _ uint64, _ uint64, key string, _ time.Time) (order.Refund, error) {
	f.refundKey = key
	return f.refund, f.requestErr
}
func (f *fakeFulfillment) ApplyRefundResult(_ context.Context, id string, _ order.RefundResult, _ time.Time) (order.Refund, error) {
	f.resultID = id
	return f.settledRefund, f.settleErr
}
func (f *fakeFulfillment) Redeem(context.Context, uint64, string, time.Time) (order.Order, error) {
	return f.redeemOrder, f.redeemErr
}

type fakeProvider struct {
	charge    ChargeResult
	chargeErr error
	refund    RefundResult
	refundErr error
	charges   []ChargeRequest
	refunds   []RefundRequest
}

func (f *fakeProvider) Charge(request ChargeRequest) (ChargeResult, error) {
	f.charges = append(f.charges, request)
	return f.charge, f.chargeErr
}
func (f *fakeProvider) Refund(request RefundRequest) (RefundResult, error) {
	f.refunds = append(f.refunds, request)
	return f.refund, f.refundErr
}

type authRepo struct{ actor auth.Actor }

func (r authRepo) FindUserByUsername(context.Context, string) (auth.User, error) {
	return auth.User{}, auth.ErrNotFound
}
func (r authRepo) CreateSession(context.Context, auth.Session) error { return nil }
func (r authRepo) FindActorByTokenHash(context.Context, [32]byte, time.Time) (auth.Actor, error) {
	return r.actor, nil
}

func newPaymentRouter(handler *HTTPHandler, actor auth.Actor) *gin.Engine {
	gin.SetMode(gin.TestMode)
	router := gin.New()
	authHandler := auth.NewHandler(auth.NewService(authRepo{actor: actor}, time.Hour))
	protected := router.Group("/")
	protected.Use(authHandler.Middleware())
	handler.RegisterRoutes(protected)
	return router
}

func request(router http.Handler, method, target, key, body string) *httptest.ResponseRecorder {
	req := httptest.NewRequest(method, target, strings.NewReader(body))
	req.Header.Set("Authorization", "Bearer test-token")
	if key != "" {
		req.Header.Set("Idempotency-Key", key)
	}
	if body != "" {
		req.Header.Set("Content-Type", "application/json")
	}
	recorder := httptest.NewRecorder()
	router.ServeHTTP(recorder, req)
	return recorder
}

func TestPaymentAttemptGetsAmountFromOwnedOrderAndRepeatsProviderID(t *testing.T) {
	orders := &fakeOrders{item: order.Order{ID: 42, UserID: 7, PriceCents: 1099, Currency: "CNY"}}
	fulfillment := &fakeFulfillment{payment: order.PaymentResult{PaymentID: 9}}
	provider := NewSimulator()
	handler := NewHTTPHandler(orders, fulfillment, provider)
	handler.now = func() time.Time { return time.Date(2026, 9, 22, 0, 0, 0, 0, time.UTC) }
	router := newPaymentRouter(handler, auth.Actor{UserID: 7, Role: auth.RoleCustomer})

	first := request(router, http.MethodPost, "/orders/42/payment-attempts", "pay-key", "{\"amount\":1}")
	if first.Code != http.StatusOK {
		t.Fatalf("first status = %d, body=%s", first.Code, first.Body.String())
	}
	firstTxn := responseString(t, first, "data", "provider_transaction_id")
	second := request(router, http.MethodPost, "/orders/42/payment-attempts", "pay-key", "")
	if second.Code != http.StatusOK {
		t.Fatalf("repeat status = %d, body=%s", second.Code, second.Body.String())
	}
	if secondTxn := responseString(t, second, "data", "provider_transaction_id"); secondTxn != firstTxn {
		t.Fatalf("repeat transaction = %q, want %q", secondTxn, firstTxn)
	}
	if fulfillment.paymentInput.AmountCents != 1099 || fulfillment.paymentInput.Currency != "CNY" || orders.seen.userID != 7 {
		t.Fatalf("payment input did not use authoritative order: %+v, seen=%+v", fulfillment.paymentInput, orders.seen)
	}
}

func TestPaymentAttemptMapsOwnedOrderAndStateFailures(t *testing.T) {
	tests := []struct {
		name      string
		readerErr error
		applyErr  error
		status    int
	}{
		{"not found", order.ErrOrderNotFound, nil, http.StatusNotFound},
		{"state conflict", nil, order.ErrOrderStateConflict, http.StatusConflict},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			orders := &fakeOrders{item: order.Order{ID: 42, PriceCents: 1099, Currency: "CNY"}, err: test.readerErr}
			fulfillment := &fakeFulfillment{paymentErr: test.applyErr}
			handler := NewHTTPHandler(orders, fulfillment, &fakeProvider{charge: ChargeResult{ProviderTransactionID: "pay_1", Status: StatusSuccess}})
			response := request(newPaymentRouter(handler, auth.Actor{UserID: 7, Role: auth.RoleCustomer}), http.MethodPost, "/orders/42/payment-attempts", "pay-key", "")
			if response.Code != test.status {
				t.Fatalf("status = %d, body=%s", response.Code, response.Body.String())
			}
		})
	}
}

func TestRefundUsesPersistedStableRequestID(t *testing.T) {
	fulfillment := &fakeFulfillment{
		refund:        order.Refund{RequestID: "normal-stable-id", PaymentID: 12, AmountCents: 1099, Currency: "CNY"},
		settledRefund: order.Refund{RequestID: "normal-stable-id", Status: string(order.RefundSucceeded)},
	}
	provider := &fakeProvider{refund: RefundResult{ProviderRefundID: "refund_1", Status: StatusSuccess}}
	handler := NewHTTPHandler(&fakeOrders{}, fulfillment, provider)
	response := request(newPaymentRouter(handler, auth.Actor{UserID: 7, Role: auth.RoleCustomer}), http.MethodPost, "/orders/42/refunds", "refund-key", "")
	if response.Code != http.StatusOK {
		t.Fatalf("status = %d, body=%s", response.Code, response.Body.String())
	}
	if len(provider.refunds) != 1 || provider.refunds[0].RefundRequestID != "normal-stable-id" || fulfillment.resultID != "normal-stable-id" {
		t.Fatalf("refund IDs were not stable: provider=%+v resultID=%q", provider.refunds, fulfillment.resultID)
	}
}

func TestRedemptionMapsMerchantAuthorizationAndStateConflict(t *testing.T) {
	merchant := auth.Actor{UserID: 9, Role: auth.RoleMerchant}
	handler := NewHTTPHandler(&fakeOrders{}, &fakeFulfillment{redeemErr: order.ErrMerchantForbidden}, &fakeProvider{})
	response := request(newPaymentRouter(handler, merchant), http.MethodPost, "/merchant/redemptions", "", `{"voucher_code":"abc"}`)
	if response.Code != http.StatusForbidden {
		t.Fatalf("merchant ownership status = %d, body=%s", response.Code, response.Body.String())
	}

	handler = NewHTTPHandler(&fakeOrders{}, &fakeFulfillment{redeemErr: order.ErrOrderStateConflict}, &fakeProvider{})
	response = request(newPaymentRouter(handler, merchant), http.MethodPost, "/merchant/redemptions", "", `{"voucher_code":"abc"}`)
	if response.Code != http.StatusConflict {
		t.Fatalf("state conflict status = %d, body=%s", response.Code, response.Body.String())
	}
}

func TestPaymentAttemptRequiresIdempotencyKey(t *testing.T) {
	handler := NewHTTPHandler(&fakeOrders{}, &fakeFulfillment{}, &fakeProvider{})
	response := request(newPaymentRouter(handler, auth.Actor{UserID: 7, Role: auth.RoleCustomer}), http.MethodPost, "/orders/42/payment-attempts", "", "")
	if response.Code != http.StatusBadRequest {
		t.Fatalf("status = %d, body=%s", response.Code, response.Body.String())
	}
}

func TestInvalidOperationKeyCannotCallProvider(t *testing.T) {
	for _, key := range []string{"", "pay ", " pay", "a b", "line\nbreak", "中文", strings.Repeat("x", 129)} {
		for _, path := range []string{"/orders/42/payment-attempts", "/orders/42/refunds"} {
			provider := &fakeProvider{}
			fulfillment := &fakeFulfillment{}
			orders := &fakeOrders{}
			handler := NewHTTPHandler(orders, fulfillment, provider)
			response := request(newPaymentRouter(handler, auth.Actor{UserID: 7, Role: auth.RoleCustomer}), http.MethodPost, path, key, "")
			if response.Code != http.StatusBadRequest || len(provider.charges) != 0 || len(provider.refunds) != 0 || orders.seen.userID != 0 || fulfillment.refundKey != "" {
				t.Fatalf("invalid key reached a side effect: status=%d key=%q", response.Code, key)
			}
		}
	}
}

func TestVoucherReissueUsesAuthenticatedOwnerAndMapsFailures(t *testing.T) {
	for _, tc := range []struct {
		err    error
		status int
	}{{nil, 200}, {order.ErrOrderNotFound, 404}, {order.ErrOrderStateConflict, 409}, {order.ErrVoucherUnavailable, 409}} {
		fulfillment := &fakeFulfillment{issued: order.VoucherResult{OrderID: 42, VoucherCode: "fresh-code"}, issueErr: tc.err}
		handler := NewHTTPHandler(&fakeOrders{}, fulfillment, &fakeProvider{})
		response := request(newPaymentRouter(handler, auth.Actor{UserID: 7, Role: auth.RoleCustomer}), http.MethodPost, "/orders/42/voucher/reissue", "", `{"user_id":999}`)
		if response.Code != tc.status || fulfillment.issueUserID != 7 || fulfillment.issueOrderID != 42 {
			t.Fatalf("reissue status=%d actor=%d order=%d", response.Code, fulfillment.issueUserID, fulfillment.issueOrderID)
		}
		if tc.err == nil && response.Header().Get("Cache-Control") != "no-store" {
			t.Fatal("voucher plaintext may be cached")
		}
	}
	handler := NewHTTPHandler(&fakeOrders{}, &fakeFulfillment{}, &fakeProvider{})
	router := newPaymentRouter(handler, auth.Actor{UserID: 7})
	for _, path := range []string{"/orders/0/voucher/reissue", "/orders/nope/voucher/reissue"} {
		if response := request(router, http.MethodPost, path, "", ""); response.Code != 400 {
			t.Fatal(response.Code)
		}
	}
	req := httptest.NewRequest(http.MethodPost, "/orders/42/voucher/reissue", nil)
	response := httptest.NewRecorder()
	router.ServeHTTP(response, req)
	if response.Code != 401 {
		t.Fatal("unauthenticated reissue allowed")
	}
}

func responseString(t *testing.T, recorder *httptest.ResponseRecorder, keys ...string) string {
	t.Helper()
	var body map[string]any
	if err := json.Unmarshal(recorder.Body.Bytes(), &body); err != nil {
		t.Fatal(err)
	}
	value := any(body)
	for _, key := range keys {
		values, ok := value.(map[string]any)
		if !ok {
			t.Fatalf("%q is not an object", key)
		}
		value = values[key]
	}
	text, ok := value.(string)
	if !ok {
		t.Fatalf("response value = %#v, want string", value)
	}
	return text
}

var _ auth.Repository = authRepo{}
