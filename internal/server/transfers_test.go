package server

import (
	"context"
	"encoding/json"
	"strings"
	"testing"

	"github.com/hua-ri/coxswain/internal/action"
	"github.com/hua-ri/coxswain/internal/eventlog"
	"github.com/hua-ri/coxswain/internal/transfer"
)

// startTransfer runs a transfer for owner that waits until it is
// cancelled; done receives Run's error.
func startTransfer(t *testing.T, f fixture, owner action.Actor) (string, <-chan error) {
	t.Helper()
	ch, cancel := f.cfg.Events.Subscribe()
	defer cancel()
	done := make(chan error, 1)
	go func() {
		_, err := f.cfg.Transfers.Run(context.Background(), transfer.Transfer{Direction: transfer.Download, Host: "dev", Owner: owner},
			nil, func(ctx context.Context, _ *transfer.Progress) error { <-ctx.Done(); return ctx.Err() })
		done <- err
	}()
	for ev := range ch {
		if ev.Type == eventlog.TransferStarted {
			return ev.Transfer, done
		}
	}
	t.Fatal("no transfer.started")
	return "", nil
}

func TestTransfersAPI(t *testing.T) {
	f := newConfig(t)
	f.cfg.Transfers = &transfer.Manager{Events: f.cfg.Events}
	t.Cleanup(f.cfg.Transfers.CancelAll)
	h := New(f.cfg)
	id, done := startTransfer(t, f, Actor("codex"))

	w := request(t, h, "GET", "/api/v1/transfers", "", map[string]string{"X-Cox-Client": "desktop"})
	var list []transfer.Transfer
	if err := json.Unmarshal(w.Body.Bytes(), &list); w.Code != 200 || err != nil || len(list) != 1 || list[0].ID != id || list[0].Status != transfer.Running {
		t.Fatalf("%d %s", w.Code, w.Body)
	}
	w = request(t, h, "GET", "/api/v1/transfers/"+id, "", nil)
	var one transfer.Transfer
	if err := json.Unmarshal(w.Body.Bytes(), &one); w.Code != 200 || err != nil || one.ID != id {
		t.Fatalf("%d %s", w.Code, w.Body)
	}
	if w := request(t, h, "GET", "/api/v1/transfers/x_nope", "", nil); w.Code != 404 {
		t.Fatalf("unknown: %d", w.Code)
	}

	// Agents may look (once allowed) but not cancel.
	codex := map[string]string{"X-Cox-Client": "codex"}
	if w := request(t, h, "GET", "/api/v1/transfers", "", codex); w.Code != 403 {
		t.Fatalf("unauthorized agent: %d", w.Code)
	}
	f.cfg.Clients.Allow("codex")
	if w := request(t, h, "GET", "/api/v1/transfers", "", codex); w.Code != 200 {
		t.Fatalf("authorized agent: %d", w.Code)
	}
	if w := request(t, h, "DELETE", "/api/v1/transfers/"+id, "", codex); w.Code != 403 {
		t.Fatalf("agent cancelled: %d", w.Code)
	}

	if w := request(t, h, "DELETE", "/api/v1/transfers/"+id, "", map[string]string{"X-Cox-Client": "desktop"}); w.Code != 204 {
		t.Fatalf("cancel: %d %s", w.Code, w.Body)
	}
	if err := <-done; err == nil || !strings.Contains(err.Error(), "cancelled by human:desktop") {
		t.Fatalf("%v", err)
	}
	if w := request(t, h, "DELETE", "/api/v1/transfers/"+id, "", nil); w.Code != 409 {
		t.Fatalf("cancel finished: %d", w.Code)
	}
	if w := request(t, h, "DELETE", "/api/v1/transfers/x_nope", "", nil); w.Code != 404 {
		t.Fatalf("cancel unknown: %d", w.Code)
	}
}

func TestTransfersUnavailable(t *testing.T) {
	h := New(newConfig(t).cfg)
	if w := request(t, h, "GET", "/api/v1/transfers", "", nil); w.Code != 503 {
		t.Fatalf("%d", w.Code)
	}
}

func TestRunCancelsTransfers(t *testing.T) {
	f := newConfig(t)
	f.cfg.Transfers = &transfer.Manager{Events: f.cfg.Events}
	f.cfg.Token = ""
	_, stop := runReady(t, f.cfg, t.TempDir())
	_, done := startTransfer(t, f, Actor("cli"))
	stop()
	if err := <-done; err == nil || !strings.Contains(err.Error(), "server stopped") {
		t.Fatalf("%v", err)
	}
}
