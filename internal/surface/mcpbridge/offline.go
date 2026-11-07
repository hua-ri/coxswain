package mcpbridge

import (
	"context"
	"errors"
	"fmt"
	"sync"
	"sync/atomic"

	"github.com/modelcontextprotocol/go-sdk/jsonrpc"
	sdk "github.com/modelcontextprotocol/go-sdk/mcp"
)

// offline is an in-process MCP session that answers the agent while the
// server is down.
type offline struct {
	conn sdk.Connection
	ids  atomic.Int64

	mu      sync.Mutex
	waiting map[jsonrpc.ID]chan *jsonrpc.Response
}

func startOffline(ctx context.Context, s *sdk.Server) (*offline, error) {
	ct, st := sdk.NewInMemoryTransports()
	if _, err := s.Connect(ctx, st, nil); err != nil {
		return nil, err
	}
	conn, err := ct.Connect(ctx)
	if err != nil {
		return nil, err
	}
	o := &offline{conn: conn, waiting: map[jsonrpc.ID]chan *jsonrpc.Response{}}
	go o.read(ctx)
	return o, nil
}

// call sends a copy of req under a private id and waits for the answer.
func (o *offline) call(ctx context.Context, req *jsonrpc.Request) (*jsonrpc.Response, error) {
	cp := *req
	cp.ID, _ = jsonrpc.MakeID(fmt.Sprintf("cox-offline-%d", o.ids.Add(1)))
	ch := make(chan *jsonrpc.Response, 1)
	o.mu.Lock()
	o.waiting[cp.ID] = ch
	o.mu.Unlock()
	if err := o.conn.Write(ctx, &cp); err != nil {
		return nil, err
	}
	select {
	case resp, ok := <-ch:
		if !ok {
			return nil, errors.New("offline session closed")
		}
		return resp, nil
	case <-ctx.Done():
		return nil, ctx.Err()
	}
}

func (o *offline) notify(msg jsonrpc.Message) { o.conn.Write(context.Background(), msg) }

func (o *offline) read(ctx context.Context) {
	for {
		msg, err := o.conn.Read(ctx)
		if err != nil {
			o.mu.Lock()
			for _, ch := range o.waiting {
				close(ch)
			}
			o.waiting = map[jsonrpc.ID]chan *jsonrpc.Response{}
			o.mu.Unlock()
			return
		}
		if resp, ok := msg.(*jsonrpc.Response); ok {
			o.mu.Lock()
			ch := o.waiting[resp.ID]
			delete(o.waiting, resp.ID)
			o.mu.Unlock()
			if ch != nil {
				ch <- resp
			}
		}
	}
}
