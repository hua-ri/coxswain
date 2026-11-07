// Package sshtest runs a small SSH server for tests. It accepts one
// ed25519 client key, runs "exec" requests with /bin/sh on this machine,
// and forwards direct-tcpip channels (so it can act as a ProxyJump host).
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
	Accepts    atomic.Int32 // TCP connections accepted

	mu    sync.Mutex
	conns []net.Conn
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
	_, chans, reqs, err := ssh.NewServerConn(c, cfg)
	if err != nil {
		c.Close()
		return
	}
	go ssh.DiscardRequests(reqs)
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
			c.Stdout, c.Stderr = ch, ch.Stderr()
			mu.Lock()
			cmd = c
			err := c.Start()
			mu.Unlock()
			// Wait in the background so that "signal" requests are handled.
			go func() {
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
			}()
		case "signal":
			kill()
		default:
			req.Reply(false, nil)
		}
	}
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
