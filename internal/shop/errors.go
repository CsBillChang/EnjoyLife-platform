package shop

import "errors"

var (
	// ErrInvalidShopID means a shop ID is absent, non-numeric, or not positive.
	ErrInvalidShopID = errors.New("shop id must be a positive integer")
	// ErrInvalidPagination means the requested page or page size is outside its allowed range.
	ErrInvalidPagination = errors.New("page must be positive and page_size must be between 1 and 100")
	// ErrShopNotFound means no shop exists with the requested ID.
	ErrShopNotFound = errors.New("shop not found")
	// ErrInvalidShopUpdate means a merchant update did not contain valid fields.
	ErrInvalidShopUpdate = errors.New("shop name, address, and status are invalid")
)
