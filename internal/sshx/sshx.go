// Package sshx opens SSH connections the way OpenSSH users expect:
// ssh-agent and key files, strict known_hosts, ProxyJump chains.
package sshx

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"net"
	"os"
	"os/user"
	"strconv"
	"strings"
	"time"

	"github.com/hua-ri/coxswain/internal/inventory"
	"github.com/skeema/knownhosts"
	"golang.org/x/crypto/ssh"
	"golang.org/x/crypto/ssh/agent"
)

type Dialer struct {
	Hosts       *inventory.Store // resolves ProxyJump aliases
	KnownHosts  []string         // known_hosts files; missing files are skipped
	DefaultKeys []string         // tried when a host lists no IdentityFile
	AgentSocket string           // SSH_AUTH_SOCK; empty disables the agent
	Timeout     time.Duration
}

// Conn is a connection to a host plus the jump connections under it.
type Conn struct {
	*ssh.Client
	jumps []*ssh.Client
}

func (c *Conn) Close() error {
	err := c.Client.Close()
	for i := len(c.jumps) - 1; i >= 0; i-- {
		c.jumps[i].Close()
	}
	return err
}

// Dial connects to h, through h.ProxyJump if set.
func (d *Dialer) Dial(ctx context.Context, h inventory.Host) (*Conn, error) {
	hops, err := d.chain(h)
	if err != nil {
		return nil, err
	}
	var clients []*ssh.Client
	for _, hop := range hops {
		var via *ssh.Client
		if n := len(clients); n > 0 {
			via = clients[n-1]
		}
		c, err := d.dialHop(ctx, via, hop)
		if err != nil {
			for i := len(clients) - 1; i >= 0; i-- {
				clients[i].Close()
			}
			return nil, fmt.Errorf("%s: %w", hop.Name, err)
		}
		clients = append(clients, c)
	}
	n := len(clients)
	return &Conn{Client: clients[n-1], jumps: clients[:n-1]}, nil
}

// chain returns the hops to reach h, h last.
func (d *Dialer) chain(h inventory.Host) ([]inventory.Host, error) {
	var hops []inventory.Host
	if h.ProxyJump != "" {
		for _, spec := range strings.Split(h.ProxyJump, ",") {
			hop, err := d.resolveJump(strings.TrimSpace(spec))
			if err != nil {
				return nil, err
			}
			if hop.ProxyJump != "" && len(hops) == 0 {
				inner, err := d.chain(hop)
				if err != nil {
					return nil, err
				}
				hops = append(hops, inner...)
				continue
			}
			hops = append(hops, hop)
		}
	}
	if len(hops) > 8 {
		return nil, errors.New("ProxyJump chain longer than 8 hops")
	}
	return append(hops, h), nil
}

func (d *Dialer) resolveJump(spec string) (inventory.Host, error) {
	if d.Hosts != nil {
		if h, err := d.Hosts.Get(spec); err == nil {
			return h, nil
		}
	}
	h := inventory.Host{Name: spec, Port: 22}
	if at := strings.LastIndex(spec, "@"); at >= 0 {
		h.User, spec = spec[:at], spec[at+1:]
	}
	host, port, err := net.SplitHostPort(spec)
	if err != nil {
		host = spec
	} else if h.Port, err = strconv.Atoi(port); err != nil {
		return h, fmt.Errorf("bad ProxyJump port in %q", spec)
	}
	h.HostName = host
	return h, nil
}

func (d *Dialer) dialHop(ctx context.Context, via *ssh.Client, h inventory.Host) (*ssh.Client, error) {
	port := h.Port
	if port == 0 {
		port = 22
	}
	addr := net.JoinHostPort(h.HostName, strconv.Itoa(port))
	cfg, err := d.clientConfig(h, addr)
	if err != nil {
		return nil, err
	}
	var raw net.Conn
	if via == nil {
		nd := net.Dialer{Timeout: d.timeout()}
		raw, err = nd.DialContext(ctx, "tcp", addr)
	} else {
		raw, err = via.DialContext(ctx, "tcp", addr)
	}
	if err != nil {
		return nil, err
	}
	raw.SetDeadline(time.Now().Add(d.timeout()))
	c, chans, reqs, err := ssh.NewClientConn(raw, addr, cfg)
	if err != nil {
		raw.Close()
		return nil, explainHostKeyError(err, h)
	}
	raw.SetDeadline(time.Time{})
	return ssh.NewClient(c, chans, reqs), nil
}

