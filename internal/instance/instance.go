// Package instance lets clients find the running `cox server`: the server
// writes <home>/server.json while it runs, clients read it and probe
// /healthz.
package instance

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"os"
	"path/filepath"
	"time"

	"github.com/hua-ri/coxswain/internal/filelock"
)

// ErrNotRunning is what every client reports when no server answers.
var ErrNotRunning = errors.New("Coxswain server is not running. Start it with: cox server")

// probeTimeout bounds a /healthz probe.
const probeTimeout = time.Second

// Info is the content of server.json.
type Info struct {
	Addr      string    `json:"addr"` // 127.0.0.1:port
	PID       int       `json:"pid"`
	Version   string    `json:"version"`
	StartedAt time.Time `json:"started_at"`
	Token     string    `json:"token"`
}

// Health is the /healthz response body.
type Health struct {
	Version string `json:"version"`
	PID     int    `json:"pid"`
}

// Path is the instance file under home (~/.coxswain).
func Path(home string) string { return filepath.Join(home, "server.json") }

// Write atomically writes the instance file, mode 0600.
func Write(home string, info Info) error {
	if err := os.MkdirAll(home, 0o700); err != nil {
		return err
	}
	b, err := json.MarshalIndent(info, "", "  ")
	if err != nil {
		return err
	}
	return filelock.WriteAtomic(Path(home), b)
}

// Read returns the instance file; a missing file is ErrNotRunning.
func Read(home string) (Info, error) {
	var info Info
	b, err := os.ReadFile(Path(home))
	if errors.Is(err, os.ErrNotExist) {
		return info, ErrNotRunning
	}
	if err != nil {
		return info, err
	}
	if err := json.Unmarshal(b, &info); err != nil {
		return info, fmt.Errorf("%s: %w", Path(home), err)
	}
	return info, nil
}

// Remove deletes the instance file if it belongs to pid, so a server
// never removes the file of another one that replaced it.
func Remove(home string, pid int) error {
	info, err := Read(home)
	if errors.Is(err, ErrNotRunning) {
		return nil
	}
	if err != nil || info.PID != pid {
		return nil // unreadable or someone else's: leave it
	}
	if err := os.Remove(Path(home)); err != nil && !errors.Is(err, os.ErrNotExist) {
		return err
	}
	return nil
}

// NotRunningError prints as ErrNotRunning (the message users see) and
// keeps the underlying cause for errors.Is/As and debugging.
type NotRunningError struct{ Cause error }

func (e *NotRunningError) Error() string      { return ErrNotRunning.Error() }
func (e *NotRunningError) Unwrap() []error    { return []error{ErrNotRunning, e.Cause} }
func notRunning(cause error) *NotRunningError { return &NotRunningError{cause} }

// Probe checks that the server described by info answers /healthz with
// its pid and version. Any failure is a *NotRunningError (errors.Is
// ErrNotRunning).
func Probe(ctx context.Context, info Info) error {
	ctx, cancel := context.WithTimeout(ctx, probeTimeout)
	defer cancel()
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, "http://"+info.Addr+"/healthz", nil)
	if err != nil {
		return notRunning(err)
	}
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		return notRunning(err)
	}
	defer resp.Body.Close()
	var h Health
	if resp.StatusCode != http.StatusOK {
		return notRunning(fmt.Errorf("healthz: %s", resp.Status))
	}
	if err := json.NewDecoder(resp.Body).Decode(&h); err != nil {
		return notRunning(fmt.Errorf("healthz: %w", err))
	}
	if h.PID != info.PID || h.Version != info.Version {
		return notRunning(fmt.Errorf("another process answers at %s", info.Addr))
	}
	return nil
}

// Discover reads the instance file and probes the server.
func Discover(ctx context.Context, home string) (Info, error) {
	info, err := Read(home)
	if err != nil {
		return info, err
	}
	return info, Probe(ctx, info)
}
