package dataplane

import (
	"errors"
	"fmt"
	"log/slog"
	"net"
	"os"
	"strings"
	"sync"
	"time"

	"github.com/oschwald/geoip2-golang"

	"asterferry/internal/domain"
)

const defaultGeoIPMaxAge = 180 * 24 * time.Hour

// ErrGeoIPStale indicates that the configured GeoIP database is older than
// the freshness policy.  A stale database is deliberately treated as
// unavailable so a release cannot silently make routing decisions from data
// that is no longer covered by its documented update policy.
var ErrGeoIPStale = errors.New("geoip database is stale")

// GeoIPResolver lazily loads one externally managed MaxMind-compatible
// database.  GeoIP is optional: an empty path disables it and explicit
// destination, CIDR, domain, and private-address rules remain available.
// The database is read into memory once, so route evaluation does not perform
// filesystem I/O or acquire a lock on the hot path.
type GeoIPResolver struct {
	path   string
	maxAge time.Duration

	once     sync.Once
	db       *geoip2.Reader
	err      error
	warnOnce sync.Once
}

func NewGeoIPResolver(path string) *GeoIPResolver {
	return NewGeoIPResolverWithMaxAge(path, defaultGeoIPMaxAge)
}

func NewGeoIPResolverWithMaxAge(path string, maxAge time.Duration) *GeoIPResolver {
	if maxAge <= 0 {
		maxAge = defaultGeoIPMaxAge
	}
	return &GeoIPResolver{path: strings.TrimSpace(path), maxAge: maxAge}
}

func (r *GeoIPResolver) load() {
	if r == nil || r.path == "" {
		return
	}
	info, err := os.Stat(r.path)
	if err != nil {
		r.err = fmt.Errorf("stat GeoIP database %q: %w", r.path, err)
		return
	}
	if !info.Mode().IsRegular() {
		r.err = fmt.Errorf("GeoIP database %q is not a regular file", r.path)
		return
	}
	if time.Since(info.ModTime()) > r.maxAge {
		r.err = fmt.Errorf("%w: %q modified at %s", ErrGeoIPStale, r.path, info.ModTime().UTC().Format(time.RFC3339))
		return
	}
	data, err := os.ReadFile(r.path)
	if err != nil {
		r.err = fmt.Errorf("read GeoIP database %q: %w", r.path, err)
		return
	}
	r.db, r.err = geoip2.FromBytes(data)
	if r.err != nil {
		r.err = fmt.Errorf("parse GeoIP database %q: %w", r.path, r.err)
	}
}

func (r *GeoIPResolver) reader() (*geoip2.Reader, error) {
	if r == nil {
		return nil, nil
	}
	r.once.Do(r.load)
	if r.err != nil {
		r.warnOnce.Do(func() {
			slog.Default().Warn("GeoIP route database unavailable; GeoIP rules will not match", "path", r.path, "error", r.err)
		})
	}
	return r.db, r.err
}

// Available reports whether the configured database was loaded successfully.
func (r *GeoIPResolver) Available() bool {
	if r == nil || r.path == "" {
		return false
	}
	db, err := r.reader()
	return db != nil && err == nil
}

// Error returns the lazy-load error.  It is nil for a disabled resolver.
func (r *GeoIPResolver) Error() error {
	if r == nil || r.path == "" {
		return nil
	}
	_, err := r.reader()
	return err
}

// GeoIPAvailable keeps the small package-level health helper for callers that
// do not own a resolver.  Without an explicitly configured path there is no
// database to report as available.
func GeoIPAvailable(resolvers ...*GeoIPResolver) bool {
	if len(resolvers) == 0 {
		return false
	}
	return resolvers[0].Available()
}

// SelectRoute evaluates the ordered Agent route rules without GeoIP data.
// It is retained for callers that only need the deterministic fallback rules.
func SelectRoute(spec domain.AgentSpec, target string) string {
	return SelectRouteWithResolver(spec, target, nil)
}

// SelectRouteWithResolver evaluates the ordered Agent route rules using the
// optional externally managed GeoIP database.
func SelectRouteWithResolver(spec domain.AgentSpec, target string, resolver *GeoIPResolver) string {
	host, _, err := net.SplitHostPort(target)
	if err != nil {
		return "direct"
	}
	host = strings.TrimSuffix(strings.ToLower(strings.TrimSpace(host)), ".")
	ip := net.ParseIP(strings.Trim(host, "[]"))
	var db *geoip2.Reader
	if resolver != nil {
		db, _ = resolver.reader()
	}
	for _, rule := range spec.Routes {
		if !rule.Enabled {
			continue
		}
		if routeMatches(rule, host, ip, db) {
			return strings.ToLower(rule.Destination)
		}
	}
	return "direct"
}

func routeMatches(rule domain.RouteRule, host string, ip net.IP, db *geoip2.Reader) bool {
	if len(rule.CIDRs) == 0 && len(rule.Domains) == 0 && len(rule.GeoIP) == 0 {
		return true
	}
	for _, value := range rule.CIDRs {
		_, prefix, err := net.ParseCIDR(strings.TrimSpace(value))
		if err == nil && ip != nil && prefix.Contains(ip) {
			return true
		}
	}
	for _, value := range rule.Domains {
		name := strings.TrimPrefix(strings.ToLower(strings.TrimSuffix(strings.TrimSpace(value), ".")), "*.")
		if host == name || strings.HasSuffix(host, "."+name) {
			return true
		}
	}
	for _, value := range rule.GeoIP {
		code := strings.ToLower(strings.TrimSpace(value))
		if code == "private" && ip != nil && (ip.IsPrivate() || ip.IsLoopback() || ip.IsLinkLocalUnicast()) {
			return true
		}
		if code != "private" && db != nil && ip != nil {
			country, err := db.Country(ip)
			if err == nil && strings.EqualFold(country.Country.IsoCode, code) {
				return true
			}
		}
	}
	return false
}
