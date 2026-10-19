package shop

import (
	"context"
	"errors"
	"testing"
)

func newTestService() *Service {
	return NewService(NewMemoryRepository([]Shop{
		{ID: 3, Name: "西山咖啡", Address: "西山区春雨路 3 号"},
		{ID: 1, Name: "翠湖书店", Address: "五华区翠湖北路 1 号"},
		{ID: 2, Name: "南屏小馆", Address: "盘龙区南屏街 2 号"},
	}))
}

func TestServiceListPaginatesInIDOrder(t *testing.T) {
	result, err := newTestService().List(context.Background(), 2, 2)
	if err != nil {
		t.Fatalf("List() error = %v", err)
	}
	if result.Total != 3 || result.Page != 2 || result.PageSize != 2 {
		t.Fatalf("List() page metadata = %+v", result)
	}
	if len(result.Items) != 1 || result.Items[0].ID != 3 {
		t.Fatalf("List() items = %+v, want only ID 3", result.Items)
	}
}

func TestServiceListRejectsInvalidPagination(t *testing.T) {
	_, err := newTestService().List(context.Background(), 0, 20)
	if !errors.Is(err, ErrInvalidPagination) {
		t.Fatalf("List() error = %v, want ErrInvalidPagination", err)
	}
}

func TestServiceGetByIDDistinguishesInvalidAndMissing(t *testing.T) {
	service := newTestService()

	_, err := service.GetByID(context.Background(), 0)
	if !errors.Is(err, ErrInvalidShopID) {
		t.Fatalf("GetByID(0) error = %v, want ErrInvalidShopID", err)
	}

	_, err = service.GetByID(context.Background(), 99)
	if !errors.Is(err, ErrShopNotFound) {
		t.Fatalf("GetByID(99) error = %v, want ErrShopNotFound", err)
	}
}
