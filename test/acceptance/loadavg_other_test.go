//go:build !darwin && !linux

package acceptance

// load1 has no portable source here, so it reports not-known and the tail
// gates in grace_test.go run unguarded — the conservative side: an unreadable
// load never skips an assertion.
func load1() (float64, bool) { return 0, false }
