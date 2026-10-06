package scan

import "testing"

// unreadable has no portable equivalent without ACL editing; the Linux leg
// covers permission failures and Windows covers the rest of the scan.
func unreadable(t *testing.T, _, _ string) {
	t.Helper()
	t.Skip("permission denial is exercised on unix")
}
