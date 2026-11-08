package sshx

import (
	"context"

	"github.com/hua-ri/coxswain/internal/inventory"
	"github.com/pkg/sftp"
)

// Connect returns a connection to h from pool, or (pool nil) a new one
// that release closes. The caller must call release, with broken=true if
// the connection failed.
func Connect(ctx context.Context, pool *Pool, d *Dialer, h inventory.Host) (*Conn, func(broken bool), error) {
	if pool != nil {
		return pool.Get(ctx, h)
	}
	conn, err := d.Dial(ctx, h)
	if err != nil {
		return nil, nil, err
	}
	return conn, func(bool) { conn.Close() }, nil
}

// SFTP opens an SFTP session to h on a pooled connection (see Connect),
// which it also returns for running commands. release closes the session
// and returns the connection; pass broken=true if the session failed.
func SFTP(ctx context.Context, pool *Pool, d *Dialer, h inventory.Host) (*sftp.Client, *Conn, func(broken bool), error) {
	conn, release, err := Connect(ctx, pool, d, h)
	if err != nil {
		return nil, nil, nil, err
	}
	c, err := sftp.NewClient(conn.Client)
	if err != nil {
		release(true)
		return nil, nil, nil, err
	}
	return c, conn, func(broken bool) {
		c.Close()
		release(broken)
	}, nil
}
