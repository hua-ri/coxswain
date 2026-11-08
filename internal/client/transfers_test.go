package client

import (
	"context"
	"strings"
	"testing"

	"github.com/hua-ri/coxswain/internal/eventlog"
	"github.com/hua-ri/coxswain/internal/server"
	"github.com/hua-ri/coxswain/internal/transfer"
)

func TestTransfers(t *testing.T) {
	var m *transfer.Manager
	c, _ := setupWith(t, func(cfg *server.Config) {
		m = &transfer.Manager{Events: cfg.Engine.Events}
		cfg.Transfers = m
	})
	t.Cleanup(m.CancelAll)
	ctx := context.Background()
	evs, stop := m.Events.Subscribe()
	defer stop()
	done := make(chan error, 1)
	go func() {
		_, err := m.Run(ctx, transfer.Transfer{Direction: transfer.Upload, Host: "dev"}, nil,
			func(ctx context.Context, _ *transfer.Progress) error { <-ctx.Done(); return ctx.Err() })
		done <- err
	}()
	var id string
	for ev := range evs {
		if ev.Type == eventlog.TransferStarted {
			id = ev.Transfer
			break
		}
	}
	list, err := c.Transfers(ctx)
	if err != nil || len(list) != 1 || list[0].ID != id {
		t.Fatalf("%+v %v", list, err)
	}
	if tr, err := c.Transfer(ctx, id); err != nil || tr.Status != transfer.Running {
		t.Fatalf("%+v %v", tr, err)
	}
	if err := c.CancelTransfer(ctx, id); err != nil {
		t.Fatal(err)
	}
	if err := <-done; err == nil || !strings.Contains(err.Error(), "cancelled by human:cli") {
		t.Fatalf("%v", err)
	}
	if err := c.CancelTransfer(ctx, id); err == nil || !strings.Contains(err.Error(), "409") {
		t.Fatalf("cancel finished: %v", err)
	}
}
