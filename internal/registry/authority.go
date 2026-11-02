package registry

import "strings"

// Suffix is the TLD every voya-routed name ends in. `.internal` is
// ICANN-reserved for private use, so it cannot collide with public
// resolution, and it is what Fly's 6PN names use — which is the whole point:
// the dial string does not change between a laptop and production (ADR 002).
const Suffix = ".internal"

// maxNameLen is a DNS label's limit. A service name becomes one, both here
// and on Fly.
const maxNameLen = 63

// ServiceFromAuthority extracts the service name from an :authority or Host
// header — "payments.internal:9777" yields "payments".
//
// This is the proxy's per-request hot path, and [hostFromAuthority] keeps it
// allocation-free for the common case.
//
// Reports false for anything not ending in [Suffix], which the caller turns
// into the diagnostic ADR 005 asks for rather than a bare failure — see
// [IsInternal] for the half of that answer this one does not carry.
func ServiceFromAuthority(authority string) (string, bool) {
	name, ok := strings.CutSuffix(hostFromAuthority(authority), Suffix)
	if !ok || name == "" {
		return "", false
	}
	// A name is a single DNS label: "api.payments.internal" is not this
	// service's to route, and silently treating it as "api.payments" would
	// invent a name nobody registered.
	if strings.IndexByte(name, '.') >= 0 {
		return "", false
	}
	return name, true
}

// IsInternal reports whether an authority is aimed at voya's TLD at all,
// whatever its shape.
//
// [ServiceFromAuthority] answers whether an authority names a service, and
// says nothing about why it does not. This answers the other half, which is
// what lets the proxy tell "example.com is nobody's business of mine" from
// "api.payments.internal is mine, but a service name is one label" — two
// different mistakes that deserve two different diagnostics (ADR 005).
//
// Single-sourced with [ServiceFromAuthority] deliberately: two copies of one
// host-normalisation rule are how a name comes to be claimed by one and
// rejected by the other.
func IsInternal(authority string) bool {
	return strings.HasSuffix(hostFromAuthority(authority), Suffix)
}

// hostFromAuthority reduces an :authority or Host header to a canonical
// hostname: no port, no root dot, lowercase.
//
// Allocation-free for the common case — every result is a slice of the input
// — and the one case that must allocate (an authority containing uppercase,
// which hostnames are allowed to) is detected before paying for it. This is
// the proxy's per-request hot path.
func hostFromAuthority(authority string) string {
	host := authority

	// Strip the port. Not net.SplitHostPort: that errors on an authority
	// with no port at all, which is the form a client using the default
	// port sends. Scanning back to the last colon and requiring digits
	// after it also leaves an IPv6 literal — which has colons inside
	// brackets — to fail the suffix check rather than be mangled here.
	if i := strings.LastIndexByte(host, ':'); i >= 0 {
		if allDigits(host[i+1:]) {
			host = host[:i]
		}
	}

	// A fully-qualified name ends in a root dot. Resolvers and some clients
	// send it; it names the same host.
	host = strings.TrimSuffix(host, ".")

	// Hostnames are case-insensitive, so an authority may arrive in any
	// case. Only pay for the lowering when there is something to lower.
	if hasUpper(host) {
		host = strings.ToLower(host)
	}
	return host
}

// Authority renders the name a caller dials to reach service.
func Authority(service string) string { return service + Suffix }

func allDigits(s string) bool {
	if s == "" {
		return false
	}
	for i := range len(s) {
		if s[i] < '0' || s[i] > '9' {
			return false
		}
	}
	return true
}

func hasUpper(s string) bool {
	for i := range len(s) {
		if s[i] >= 'A' && s[i] <= 'Z' {
			return true
		}
	}
	return false
}

// ValidName reports whether a service may register under this name, and why
// not when it may not. The error is always a [*NameError], whose Reason is
// the half a diagnostic wants without this package's "cannot register"
// framing.
//
// The rule is Fly's app-name rule — lowercase letters, digits and hyphens,
// not leading or trailing a hyphen — deliberately, and not because voya needs
// it. voya exists to keep the dial string identical in both environments
// (ADR 001), so a name that works locally and is rejected by Fly is exactly
// the divergence this project is here to remove. Better to fail at the
// developer's first `voya run` than at their first deploy.
//
// Exported for the proxy, which asks the same question of a name it is asked
// to *route*: a name this refuses can never come good, however long a caller
// retries, and that is the line ADR 005's retryability column is drawn on
// (#88). Single-sourced for the same reason [IsInternal] is — two copies of
// one naming rule are how a name comes to be routable and unregistrable at
// once.
func ValidName(name string) error {
	switch {
	case name == "":
		return &NameError{Name: name, Reason: "a service name cannot be empty"}
	case len(name) > maxNameLen:
		return &NameError{
			Name:   name,
			Reason: "a service name becomes a DNS label, so it cannot exceed 63 characters",
		}
	case name == Reserved:
		return &NameError{
			Name:   name,
			Reason: Reserved + Suffix + " is reserved for voya's own diagnostics endpoint (ADR 005)",
		}
	}
	for i := range len(name) {
		c := name[i]
		switch {
		case c >= 'a' && c <= 'z', c >= '0' && c <= '9':
		case c == '-':
			if i == 0 || i == len(name)-1 {
				return &NameError{
					Name:   name,
					Reason: "a service name cannot start or end with a hyphen",
				}
			}
		default:
			return &NameError{
				Name:   name,
				Reason: "a service name may hold only lowercase letters, digits and hyphens — the same rule Fly applies to app names, so that a name working here works there",
			}
		}
	}
	return nil
}
