//go:build race

package registry

// raceEnabled reports whether the test binary was built with -race. The race
// detector instruments memory access, which perturbs allocation accounting,
// so the one test that counts allocations skips rather than reporting a
// number it cannot trust. `make test` runs with -race; `make test-short`
// does not, and that is where the count is checked.
const raceEnabled = true
