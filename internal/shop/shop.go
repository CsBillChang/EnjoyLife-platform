// Package shop contains the in-memory shop domain used by the CLI and HTTP API.
package shop

// Shop is the public representation of a local merchant shop.
type Shop struct {
	ID      int64  `json:"id"`
	Name    string `json:"name"`
	Address string `json:"address"`
	Status  string `json:"status,omitempty"`
	Version uint64 `json:"version,omitempty"`
}
