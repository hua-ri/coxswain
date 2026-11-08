package server

import (
	"context"
	"crypto/rand"
	"encoding/base64"
	"errors"
	"fmt"
	"net"
	"net/http"
	"path/filepath"
	"strconv"
	"time"

	"github.com/hua-ri/coxswain/internal/filelock"
	"github.com/hua-ri/coxswain/internal/instance"
)

// ErrAlreadyRunning is returned by Run when another server answers.
var ErrAlreadyRunning = errors.New("Coxswain server is already running")

// NewToken returns 32 random bytes, base64url-encoded.
func NewToken() (string, error) {
	var b [32]byte
	if _, err := rand.Read(b[:]); err != nil {
		return "", err
	}
	return base64.RawURLEncoding.EncodeToString(b[:]), nil
}

// Run serves on 127.0.0.1 until ctx ends. Config.Port 0 reuses the
// previous server's port if it is free, else picks a random one. One
// server runs per home: it holds a lock on <home>/server.lock while it
// runs. It writes <home>/server.json once listening (replacing a stale or
// corrupt one) and removes it on shutdown.
func Run(ctx context.Context, cfg Config, home string) error {
	cfg.defaults()
	unlock, err := filelock.TryLock(filepath.Join(home, "server.lock"))
	if errors.Is(err, filelock.ErrLocked) {
		if prev, err := instance.Read(home); err == nil {
			return fmt.Errorf("%w at %s (pid %d)", ErrAlreadyRunning, prev.Addr, prev.PID)
		}
		return ErrAlreadyRunning
	}
	if err != nil {
		return err
	}
	defer unlock()
	prev, _ := instance.Read(home) // only for its port; stale or corrupt is fine
	if cfg.Token == "" {
		if cfg.Token, err = NewToken(); err != nil {
			return err
		}
	}
	ln, err := listen(cfg.Port, prev.Addr)
	if err != nil {
		return err
	}

	base, stop := context.WithCancel(context.Background())
	defer stop()
	srv := &http.Server{
		Handler:           New(cfg),
		ReadHeaderTimeout: 10 * time.Second,
		BaseContext:       func(net.Listener) context.Context { return base },
	}
	served := make(chan error, 1)
	go func() { served <- srv.Serve(ln) }()

	info := instance.Info{
		Addr: ln.Addr().String(), PID: cfg.PID, Version: cfg.Version,
		StartedAt: time.Now().UTC().Truncate(time.Second), Token: cfg.Token,
	}
	if err := instance.Write(home, info); err != nil {
		srv.Close()
		return err
	}
	defer instance.Remove(home, cfg.PID)
	if cfg.Ready != nil {
		cfg.Ready(info)
	}

	select {
	case err := <-served:
		return err
	case <-ctx.Done():
	}
	cfg.Engine.Cancel() // running actions end and are recorded as failed
	if cfg.Transfers != nil {
		cfg.Transfers.CancelAll() // any not started by an action
	}
	if cfg.Terminals != nil {
		cfg.Terminals.CloseAll() // streams end with an exit event
	}
	if cfg.Tunnels != nil {
		cfg.Tunnels.CloseAll()
	}
	stop() // ends event streams and other long requests
	// Requests are already cancelled; a short grace period covers
	// responses being written. (Unused client connections would otherwise
	// hold Shutdown for 5s.)
	shut, cancel := context.WithTimeout(context.Background(), time.Second)
	defer cancel()
	if err := srv.Shutdown(shut); err != nil {
		srv.Close()
	}
	return nil
}

// listen binds 127.0.0.1:port; with port 0 it first tries prevAddr's port.
func listen(port int, prevAddr string) (net.Listener, error) {
	if port == 0 {
		if _, p, err := net.SplitHostPort(prevAddr); err == nil {
			if ln, err := net.Listen("tcp", net.JoinHostPort("127.0.0.1", p)); err == nil {
				return ln, nil
			}
		}
	}
	return net.Listen("tcp", net.JoinHostPort("127.0.0.1", strconv.Itoa(port)))
}
