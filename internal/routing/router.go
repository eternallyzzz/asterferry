package routing

import (
	"embed"
	"net"
	"strings"

	"github.com/oschwald/geoip2-golang"

	"asterferry/internal/config"
)

//go:embed cn.mmdb
var countryDB embed.FS

type Router struct {
	Rules   []config.RouteRule
	Default string
	db      *geoip2.Reader
}

func New(c config.ProxyConfig) (*Router, error) {
	r := &Router{Rules: c.Rules, Default: c.DefaultRoute}
	return newRouter(r)
}

func NewOptions(c config.ProxyOptions) (*Router, error) {
	r := &Router{Rules: c.Rules, Default: c.DefaultRoute}
	return newRouter(r)
}

func newRouter(r *Router) (*Router, error) {
	if r.Default == "" {
		r.Default = config.RouteGateway
	}
	b, err := countryDB.ReadFile("cn.mmdb")
	if err != nil {
		return nil, err
	}
	r.db, err = geoip2.FromBytes(b)
	if err != nil {
		return nil, err
	}
	return r, nil
}

func (r *Router) Close() error {
	if r != nil && r.db != nil {
		return r.db.Close()
	}
	return nil
}

func (r *Router) Choose(inbound, host string, ip net.IP) string {
	for _, rule := range r.Rules {
		if rule.Inbound != "" && rule.Inbound != inbound {
			continue
		}
		if !matches(rule, host, ip, r.db) {
			continue
		}
		return rule.Route
	}
	return r.Default
}

func matches(rule config.RouteRule, host string, ip net.IP, db *geoip2.Reader) bool {
	if len(rule.CIDRs) == 0 && len(rule.GeoIP) == 0 && len(rule.Domains) == 0 {
		return true
	}
	for _, cidr := range rule.CIDRs {
		_, n, err := net.ParseCIDR(cidr)
		if err == nil && n.Contains(ip) {
			return true
		}
	}
	for _, domain := range rule.Domains {
		domain = strings.TrimPrefix(strings.ToLower(strings.TrimSpace(domain)), "*.")
		h := strings.ToLower(strings.TrimSuffix(host, "."))
		if h == domain || strings.HasSuffix(h, "."+domain) {
			return true
		}
	}
	for _, code := range rule.GeoIP {
		code = strings.ToLower(strings.TrimSpace(code))
		if code == "private" && (ip.IsPrivate() || ip.IsLoopback() || ip.IsLinkLocalUnicast()) {
			return true
		}
		if db != nil && ip != nil {
			country, err := db.Country(ip)
			if err == nil && strings.EqualFold(country.Country.IsoCode, code) {
				return true
			}
		}
	}
	return false
}
