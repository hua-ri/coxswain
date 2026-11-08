//go:build !cgo

package desktop

import (
	"context"
	"errors"
)

// Run reports that this build has no desktop window (it needs cgo).
func Run(context.Context, string, string, bool) error {
	return errors.New("this cox was built without the desktop app (cgo disabled)")
}
