package config

import (
	"errors"
	"fmt"
	"net/url"
	"slices"
	"strconv"
	"strings"
	"unicode"

	configv1 "github.com/trendvidia/voya/proto/voya/config/v1"
)

// maxPort is the highest port number a TCP or UDP listener can bind.
const maxPort = 65535

// ValidationError reports a configuration voya will not run with.
//
// Rendering is multi-line on purpose. ADR 005 makes the error the primary
// diagnostic surface: the developer who just made two listeners collide
// needs the two field names, the value, and which file set each — not
// "invalid config" and a hunt.
type ValidationError struct {
	// Problem is the one-line summary, naming the fields involved.
	Problem string

	// Details are provenance lines, one per field the problem names.
	Details []string
}

func (e *ValidationError) Error() string {
	var b strings.Builder
	b.WriteString("config: ")
	b.WriteString(e.Problem)
	for _, d := range e.Details {
		b.WriteString("\n  ")
		b.WriteString(d)
	}
	return b.String()
}

func (e *ValidationError) Unwrap() error { return ErrInvalid }

// Validate reports whether c is a configuration voya can run with. [Load]
// applies it to every resolution; it is exported for callers that build a
// Config in code rather than resolving one.
//
// Validation covers what voya can check before it binds anything: a port in
// range, no two listeners on one port, a log level that exists, a usable
// shutdown grace. Whether a port is *free* is not knowable here — that
// failure belongs to the daemon, which must name the port and, where it
// can, the process holding it (ADR 001).
func Validate(c *configv1.Config) error {
	return validate(c, func(string) string { return "" })
}

// validate is Validate with provenance. origin renders where a field's
// value came from, or "" when that is unknown.
func validate(c *configv1.Config, origin func(field string) string) error {
	if c == nil {
		return &ValidationError{Problem: "no configuration was resolved"}
	}

	// Proto field names, not Go ones: these are what the developer wrote in
	// a .pxf layer, and what they will search that file for.
	ports := []portField{
		{"proxy_port", c.GetProxyPort()},
		{"dns_port", c.GetDnsPort()},
		{"registration_port", c.GetRegistrationPort()},
	}

	for _, p := range ports {
		switch {
		case p.value == 0:
			// Either the field was cleared by a layer, or someone wrote 0
			// meaning "any free port". The second is a reasonable thing to
			// want and still wrong: every one of these ports is dialled by
			// something that was told the number in advance.
			return &ValidationError{
				Problem: fmt.Sprintf("%s is not set — a listener clients dial by number cannot take an ephemeral port", p.name),
				Details: []string{detail(p, origin)},
			}
		case p.value > maxPort:
			return &ValidationError{
				Problem: fmt.Sprintf("%s is %d, above the highest port number %d", p.name, p.value, maxPort),
				Details: []string{detail(p, origin)},
			}
		}
	}

	for i, a := range ports {
		for _, b := range ports[i+1:] {
			if a.value != b.value {
				continue
			}
			return &ValidationError{
				Problem: fmt.Sprintf("%s and %s are both %d — voya's three listeners each need their own port", a.name, b.name, a.value),
				Details: []string{detail(a, origin), detail(b, origin)},
			}
		}
	}

	if _, known := configv1.LogLevel_name[int32(c.GetLogLevel())]; !known {
		return &ValidationError{
			Problem: fmt.Sprintf("log_level is %d, which is not a known level — use one of %s",
				int32(c.GetLogLevel()), strings.Join(logLevelNames(), ", ")),
			Details: []string{originLine("log_level", origin)},
		}
	}
	if c.GetLogLevel() == configv1.LogLevel_LOG_LEVEL_UNSPECIFIED {
		return &ValidationError{
			Problem: fmt.Sprintf("log_level is not set — use one of %s", strings.Join(logLevelNames(), ", ")),
			Details: []string{originLine("log_level", origin)},
		}
	}

	grace := c.GetShutdownGrace()
	switch {
	case grace == nil:
		return &ValidationError{
			Problem: "shutdown_grace is not set",
			Details: []string{originLine("shutdown_grace", origin)},
		}
	case grace.CheckValid() != nil:
		return &ValidationError{
			Problem: fmt.Sprintf("shutdown_grace is not a valid duration: %v", grace.CheckValid()),
			Details: []string{originLine("shutdown_grace", origin)},
		}
	case grace.AsDuration() < 0:
		return &ValidationError{
			Problem: fmt.Sprintf("shutdown_grace is negative (%s)", grace.AsDuration()),
			Details: []string{originLine("shutdown_grace", origin)},
		}
	}

	if err := validateAuth(c.GetAuth(), origin); err != nil {
		return err
	}

	if err := validateRateLimit(c.GetRateLimit(), origin); err != nil {
		return err
	}

	return validateCORSOrigins(c.GetCorsOrigins(), origin)
}

