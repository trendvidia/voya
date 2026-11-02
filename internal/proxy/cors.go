package proxy

import (
	"net/url"
	"strings"
)

// originAllowed reports whether a browser page at origin may read responses
// through voya (ADR 003, amendment of 2026-11-01). Any website can send a
// request to loopback, so reflecting every origin would let any page read
// every service. A local frontend is always allowed, from a loopback name or
// from the .internal zone only voya serves; anything else must be named in
// cors_origins. The opaque origin "null" is never allowed: sandboxed frames and
// local files share it, so it identifies nobody.
func (p *proxy) originAllowed(origin string) bool {
	origin = strings.ToLower(origin)
	if p.corsOrigins[origin] {
		return true
	}
	u, err := url.Parse(origin)
	if err != nil || (u.Scheme != "http" && u.Scheme != "https") {
		return false
	}
	return isLocalHost(u.Hostname())
}

// isLocalHost reports whether a host name can only be this machine or voya.
func isLocalHost(host string) bool {
	if host == "localhost" || host == "127.0.0.1" || host == "::1" {
		return true
	}
	return strings.HasSuffix(host, ".localhost") || strings.HasSuffix(host, ".internal")
}
