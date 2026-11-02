// Package proto holds voya's schemas and the Go bindings generated from
// them. The bindings are committed: `go install` must work with no buf,
// protoc, or plugin on the machine.
//
// Subdirectories:
//
//   - voya/config/v1/   — voya's own configuration (ADR 001).
//   - voya/registry/v1/ — the registration stream (ADR 004).
//   - voya/diag/v1/     — DiagService under voya.internal, and the report
//     shapes the diagnostics CLIs render (ADR 005, #149).
//
// Regenerate with `make generate` from the repo root, which pins the plugin
// versions. `make proto-check` fails if what is committed differs from what
// the pinned toolchain produces.
package proto

//go:generate buf generate
