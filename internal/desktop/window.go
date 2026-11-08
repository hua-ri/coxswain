//go:build cgo

package desktop

import (
	"context"
	"net"
	"net/http"
	"time"

	"github.com/hua-ri/coxswain/internal/client"
	"github.com/hua-ri/coxswain/internal/desktop/ui"
	webview "github.com/webview/webview_go"
)

// Run opens the desktop window on the running server and returns when
// the window is closed or ctx ends. Without a server it returns
// instance.ErrNotRunning before opening anything. It must be called on
// the main thread (the main goroutine, locked with runtime.LockOSThread).
func Run(ctx context.Context, home, version string, debug bool) error {
	if _, err := client.Discover(ctx, home, ClientName); err != nil {
		return err
	}
	// The page is served from 127.0.0.1; it holds no secrets (the token
	// stays in this process) and can do nothing but call the bindings.
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		return err
	}
	srv := &http.Server{Handler: ui.Handler(), ReadHeaderTimeout: 5 * time.Second}
	go srv.Serve(ln)
	defer srv.Close()

	w := webview.New(debug)
	defer w.Destroy()
	b := New(w, home)
	if err := b.Start(ctx); err != nil {
		return err
	}
	defer b.Close()
	for name, f := range b.Bindings() {
		if err := w.Bind(name, f); err != nil {
			return err
		}
	}
	setupMenu(version) // ⌘C / ⌘V / ⌘Q need a main menu
	w.SetTitle("Coxswain")
	w.SetSize(1280, 800, webview.HintNone)
	w.Navigate("http://" + ln.Addr().String() + "/")

	stop := context.AfterFunc(ctx, w.Terminate) // Ctrl-C in the terminal
	defer stop()
	w.Run()
	return nil
}
