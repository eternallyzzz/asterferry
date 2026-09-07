package controller

import (
	"time"
)

const (
	EnrollmentTTL           = 15 * time.Minute
	NodeCertificateTTL      = 30 * 24 * time.Hour
	CertificateRotateBefore = 7 * 24 * time.Hour
)

type EnrollmentToken struct {
	ID        string     `json:"id"`
	ExpiresAt time.Time  `json:"expires_at"`
	UsedAt    *time.Time `json:"used_at,omitempty"`
	CreatedAt time.Time  `json:"created_at"`
}

type Certificate struct {
	CertificatePEM []byte    `json:"certificate_pem"`
	CAPEM          []byte    `json:"ca_pem"`
	Serial         string    `json:"serial"`
	NotBefore      time.Time `json:"not_before"`
	NotAfter       time.Time `json:"not_after"`
}
