//go:build !darwin || !cgo

package desktop

// setupMenu does nothing outside macOS.
func setupMenu(string) {}
