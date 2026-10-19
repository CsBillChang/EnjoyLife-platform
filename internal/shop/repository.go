package shop

import (
	"context"
	"sort"
)

// Repository supplies shop data to the service layer.
type Repository interface {
	List(ctx context.Context, offset, limit int) ([]Shop, int, error)
	FindByID(ctx context.Context, id int64) (Shop, error)
}

// MemoryRepository is a deterministic, read-only repository for the first CLI and HTTP examples.
type MemoryRepository struct {
	shops []Shop
}

// NewMemoryRepository copies and orders sample data so callers cannot mutate repository state.
func NewMemoryRepository(shops []Shop) *MemoryRepository {
	items := append([]Shop(nil), shops...)
	sort.Slice(items, func(i, j int) bool { return items[i].ID < items[j].ID })
	return &MemoryRepository{shops: items}
}

// List returns one stable page of shops and the total number of available shops.
func (r *MemoryRepository) List(ctx context.Context, offset, limit int) ([]Shop, int, error) {
	if err := ctx.Err(); err != nil {
		return nil, 0, err
	}

	total := len(r.shops)
	if offset >= total {
		return []Shop{}, total, nil
	}

	end := offset + limit
	if end > total {
		end = total
	}
	return append([]Shop(nil), r.shops[offset:end]...), total, nil
}

// FindByID returns one shop by its stable numeric ID.
func (r *MemoryRepository) FindByID(ctx context.Context, id int64) (Shop, error) {
	if err := ctx.Err(); err != nil {
		return Shop{}, err
	}

	index := sort.Search(len(r.shops), func(i int) bool { return r.shops[i].ID >= id })
	if index == len(r.shops) || r.shops[index].ID != id {
		return Shop{}, ErrShopNotFound
	}
	return r.shops[index], nil
}
