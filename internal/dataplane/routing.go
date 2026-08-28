package dataplane

import (
	"embed"
	"net"
	"strings"
	"sync"

	"github.com/oschwald/geoip2-golang"

	"asterferry/internal/domain"
)

// The country database is a read-only data-plane asset. It is embedded here
// so routing never needs to open a Controller/configuration package at run
// time.
//
//go:embed cn.mmdb
var countryDB embed.FS

var routeDB struct {
	sync.Once
	db  *geoip2.Reader
	err error
}

func countryReader() (*geoip2.Reader, error) {
	routeDB.Do(func() {
		data, err := countryDB.ReadFile("cn.mmdb")
		if err != nil {
			routeDB.err = err
			return
		}
		routeDB.db, routeDB.err = geoip2.FromBytes(data)
	})
	return routeDB.db, routeDB.err
}

// SelectRoute evaluates the ordered Agent route rules. It intentionally has a
// pure fallback for deployments that omit the optional embedded country data;
// CIDR, domain and private-address rules still work in that case.
func SelectRoute(spec domain.AgentSpec, target string) string {
	host, _, err := net.SplitHostPort(target)
	if err != nil {
		return "direct"
	}
	host = strings.TrimSuffix(strings.ToLower(strings.TrimSpace(host)), ".")
	ip := net.ParseIP(strings.Trim(host, "[]"))
	db, _ := countryReader()
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
