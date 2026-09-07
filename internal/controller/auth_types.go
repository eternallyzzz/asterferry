package controller

import (
	"fmt"
	"time"
)

const (
	RoleViewer   = "viewer"
	RoleOperator = "operator"
	RoleAdmin    = "admin"
)

type User struct {
	ID                string    `json:"id"`
	Username          string    `json:"username"`
	Role              string    `json:"role"`
	Enabled           bool      `json:"enabled"`
	Revision          int64     `json:"revision"`
	CreatedAt         time.Time `json:"created_at"`
	UpdatedAt         time.Time `json:"updated_at"`
	PasswordChangedAt time.Time `json:"-"`
}

type APIToken struct {
	ID        string     `json:"id"`
	UserID    string     `json:"user_id"`
	Name      string     `json:"name"`
	ExpiresAt *time.Time `json:"expires_at,omitempty"`
	RevokedAt *time.Time `json:"revoked_at,omitempty"`
	CreatedAt time.Time  `json:"created_at"`
}

type UserUpdate struct {
	Username *string
	Password *string
	Role     *string
	Enabled  *bool
}

func ValidateRole(role string) error {
	if role != RoleViewer && role != RoleOperator && role != RoleAdmin {
		return fmt.Errorf("unknown role %q", role)
	}
	return nil
}
