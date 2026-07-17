package discovery

import "os"

func setSafeOpenForTest(fn func(string, string) (*os.File, fileIdentity, error)) func() {
	previous := safeOpenFile
	safeOpenFile = fn
	return func() { safeOpenFile = previous }
}
