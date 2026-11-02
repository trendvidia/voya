// Package importsroot is the corpus for TestTheImportCheckCanFail in
// deps_test.go: a file that does exactly what authn must never do, so the
// check that forbids it is proven able to fire. Under testdata, so the Go
// tool never builds it and the import resolves nothing.
package importsroot

import _ "github.com/trendvidia/voya/internal/config"
