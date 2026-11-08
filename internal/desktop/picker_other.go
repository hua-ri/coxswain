//go:build !darwin || !cgo

package desktop

// pickFile is nil: there is no native file picker outside macOS.
var pickFile func() string
