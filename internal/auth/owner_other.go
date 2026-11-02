//go:build !unix

package auth

import "io/fs"

// fileOwner has no answer off unix. Windows has no uid to compare against —
// os.Geteuid returns -1 there — and the key's protection is the ACL on the
// profile directory, the same reason [checkMode] is skipped.
func fileOwner(fs.FileInfo) (uid int, known bool) {
	return 0, false
}