func (d *Dialer) clientConfig(h inventory.Host, addr string) (*ssh.ClientConfig, error) {
	var files []string
	for _, f := range d.KnownHosts {
		if _, err := os.Stat(f); err == nil {
			files = append(files, f)
		}
	}
	if len(files) == 0 {
		return nil, errors.New("no known_hosts file found; connect once with plain `ssh` to record the host key")
	}
	db, err := knownhosts.NewDB(files...)
	if err != nil {
		return nil, err
	}
	name := h.User
	if name == "" {
		if u, err := user.Current(); err == nil {
			name = u.Username
		}
	}
	return &ssh.ClientConfig{
		User:              name,
		Auth:              []ssh.AuthMethod{ssh.PublicKeysCallback(d.signers(h))},
		HostKeyCallback:   db.HostKeyCallback(),
		HostKeyAlgorithms: db.HostKeyAlgorithms(addr),
		Timeout:           d.timeout(),
	}, nil
}

// signers offers ssh-agent keys first, then unencrypted key files.
func (d *Dialer) signers(h inventory.Host) func() ([]ssh.Signer, error) {
	return func() ([]ssh.Signer, error) {
		var out []ssh.Signer
		if d.AgentSocket != "" {
			if c, err := net.Dial("unix", d.AgentSocket); err == nil {
				if s, err := agent.NewClient(c).Signers(); err == nil {
					out = append(out, s...)
				}
			}
		}
		files := h.IdentityFiles
		if len(files) == 0 {
			files = d.DefaultKeys
		}
		for _, f := range files {
			b, err := os.ReadFile(f)
			if err != nil {
				continue
			}
			s, err := ssh.ParsePrivateKey(b)
			if err != nil {
				continue // passphrase-protected keys must be loaded into ssh-agent
			}
			out = append(out, s)
		}
		if len(out) == 0 {
			return nil, errors.New("no usable SSH key: load your key with `ssh-add` or set IdentityFile")
		}
		return out, nil
	}
}

func explainHostKeyError(err error, h inventory.Host) error {
	switch {
	case knownhosts.IsHostKeyChanged(err):
		return fmt.Errorf("HOST KEY CHANGED for %s — possible man-in-the-middle, refusing to connect: %w", h.HostName, err)
	case knownhosts.IsHostUnknown(err):
		return fmt.Errorf("host key for %s is not in known_hosts; connect once with `ssh %s` to verify and record it", h.HostName, h.Name)
	}
	return err
}

func (d *Dialer) timeout() time.Duration {
	if d.Timeout > 0 {
		return d.Timeout
	}
	return 15 * time.Second
}

// Result of one remote command.
type Result struct {
	Stdout    string
	Stderr    string
	ExitCode  int
	Truncated bool // output exceeded the capture limit
}

// Exec runs cmd in a new session. ctx cancellation kills the command.
// At most maxBytes of stdout and of stderr are kept.
func Exec(ctx context.Context, c *ssh.Client, cmd string, maxBytes int) (Result, error) {
	s, err := c.NewSession()
	if err != nil {
		return Result{}, err
	}
	defer s.Close()
	stdout, stderr := &capped{max: maxBytes}, &capped{max: maxBytes}
	s.Stdout, s.Stderr = stdout, stderr

	done := make(chan error, 1)
	go func() { done <- s.Run(cmd) }()
	select {
	case <-ctx.Done():
		s.Signal(ssh.SIGKILL)
		s.Close()
		return Result{}, fmt.Errorf("command stopped: %w", ctx.Err())
	case err = <-done:
	}
	res := Result{Stdout: stdout.String(), Stderr: stderr.String(), Truncated: stdout.over || stderr.over}
	var exitErr *ssh.ExitError
	switch {
	case err == nil:
	case errors.As(err, &exitErr):
		res.ExitCode = exitErr.ExitStatus()
	case errors.Is(err, &ssh.ExitMissingError{}):
		res.ExitCode = -1
	default:
		return res, err
	}
	return res, nil
}

type capped struct {
	bytes.Buffer
	max  int
	over bool
}

func (c *capped) Write(p []byte) (int, error) {
	if room := c.max - c.Len(); room < len(p) {
		c.over = true
		if room > 0 {
			c.Buffer.Write(p[:room])
		}
		return len(p), nil
	}
	return c.Buffer.Write(p)
}
