package shop

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"reflect"
	"testing"

	"github.com/gin-gonic/gin"
)

func newTestRouter() *gin.Engine {
	gin.SetMode(gin.TestMode)
	router := gin.New()
	NewHandler(newTestService()).RegisterRoutes(router)
	return router
}

func TestHandlerListAndGetByID(t *testing.T) {
	router := newTestRouter()

	list := httptest.NewRecorder()
	router.ServeHTTP(list, httptest.NewRequest(http.MethodGet, "/shops?page=1&page_size=2", nil))
	if list.Code != http.StatusOK {
		t.Fatalf("GET /shops status = %d, body = %s", list.Code, list.Body.String())
	}

	detail := httptest.NewRecorder()
	router.ServeHTTP(detail, httptest.NewRequest(http.MethodGet, "/shops/2", nil))
	if detail.Code != http.StatusOK {
		t.Fatalf("GET /shops/2 status = %d, body = %s", detail.Code, detail.Body.String())
	}
	var payload struct {
		Data Shop `json:"data"`
	}
	if err := json.Unmarshal(detail.Body.Bytes(), &payload); err != nil {
		t.Fatalf("decode detail response: %v", err)
	}
	want := Shop{ID: 2, Name: "南屏小馆", Address: "盘龙区南屏街 2 号"}
	if !reflect.DeepEqual(payload.Data, want) {
		t.Fatalf("GET /shops/2 data = %+v, want %+v", payload.Data, want)
	}
	var raw struct {
		Data map[string]json.RawMessage `json:"data"`
	}
	if err := json.Unmarshal(detail.Body.Bytes(), &raw); err != nil {
		t.Fatalf("decode raw detail response: %v", err)
	}
	if _, exists := raw.Data["status"]; exists {
		t.Fatal("zero status must remain omitted")
	}
	if _, exists := raw.Data["version"]; exists {
		t.Fatal("zero version must remain omitted")
	}
}

func BenchmarkHandlerGetByID(b *testing.B) {
	router := newTestRouter()
	b.ReportAllocs()
	b.ResetTimer()
	for b.Loop() {
		response := httptest.NewRecorder()
		request := httptest.NewRequest(http.MethodGet, "/shops/2", nil)
		router.ServeHTTP(response, request)
		if response.Code != http.StatusOK {
			b.Fatalf("GET /shops/2 status = %d", response.Code)
		}
	}
}

func TestHandlerSeparatesBadRequestAndNotFound(t *testing.T) {
	router := newTestRouter()

	invalid := httptest.NewRecorder()
	router.ServeHTTP(invalid, httptest.NewRequest(http.MethodGet, "/shops/not-a-number", nil))
	if invalid.Code != http.StatusBadRequest {
		t.Fatalf("GET invalid shop status = %d, body = %s", invalid.Code, invalid.Body.String())
	}

	missing := httptest.NewRecorder()
	router.ServeHTTP(missing, httptest.NewRequest(http.MethodGet, "/shops/99", nil))
	if missing.Code != http.StatusNotFound {
		t.Fatalf("GET missing shop status = %d, body = %s", missing.Code, missing.Body.String())
	}
}
