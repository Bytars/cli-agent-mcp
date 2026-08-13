//go:build !windows

package inspect

// enableANSI is a no-op away from Windows: a stream that reports itself as a
// character device already interprets escape sequences there.
func enableANSI() bool { return true }