// validateCORSOrigins holds each entry to the exact form a browser sends in
// Origin. The proxy compares whole strings, so anything else could never match,
// and it would fail silently as a CORS error in someone's browser.
func validateCORSOrigins(origins []string, origin func(field string) string) error {
	for _, entry := range origins {
		if problem := corsOriginProblem(entry); problem != "" {
			return &ValidationError{
				Problem: fmt.Sprintf("cors_origins entry %q is not an origin: %s", entry, problem),
				Details: []string{originLine("cors_origins", origin)},
			}
		}
	}
	return nil
}

// corsOriginProblem says what keeps entry from being an Origin value a browser
// could send, or "" when nothing does.
func corsOriginProblem(entry string) string {
	u, err := url.Parse(entry)
	switch {
	case err != nil:
		return fmt.Sprintf("it does not parse (%v)", errors.Unwrap(err))
	case u.Scheme != "http" && u.Scheme != "https":
		return "the scheme must be http or https"
	case u.Host == "":
		return "it has no host"
	case strings.Contains(entry, "*"):
		return "wildcards are not supported, so name each origin"
	case u.User != nil:
		return "a browser never sends a user name"
	case strings.IndexFunc(u.Hostname(), func(r rune) bool { return r > unicode.MaxASCII }) >= 0:
		return "a browser sends a non-ASCII host as punycode (xn--...)"
	case !strings.HasPrefix(entry, u.Scheme+"://"):
		return "the scheme must be lower case"
	case strings.Contains(entry, "?"):
		return "a browser never sends a query"
	case strings.Contains(entry, "#"):
		return "a browser never sends a fragment"
	case u.Path != "":
		return "a browser never sends a path, not even a trailing slash"
	case strings.HasSuffix(u.Host, ":"):
		return "it has a colon with no port after it"
	case entry != u.Scheme+"://"+u.Host:
		return "write exactly scheme://host[:port]"
	}

	port := u.Port()
	if port == "" {
		return ""
	}
	n, err := strconv.Atoi(port)
	switch {
	case err != nil || n < 1 || n > maxPort || strconv.Itoa(n) != port:
		return fmt.Sprintf("a browser would never send port %s", port)
	case (u.Scheme == "http" && n == 80) || (u.Scheme == "https" && n == 443):
		return fmt.Sprintf("a browser leaves out the default port, so remove :%s", port)
	}
	return ""
}

// validateRateLimit rejects the shapes an enabled limiter cannot run with
// (#223): a budget of zero requests, which refuses everything and reads as
// an outage rather than a limit, and a window that is missing, invalid or
// not positive, which leaves the reset and the Retry-After header with no
// meaning. A disabled limiter is not held to them — its thresholds are
// inert — but the message itself must be set, like auth, so a layer that
// cleared it fails loudly instead of resolving to a limiter with no shape.
func validateRateLimit(rl *configv1.RateLimit, origin func(field string) string) error {
	if rl == nil {
		return &ValidationError{
			Problem: "rate_limit is not set",
			Details: []string{originLine("rate_limit", origin)},
		}
	}
	if !rl.GetEnabled() {
		return nil
	}
	if rl.GetRequests() == 0 {
		return &ValidationError{
			Problem: "rate_limit.requests is 0 — a limiter that admits nothing is an outage, not a limit",
			Details: []string{originLine("rate_limit.requests", origin)},
		}
	}
	window := rl.GetWindow()
	switch {
	case window == nil:
		return &ValidationError{
			Problem: "rate_limit.window is not set",
			Details: []string{originLine("rate_limit.window", origin)},
		}
	case window.CheckValid() != nil:
		return &ValidationError{
			Problem: fmt.Sprintf("rate_limit.window is not a valid duration: %v", window.CheckValid()),
			Details: []string{originLine("rate_limit.window", origin)},
		}
	case window.AsDuration() <= 0:
		return &ValidationError{
			Problem: fmt.Sprintf("rate_limit.window is %s — a budget needs a positive window to reset over", window.AsDuration()),
			Details: []string{originLine("rate_limit.window", origin)},
		}
	}
	return nil
}

// reservedClaims are the claims voya stamps on every token it mints (#208).
// A persona that restated one in Persona.claims would be describing a token
// voya does not mint, so validation refuses it rather than letting the
// persona's value silently win or silently lose.
var reservedClaims = []string{"iss", "sub", "aud", "exp", "iat", "nbf", "jti", "scope"}

