package k8s

import "time"

// SetPluginTimer replaces the credential plugins' timeout timer until
// restore is called.
func SetPluginTimer(after func(time.Duration, func()) func() bool) (restore func()) {
	prev := afterTimeout
	afterTimeout = after
	return func() { afterTimeout = prev }
}
