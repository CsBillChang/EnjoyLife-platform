package auth

import "time"

type Role string

const (
	RoleCustomer Role = "CUSTOMER"
	RoleMerchant Role = "MERCHANT"
	RoleAdmin    Role = "ADMIN"
)

type User struct {
	ID           uint64
	Username     string
	PasswordHash string
	Role         Role
}

type Actor struct {
	UserID   uint64 `json:"user_id"`
	Username string `json:"username"`
	Role     Role   `json:"role"`
}

type Session struct {
	TokenHash [32]byte
	UserID    uint64
	ExpiresAt time.Time
}