// validateAuth rejects the shapes that would silently mint useless tokens
// (#208): no issuer for the service's allowlist to name, no audience, a
// lifetime that is missing or already expired, a persona nothing can ask
// for by name, two personas answering to one name, a persona with no
// subject to put in `sub`, a scope that breaks the space-joined wire form,
// a claim with no name, or a claim voya itself stamps.
func validateAuth(a *configv1.Auth, origin func(field string) string) error {
	if a == nil {
		return &ValidationError{
			Problem: "auth is not set — voya cannot mint tokens without an issuer, an audience and a token lifetime",
			Details: []string{originLine("auth", origin)},
		}
	}
	if a.GetIssuer() == "" {
		return &ValidationError{
			Problem: "auth.issuer is empty — a service's issuer allowlist has nothing to name, so no token voya mints would validate",
			Details: []string{originLine("auth.issuer", origin)},
		}
	}
	if a.GetAudience() == "" {
		return &ValidationError{
			Problem: "auth.audience is empty — a token with no audience is rejected by every service that checks one",
			Details: []string{originLine("auth.audience", origin)},
		}
	}

	lifetime := a.GetTokenLifetime()
	switch {
	case lifetime == nil:
		return &ValidationError{
			Problem: "auth.token_lifetime is not set",
			Details: []string{originLine("auth.token_lifetime", origin)},
		}
	case lifetime.CheckValid() != nil:
		return &ValidationError{
			Problem: fmt.Sprintf("auth.token_lifetime is not a valid duration: %v", lifetime.CheckValid()),
			Details: []string{originLine("auth.token_lifetime", origin)},
		}
	case lifetime.AsDuration() <= 0:
		return &ValidationError{
			Problem: fmt.Sprintf("auth.token_lifetime is %s — a token that expires when it is minted is useless", lifetime.AsDuration()),
			Details: []string{originLine("auth.token_lifetime", origin)},
		}
	}

	seen := make(map[string]int, len(a.GetPersonas()))
	for i, p := range a.GetPersonas() {
		name := p.GetName()
		if name == "" {
			return &ValidationError{
				Problem: fmt.Sprintf("auth.personas[%d] has no name — `voya token --as` has nothing to ask for it by", i),
				Details: []string{originLine("auth.personas", origin)},
			}
		}
		if first, dup := seen[name]; dup {
			return &ValidationError{
				Problem: fmt.Sprintf("auth.personas[%d] and auth.personas[%d] are both named %q — `voya token --as %s` could mint either", first, i, name, name),
				Details: []string{originLine("auth.personas", origin)},
			}
		}
		seen[name] = i

		if p.GetSubject() == "" {
			return &ValidationError{
				Problem: fmt.Sprintf("persona %q has no subject — `sub` is who the token says it is for, and a service that logs or authorizes on it would get an empty string", name),
				Details: []string{originLine("auth.personas", origin)},
			}
		}

		for _, scope := range p.GetScopes() {
			if scope == "" || strings.ContainsFunc(scope, unicode.IsSpace) {
				return &ValidationError{
					Problem: fmt.Sprintf("persona %q has the scope %q — scopes are space-joined on the wire, so one that is empty or contains whitespace cannot be told apart from two", name, scope),
					Details: []string{originLine("auth.personas", origin)},
				}
			}
		}
		if _, set := p.GetClaims()[""]; set {
			return &ValidationError{
				Problem: fmt.Sprintf("persona %q sets a claim with an empty name — nothing can ask a token for it, and some JOSE libraries drop the whole claim set", name),
				Details: []string{originLine("auth.personas", origin)},
			}
		}
		for _, claim := range reservedClaims {
			if _, set := p.GetClaims()[claim]; set {
				return &ValidationError{
					Problem: fmt.Sprintf("persona %q sets the claim %q, which voya stamps itself — use the persona's own fields (subject, scopes, audience) or a different claim name", name, claim),
					Details: []string{originLine("auth.personas", origin)},
				}
			}
		}
	}
	return nil
}

// portField is one of the three listener ports, paired with the name the
// developer writes in a layer file.
type portField struct {
	name  string
	value uint32
}

func detail(p portField, origin func(field string) string) string {
	line := fmt.Sprintf("%s = %d", p.name, p.value)
	if o := origin(p.name); o != "" {
		line += ", " + o
	}
	return line
}

func originLine(field string, origin func(field string) string) string {
	if o := origin(field); o != "" {
		return field + " " + o
	}
	return field
}

// logLevelNames lists the levels a developer may write, in increasing
// verbosity and without the unspecified zero value.
func logLevelNames() []string {
	nums := make([]int32, 0, len(configv1.LogLevel_name))
	for n := range configv1.LogLevel_name {
		if n == int32(configv1.LogLevel_LOG_LEVEL_UNSPECIFIED) {
			continue
		}
		nums = append(nums, n)
	}
	slices.Sort(nums)
	names := make([]string, 0, len(nums))
	for _, n := range nums {
		names = append(names, configv1.LogLevel_name[n])
	}
	return names
}
