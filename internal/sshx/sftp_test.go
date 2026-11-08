package sshx

import (
	"context"
	"io"
	"os"
	"path/filepath"
	"testing"
	"time"
)

func TestSFTPReusesPooledConnection(t *testing.T) {
	p, h, srv := poolFor(t, time.Minute)
	os.WriteFile(filepath.Join(srv.Root, "a.txt"), []byte("hello"), 0o644)
	for range 2 {
		c, release, err := SFTP(context.Background(), p, p.Dialer, h)
		if err != nil {
			t.Fatal(err)
		}
		f, err := c.Open("/a.txt")
		if err != nil {
			t.Fatal(err)
		}
		b, _ := io.ReadAll(f)
		f.Close()
		release(false)
		if string(b) != "hello" {
			t.Fatalf("read %q", b)
		}
	}
	if n := srv.Accepts.Load(); n != 1 {
		t.Fatalf("%d connections, want 1 (pooled)", n)
	}
}

// TestSFTPStaysInRoot: the test server never reaches files outside its
// root, by .. or by symlink.
func TestSFTPStaysInRoot(t *testing.T) {
	p, h, srv := poolFor(t, time.Minute)
	outside := filepath.Join(filepath.Dir(srv.Root), "outside.txt")
	if err := os.WriteFile(outside, []byte("secret"), 0o644); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { os.Remove(outside) })
	os.Symlink(outside, filepath.Join(srv.Root, "link"))
	c, release, err := SFTP(context.Background(), p, p.Dialer, h)
	if err != nil {
		t.Fatal(err)
	}
	defer release(false)
	for _, name := range []string{"../outside.txt", "/../outside.txt", "link", outside} {
		if f, err := c.Open(name); err == nil {
			b, _ := io.ReadAll(f)
			f.Close()
			if string(b) == "secret" {
				t.Fatalf("%s escaped the root", name)
			}
		}
	}
}
