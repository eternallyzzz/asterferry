package logging

import (
	"crypto/hmac"
	"crypto/rand"
	"crypto/sha256"
	"encoding/hex"
	"strings"
	"sync"
)

var (
	domainKeyOnce sync.Once
	domainKey     [32]byte
)

// DomainHash returns a process-scoped keyed digest. It is intentionally not
// reversible and avoids putting destinations into log aggregation keys.
func DomainHash(domain string) string {
	domainKeyOnce.Do(func() {
		if _, err := rand.Read(domainKey[:]); err != nil {
			// A deterministic fallback is preferable to leaking the domain if the
			// system CSPRNG is unavailable; startup still remains functional.
			domainKey = sha256.Sum256([]byte("asterferry-domain-log-key"))
		}
	})
	mac := hmac.New(sha256.New, domainKey[:])
	_, _ = mac.Write([]byte(strings.ToLower(strings.TrimSuffix(strings.TrimSpace(domain), "."))))
	return hex.EncodeToString(mac.Sum(nil)[:8])
}
