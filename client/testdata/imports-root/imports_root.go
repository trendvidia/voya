// Package importsroot is the corpus for TestTheImportCheckCanFail in
// deps_test.go: a file that does exactly what client must never do, so the
// check that forbids it is proven able to fire — with an allowed proto
// import beside it, so the carve-out is proven not to swallow the hit.
// Under testdata, so the Go tool never builds it and the imports resolve
// nothing.
package importsroot

import (
	_ "github.com/trendvidia/voya/internal/registry"
	_ "github.com/trendvidia/voya/proto/voya/registry/v1"
)
