//go:build !linux

package transfer

// diskFree is only measured on Linux (the image target); elsewhere there is no guard.
func diskFree(string) (int64, bool) { return 0, false }
