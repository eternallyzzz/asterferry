package domain

import "net/url"

// NodeIdentityURI returns the canonical SPIFFE identity used in node
// certificates. The caller must provide an already validated node ID.
func NodeIdentityURI(id string) *url.URL {
	return &url.URL{Scheme: "spiffe", Host: "asterferry", Path: "/node/" + id}
}
