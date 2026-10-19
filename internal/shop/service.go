package shop

import "context"

const (
	defaultPageSize = 20
	maxPageSize     = 100
)

// Page is the paginated result returned by shop list operations.
type Page struct {
	Items    []Shop `json:"items"`
	Page     int    `json:"page"`
	PageSize int    `json:"page_size"`
	Total    int    `json:"total"`
}

// Service enforces shop-query input rules before accessing data.
type Service struct {
	repository Repository
}

// NewService creates a shop service backed by repository.
func NewService(repository Repository) *Service {
	return &Service{repository: repository}
}

// List returns shops for a one-based page. A zero page size selects the default.
func (s *Service) List(ctx context.Context, page, pageSize int) (Page, error) {
	if page < 1 || pageSize < 0 || pageSize > maxPageSize {
		return Page{}, ErrInvalidPagination
	}
	if pageSize == 0 {
		pageSize = defaultPageSize
	}

	items, total, err := s.repository.List(ctx, (page-1)*pageSize, pageSize)
	if err != nil {
		return Page{}, err
	}
	return Page{Items: items, Page: page, PageSize: pageSize, Total: total}, nil
}

// GetByID returns a shop only when id is a positive numeric identifier.
func (s *Service) GetByID(ctx context.Context, id int64) (Shop, error) {
	if id <= 0 {
		return Shop{}, ErrInvalidShopID
	}
	return s.repository.FindByID(ctx, id)
}
