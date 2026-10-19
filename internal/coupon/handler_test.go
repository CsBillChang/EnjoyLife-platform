package coupon

import (
	"context"
	"errors"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/CsBillChang/local-life-platform/internal/auth"
	"github.com/gin-gonic/gin"
)

type handlerRepo struct {
	item      Activity
	listCalls int
}

func (r *handlerRepo) ListByShop(_ context.Context, _ uint64, offset, limit int) ([]Activity, int, error) {
	r.listCalls++
	if offset < 0 || limit < 1 {
		return nil, 0, ErrInvalidPagination
	}
	return []Activity{r.item}, 1, nil
}
func (r *handlerRepo) FindByID(_ context.Context, id uint64) (Activity, error) {
	if id != r.item.ID {
		return Activity{}, ErrActivityNotFound
	}
	return r.item, nil
}

type updateFake struct {
	input  UpdateInput
	err    error
	result Activity
}

func (f *updateFake) UpdateOwned(_ context.Context, _ uint64, _ uint64, input UpdateInput) (Activity, error) {
	f.input = input
	return f.result, f.err
}

type invalidatorFake struct{ calls int }

func (f *invalidatorFake) Invalidate(context.Context, uint64, uint64, uint64) error {
	f.calls++
	return nil
}
func merchantActor() gin.HandlerFunc {
	return func(c *gin.Context) {
		c.Set("authenticated_actor", auth.Actor{UserID: 7, Role: auth.RoleMerchant})
		c.Next()
	}
}

func TestPublicHandlerPaginationAndHint(t *testing.T) {
	gin.SetMode(gin.TestMode)
	repo := &handlerRepo{item: Activity{ID: 2, ShopID: 1, Title: "A", AvailableStock: 8, StartsAt: time.Now(), EndsAt: time.Now().Add(time.Hour), VoucherValidUntil: time.Now().Add(2 * time.Hour)}}
	r := gin.New()
	NewPublicHandler(repo).RegisterPublicRoutes(r)
	w := httptest.NewRecorder()
	r.ServeHTTP(w, httptest.NewRequest(http.MethodGet, "/shops/1/activities?page=0", nil))
	if w.Code != http.StatusBadRequest {
		t.Fatalf("status=%d", w.Code)
	}
	w = httptest.NewRecorder()
	r.ServeHTTP(w, httptest.NewRequest(http.MethodGet, "/shops/1/activities?page=9223372036854775807&page_size=100", nil))
	if w.Code != http.StatusBadRequest {
		t.Fatalf("overflow page status=%d", w.Code)
	}
	w = httptest.NewRecorder()
	r.ServeHTTP(w, httptest.NewRequest(http.MethodGet, "/shops/1/activities?page=1&page_size=20", nil))
	if w.Code != http.StatusOK || !strings.Contains(w.Body.String(), "available_stock_hint") {
		t.Fatalf("%d %s", w.Code, w.Body.String())
	}
}

func TestPublicRoutesComposeWithShopDetailWildcard(t *testing.T) {
	gin.SetMode(gin.TestMode)
	r := gin.New()
	r.GET("/shops/:id", func(c *gin.Context) { c.Status(http.StatusOK) })
	NewPublicHandler(&handlerRepo{}).RegisterPublicRoutes(r)
	w := httptest.NewRecorder()
	r.ServeHTTP(w, httptest.NewRequest(http.MethodGet, "/shops/1", nil))
	if w.Code != http.StatusOK {
		t.Fatalf("shop detail status=%d", w.Code)
	}
}
func TestMerchantHandlerRejectsInvalidAndMapsImmutable(t *testing.T) {
	gin.SetMode(gin.TestMode)
	fake := &updateFake{err: ErrActivityImmutable}
	r := gin.New()
	r.Use(merchantActor())
	NewMerchantHandler(fake, nil).RegisterMerchantRoutes(r)
	w := httptest.NewRecorder()
	r.ServeHTTP(w, httptest.NewRequest(http.MethodPatch, "/merchant/activities/1", strings.NewReader(`{}`)))
	if w.Code != http.StatusBadRequest {
		t.Fatalf("empty update=%d", w.Code)
	}
	w = httptest.NewRecorder()
	r.ServeHTTP(w, httptest.NewRequest(http.MethodPatch, "/merchant/activities/1", strings.NewReader(`{"title":"new","stock":1}`)))
	if w.Code != http.StatusBadRequest {
		t.Fatalf("unknown field=%d", w.Code)
	}
	w = httptest.NewRecorder()
	r.ServeHTTP(w, httptest.NewRequest(http.MethodPatch, "/merchant/activities/1", strings.NewReader(`{"price_cents":3}`)))
	if w.Code != http.StatusConflict {
		t.Fatalf("immutable=%d", w.Code)
	}
	fake.err = ErrMerchantForbidden
	w = httptest.NewRecorder()
	r.ServeHTTP(w, httptest.NewRequest(http.MethodPatch, "/merchant/activities/1", strings.NewReader(`{"title":"new"}`)))
	if w.Code != http.StatusNotFound {
		t.Fatalf("ownership=%d", w.Code)
	}
	fake.err = ErrActivityImmutable
	w = httptest.NewRecorder()
	r.ServeHTTP(w, httptest.NewRequest(http.MethodPatch, "/merchant/activities/1", strings.NewReader(`{"ends_at":"2026-09-22T10:00:00Z"}`)))
	if w.Code != http.StatusConflict {
		t.Fatalf("term update=%d", w.Code)
	}
	fake.err = errors.New("db")
	w = httptest.NewRecorder()
	r.ServeHTTP(w, httptest.NewRequest(http.MethodPatch, "/merchant/activities/1", strings.NewReader(`{"title":"new"}`)))
	if w.Code != http.StatusServiceUnavailable {
		t.Fatalf("dependency=%d", w.Code)
	}
}
