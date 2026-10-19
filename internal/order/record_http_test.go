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

type fakeRecordQueries struct {
	paymentResult              PaymentRecordPage
	paymentErr                 error
	redemptionResult           RedemptionRecordPage
	redemptionErr              error
	userID, merchantID, shopID uint64
	page                       RecordPage
}

func (f *fakeRecordQueries) ListPaymentRecords(_ context.Context, userID uint64, page RecordPage) (PaymentRecordPage, error) {
	f.userID, f.page = userID, page
	return f.paymentResult, f.paymentErr
}

func (f *fakeRecordQueries) ListMerchantRedemptionRecords(_ context.Context, merchantID, shopID uint64, page RecordPage) (RedemptionRecordPage, error) {
	f.merchantID, f.shopID, f.page = merchantID, shopID, page
	return f.redemptionResult, f.redemptionErr
}

func recordRouter(service RecordQueryBoundary, actor *auth.Actor) *gin.Engine {
	gin.SetMode(gin.TestMode)
	router := gin.New()
	if actor != nil {
		router.Use(func(c *gin.Context) { c.Set("authenticated_actor", *actor) })
	}
	NewRecordHTTPHandler(service).RegisterRoutes(router)
	return router
}

func TestPaymentRecordsUseAuthenticatedOwnerAndOmitProviderTransaction(t *testing.T) {
	queries := &fakeRecordQueries{paymentResult: PaymentRecordPage{Items: []PaymentRecord{{
		PaymentID: 9, OrderID: 8, ShopID: 7, AmountCents: 1099, Currency: "CNY", Status: "SUCCEEDED", Settlement: "APPLIED", CreatedAt: time.Now(),
	}}}}
	actor := &auth.Actor{UserID: 42, Role: auth.RoleCustomer}
	recorder := httptest.NewRecorder()
	recordRouter(queries, actor).ServeHTTP(recorder, httptest.NewRequest(http.MethodGet, "/me/payment-records?limit=1", nil))
	if recorder.Code != http.StatusOK || queries.userID != 42 || queries.page.Limit != 1 {
		t.Fatalf("status=%d user=%d page=%+v body=%s", recorder.Code, queries.userID, queries.page, recorder.Body.String())
	}
	if body := recorder.Body.String(); contains(body, "provider_transaction_id") {
		t.Fatalf("sensitive provider identity leaked: %s", body)
	}
}

func TestRecordHandlersAuthenticationRolesAndMembershipErrors(t *testing.T) {
	queries := &fakeRecordQueries{paymentResult: PaymentRecordPage{Items: []PaymentRecord{}}}
	unauthenticated := httptest.NewRecorder()
	recordRouter(queries, nil).ServeHTTP(unauthenticated, httptest.NewRequest(http.MethodGet, "/me/payment-records", nil))
	if unauthenticated.Code != http.StatusUnauthorized {
		t.Fatalf("unauthenticated=%d", unauthenticated.Code)
	}

	customer := &auth.Actor{UserID: 4, Role: auth.RoleCustomer}
	forbidden := httptest.NewRecorder()
	recordRouter(queries, customer).ServeHTTP(forbidden, httptest.NewRequest(http.MethodGet, "/merchant/redemption-records?shop_id=3", nil))
	if forbidden.Code != http.StatusForbidden {
		t.Fatalf("customer merchant list=%d", forbidden.Code)
	}

	merchant := &auth.Actor{UserID: 5, Role: auth.RoleMerchant}
	queries.redemptionErr = ErrMerchantShopNotFound
	hidden := httptest.NewRecorder()
	recordRouter(queries, merchant).ServeHTTP(hidden, httptest.NewRequest(http.MethodGet, "/merchant/redemption-records?shop_id=3", nil))
	if hidden.Code != http.StatusNotFound {
		t.Fatalf("unowned merchant shop=%d body=%s", hidden.Code, hidden.Body.String())
	}
}

func TestRecordHandlersRejectInvalidPaginationAndReturnEmptyLists(t *testing.T) {
	actor := &auth.Actor{UserID: 4, Role: auth.RoleCustomer}
	queries := &fakeRecordQueries{paymentResult: PaymentRecordPage{Items: []PaymentRecord{}}}
	for _, target := range []string{
		"/me/payment-records?limit=0", "/me/payment-records?limit=101", "/me/payment-records?cursor=not-base64",
		"/me/payment-records?cursor=eyJ0IjoiMjAyNi0wOS0yMlQwMDowMDowMFoiLCJpIjoxLCJ4IjoyfQ",
	} {
		recorder := httptest.NewRecorder()
		recordRouter(queries, actor).ServeHTTP(recorder, httptest.NewRequest(http.MethodGet, target, nil))
		if recorder.Code != http.StatusBadRequest {
			t.Fatalf("target=%s status=%d", target, recorder.Code)
		}
	}
	recorder := httptest.NewRecorder()
	recordRouter(queries, actor).ServeHTTP(recorder, httptest.NewRequest(http.MethodGet, "/me/payment-records", nil))
	if recorder.Code != http.StatusOK || queries.page.Limit != DefaultRecordPageSize {
		t.Fatalf("empty list=%d page=%+v", recorder.Code, queries.page)
	}
	var body struct {
		Data PaymentRecordPage `json:"data"`
	}
	if err := json.Unmarshal(recorder.Body.Bytes(), &body); err != nil || body.Data.Items == nil || len(body.Data.Items) != 0 {
		t.Fatalf("empty response=%s err=%v", recorder.Body.String(), err)
	}
}

func TestRecordHandlerMapsDependencyErrorsWithoutLeaking(t *testing.T) {
	queries := &fakeRecordQueries{paymentErr: errors.New("database connection refused")}
	actor := &auth.Actor{UserID: 4, Role: auth.RoleCustomer}
	recorder := httptest.NewRecorder()
	recordRouter(queries, actor).ServeHTTP(recorder, httptest.NewRequest(http.MethodGet, "/me/payment-records", nil))
	if recorder.Code != http.StatusServiceUnavailable || contains(recorder.Body.String(), "database connection refused") {
		t.Fatalf("dependency response=%d body=%s", recorder.Code, recorder.Body.String())
	}
}

func contains(value, needle string) bool {
	return strings.Contains(value, needle)
}
