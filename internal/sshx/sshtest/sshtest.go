// Package sshtest runs a small SSH server for tests. It accepts one
// ed25519 client key, runs "exec" requests with /bin/sh on this machine
// (in Root), runs /bin/sh (without a real pty) for "shell" requests,
// serves the "sftp" subsystem with Root as "/", forwards direct-tcpip
// channels (so it can act as a ProxyJump host) and serves remote
// forwards (tcpip-forward), always on 127.0.0.1.
package sshtest

import (
	"crypto/ed25519"
	"crypto/rand"
	"encoding/binary"
	"encoding/pem"
	"io"
	"net"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"sync"
	"sync/atomic"
	"testing"

	"github.com/skeema/knownhosts"
	"golang.org/x/crypto/ssh"
)

type Server struct {
	Addr       string // 127.0.0.1:port
	Host       string
	Port       int
	User       string
	KeyFile    string // client private key (OpenSSH PEM, unencrypted)
	KnownHosts string // known_hosts file containing this server's key
	Commands   chan string
	// Root is a temporary directory: the SFTP root ("/") and the working
	// directory of commands, so a relative path names the same file in
	// both.
	Root string
	// Sizes receives every pty-req (with Term) and window-change request.
	Sizes   chan Size
	Accepts atomic.Int32 // TCP connections accepted

	mu    sync.Mutex
	conns []net.Conn
}

// Size is a terminal size a client asked for.
type Size struct {
	Term       string // set for pty-req, empty for window-change
	Cols, Rows int
}

// DropAll closes every accepted connection, as if the network failed.
func (s *Server) DropAll() {
	s.mu.Lock()
	defer s.mu.Unlock()
	for _, c := range s.conns {
		c.Close()
	}
	s.conns = nil
}

// Start starts a server that lives until the test ends.
func Start(t *testing.T) *Server {
	t.Helper()
	dir := t.TempDir()
	_, hostPriv, _ := ed25519.GenerateKey(rand.Reader)
	hostSigner, err := ssh.NewSignerFromKey(hostPriv)
	if err != nil {
		t.Fatal(err)
	}
	clientPub, clientPriv, _ := ed25519.GenerateKey(rand.Reader)
	block, err := ssh.MarshalPrivateKey(clientPriv, "")
	if err != nil {
		t.Fatal(err)
	}
	keyFile := filepath.Join(dir, "id_ed25519")
	if err := os.WriteFile(keyFile, pem.EncodeToMemory(block), 0o600); err != nil {
		t.Fatal(err)
	}
	authorized, _ := ssh.NewPublicKey(clientPub)

	cfg := &ssh.ServerConfig{
		PublicKeyCallback: func(_ ssh.ConnMetadata, k ssh.PublicKey) (*ssh.Permissions, error) {
			if string(k.Marshal()) == string(authorized.Marshal()) {
				return nil, nil
			}
			return nil, io.EOF
		},
	}
	cfg.AddHostKey(hostSigner)

	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { ln.Close() })
	addr := ln.Addr().(*net.TCPAddr)
	s := &Server{
		Addr: addr.String(), Host: "127.0.0.1", Port: addr.Port, User: "tester",
		KeyFile: keyFile, KnownHosts: filepath.Join(dir, "known_hosts"),
		Commands: make(chan string, 100),
		Root:     t.TempDir(),
		Sizes:    make(chan Size, 100),
	}
	line := knownhosts.Line([]string{knownhosts.Normalize(s.Addr)}, hostSigner.PublicKey())
	if err := os.WriteFile(s.KnownHosts, []byte(line+"\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	go func() {
		for {
			c, err := ln.Accept()
			if err != nil {
				return
			}
			s.Accepts.Add(1)
			s.mu.Lock()
			s.conns = append(s.conns, c)
			s.mu.Unlock()
			go s.serve(c, cfg)
		}
	}()
	return s
}

func (s *Server) serve(c net.Conn, cfg *ssh.ServerConfig) {
	sc, chans, reqs, err := ssh.NewServerConn(c, cfg)
	if err != nil {
		c.Close()
		return
	}
	fw := &forwards{conn: sc}
	defer fw.closeAll()
	go fw.serve(reqs)
	for nc := range chans {
		switch nc.ChannelType() {
		case "session":
			go s.session(nc)
		case "direct-tcpip":
			go directTCPIP(nc)
		default:
			nc.Reject(ssh.UnknownChannelType, "unsupported")
		}
	}
}

func (s *Server) session(nc ssh.NewChannel) {
	ch, reqs, err := nc.Accept()
	if err != nil {
		return
	}
	defer ch.Close()
	var mu sync.Mutex
	var cmd *exec.Cmd
	pty := false
	kill := func() {
		mu.Lock()
		if cmd != nil && cmd.Process != nil {
			cmd.Process.Kill()
		}
		mu.Unlock()
	}
	defer kill() // the client closed the channel
	for req := range reqs {
		switch req.Type {
		case "exec":
			var p struct{ Command string }
			ssh.Unmarshal(req.Payload, &p)
			req.Reply(true, nil)
			select {
			case s.Commands <- p.Command:
			default:
			}
			c := exec.Command("/bin/sh", "-c", p.Command)
			c.Dir = s.Root
			c.Stdout, c.Stderr = ch, ch.Stderr()
			mu.Lock()
			cmd = c
			err := c.Start()
			mu.Unlock()
			// Wait in the background so that "signal" requests are handled.
			go wait(ch, c, err)
		case "pty-req":
			var p struct {
				Term             string
				Cols, Rows, W, H uint32
				Modes            string
			}
			ssh.Unmarshal(req.Payload, &p)
			pty = true
			s.size(Size{Term: p.Term, Cols: int(p.Cols), Rows: int(p.Rows)})
			req.Reply(true, nil)
		case "window-change":
			var p struct{ Cols, Rows, W, H uint32 }
			ssh.Unmarshal(req.Payload, &p)
			s.size(Size{Cols: int(p.Cols), Rows: int(p.Rows)})
			req.Reply(false, nil)
		case "shell":
			req.Reply(true, nil)
			// No real pty: a pty would turn Enter (\r) into \n and merge
			// stderr into the output, so do that here.
			c := exec.Command("/bin/sh")
			c.Stdout, c.Stderr = ch, ch.Stderr()
			if pty {
				c.Stderr = ch
			}
			stdin, perr := c.StdinPipe()
			mu.Lock()
			cmd = c
			err := perr
			if err == nil {
				err = c.Start()
			}
			mu.Unlock()
			if err == nil {
				go func() {
					var in io.Reader = ch
					if pty {
						in = crToNL{ch}
					}
					io.Copy(stdin, in)
					stdin.Close()
				}()
			}
			go wait(ch, c, err)
		case "subsystem":
			var p struct{ Name string }
			ssh.Unmarshal(req.Payload, &p)
			if p.Name != "sftp" {
				req.Reply(false, nil)
				continue
			}
			req.Reply(true, nil)
			go serveSFTP(ch, s.Root)
		case "signal":
			kill()
		default:
			req.Reply(false, nil)
		}
	}
}

func (s *Server) size(sz Size) {
	select {
	case s.Sizes <- sz:
	default:
	}
}

// wait sends c's exit status once it ends (err is its start error), then
// closes the channel.
func wait(ch ssh.Channel, c *exec.Cmd, err error) {
	if err == nil {
		err = c.Wait()
	}
	code := 0
	if err != nil {
		code = 255
		if ee, ok := err.(*exec.ExitError); ok && ee.ExitCode() >= 0 {
			code = ee.ExitCode()
		}
	}
	status := make([]byte, 4)
	binary.BigEndian.PutUint32(status, uint32(code))
	ch.SendRequest("exit-status", false, status)
	ch.Close()
}

// crToNL turns \r into \n, like a pty's ICRNL input mode.
type crToNL struct{ r io.Reader }

func (c crToNL) Read(p []byte) (int, error) {
	n, err := c.r.Read(p)
	for i := range p[:n] {
		if p[i] == '\r' {
			p[i] = '\n'
		}
	}
	return n, err
}

func directTCPIP(nc ssh.NewChannel) {
	var p struct {
		Host     string
		Port     uint32
		OrigHost string
		OrigPort uint32
	}
	if err := ssh.Unmarshal(nc.ExtraData(), &p); err != nil {
		nc.Reject(ssh.ConnectionFailed, err.Error())
		return
	}
	dst, err := net.Dial("tcp", net.JoinHostPort(p.Host, strconv.Itoa(int(p.Port))))
	if err != nil {
		nc.Reject(ssh.ConnectionFailed, err.Error())
		return
	}
	ch, reqs, err := nc.Accept()
	if err != nil {
		dst.Close()
		return
	}
	go ssh.DiscardRequests(reqs)
	go func() { io.Copy(ch, dst); ch.CloseWrite() }()
	io.Copy(dst, ch)
	dst.Close()
	ch.Close()
}
