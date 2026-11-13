// Package sshx opens SSH connections the way OpenSSH users expect:
// ssh-agent and key files, strict known_hosts, ProxyJump chains.
package sshx

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"io"
	"net"
	"os"
	"os/user"
	"slices"
	"strconv"
	"strings"
	"time"

	"github.com/hua-ri/coxswain/internal/inventory"
	"github.com/skeema/knownhosts"
	"golang.org/x/crypto/ssh"
	"golang.org/x/crypto/ssh/agent"
)

type Dialer struct {
	Hosts      *inventory.Store // resolves ProxyJump aliases
	KnownHosts []string         // known_hosts files; missing files are skipped
	// TrustFile is Coxswain's own known_hosts (~/.coxswain/known_hosts),
	// the only file host keys are ever written to (see TrustKey). It is
	// read like the KnownHosts files. The user's ~/.ssh files are never
	// written.
	TrustFile   string
	DefaultKeys []string // tried when a host lists no IdentityFile
	AgentSocket string   // SSH_AUTH_SOCK; empty disables the agent
	Timeout     time.Duration
}

// Conn is a connection to a host plus the jump connections under it and
// the ssh-agent connection used to sign for them.
type Conn struct {
	*ssh.Client
	jumps []*ssh.Client
	agent io.Closer
}

func (c *Conn) Close() error {
	err := c.Client.Close()
	for i := len(c.jumps) - 1; i >= 0; i-- {
		c.jumps[i].Close()
	}
	if c.agent != nil {
		c.agent.Close()
	}
	return err
}

// maxHops bounds ProxyJump chains.
const maxHops = 8

// Dial connects to h, through h.ProxyJump if set. One ssh-agent
// connection serves all hops and is closed with the returned Conn.
func (d *Dialer) Dial(ctx context.Context, h inventory.Host) (*Conn, error) {
	hops, err := d.chain(h)
	if err != nil {
		return nil, err
	}
	var ag agent.ExtendedAgent
	var agConn net.Conn
	if d.AgentSocket != "" {
		if c, err := net.Dial("unix", d.AgentSocket); err == nil {
			agConn, ag = c, agent.NewClient(c)
		}
	}
	var clients []*ssh.Client
	for _, hop := range hops {
		var via *ssh.Client
		if n := len(clients); n > 0 {
			via = clients[n-1]
		}
		c, err := d.dialHop(ctx, via, hop, ag)
		if err != nil {
			for i := len(clients) - 1; i >= 0; i-- {
				clients[i].Close()
			}
			if agConn != nil {
				agConn.Close()
			}
			return nil, fmt.Errorf("%s: %w", hop.Name, err)
		}
		clients = append(clients, c)
	}
	n := len(clients)
	conn := &Conn{Client: clients[n-1], jumps: clients[:n-1]}
	if agConn != nil {
		conn.agent = agConn
	}
	return conn, nil
}

// chain returns the hops to reach h, h last. A host that appears twice
// is a loop.
func (d *Dialer) chain(h inventory.Host) ([]inventory.Host, error) {
	hops, err := d.chainVia(h, []string{h.Name})
	if err != nil {
		return nil, err
	}
	seen := map[string]bool{}
	var names []string
	for _, hop := range hops {
		names = append(names, hop.Name)
		if seen[hop.Name] {
			return nil, fmt.Errorf("ProxyJump loop: %s", strings.Join(names, " -> "))
		}
		seen[hop.Name] = true
	}
	return hops, nil
}

// chainVia resolves h's jumps; path is the chain of names that led to h.
func (d *Dialer) chainVia(h inventory.Host, path []string) ([]inventory.Host, error) {
	if len(path) > maxHops+1 {
		return nil, fmt.Errorf("ProxyJump chain longer than %d hops", maxHops)
	}
	var hops []inventory.Host
	if h.ProxyJump != "" {
		for _, spec := range strings.Split(h.ProxyJump, ",") {
			hop, err := d.resolveJump(strings.TrimSpace(spec))
			if err != nil {
				return nil, err
			}
			if slices.Contains(path, hop.Name) {
				return nil, fmt.Errorf("ProxyJump loop: %s -> %s", strings.Join(path, " -> "), hop.Name)
			}
			if hop.ProxyJump != "" && len(hops) == 0 {
				inner, err := d.chainVia(hop, append(slices.Clip(path), hop.Name))
				if err != nil {
					return nil, err
				}
				hops = append(hops, inner...)
				continue
			}
			hops = append(hops, hop)
		}
	}
	if len(hops) > maxHops {
		return nil, fmt.Errorf("ProxyJump chain longer than %d hops", maxHops)
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

func (d *Dialer) dialHop(ctx context.Context, via *ssh.Client, h inventory.Host, ag agent.ExtendedAgent) (*ssh.Client, error) {
	addr := hopAddr(h)
	cfg, err := d.clientConfig(h, addr, ag)
	if err != nil {
		return nil, err
	}
	raw, err := d.rawDial(ctx, via, addr)
	if err != nil {
		return nil, err
	}
	c, chans, reqs, err := ssh.NewClientConn(raw, addr, cfg)
	if err != nil {
		raw.Close()
		return nil, explainHostKeyError(err, h)
	}
	raw.SetDeadline(time.Time{})
	return ssh.NewClient(c, chans, reqs), nil
}

// hopAddr is host:port of a hop (port 22 if unset).
func hopAddr(h inventory.Host) string {
	port := h.Port
	if port == 0 {
		port = 22
	}
	return net.JoinHostPort(h.HostName, strconv.Itoa(port))
}

// rawDial opens the TCP connection to addr, through via if set, with the
// handshake deadline set.
func (d *Dialer) rawDial(ctx context.Context, via *ssh.Client, addr string) (net.Conn, error) {
	var raw net.Conn
	var err error
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
	return raw, nil
}

// knownFiles are the known_hosts files that exist: KnownHosts, then the
// TrustFile (once).
func (d *Dialer) knownFiles() []string {
	var files []string
	for _, f := range append(slices.Clone(d.KnownHosts), d.TrustFile) {
		if f == "" || slices.Contains(files, f) {
			continue
		}
		if _, err := os.Stat(f); err == nil {
			files = append(files, f)
		}
	}
	return files
}

// db loads the known host keys. Without any file, every host is unknown.
func (d *Dialer) db() (*knownhosts.HostKeyDB, error) {
	return knownhosts.NewDB(d.knownFiles()...)
}

func (d *Dialer) clientConfig(h inventory.Host, addr string, ag agent.ExtendedAgent) (*ssh.ClientConfig, error) {
	db, err := d.db()
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
		Auth:              []ssh.AuthMethod{ssh.PublicKeysCallback(d.signers(h, ag))},
		HostKeyCallback:   db.HostKeyCallback(),
		HostKeyAlgorithms: db.HostKeyAlgorithms(addr),
		Timeout:           d.timeout(),
	}, nil
}

// signers offers ssh-agent keys first (ag may be nil), then unencrypted
// key files.
func (d *Dialer) signers(h inventory.Host, ag agent.ExtendedAgent) func() ([]ssh.Signer, error) {
	return func() ([]ssh.Signer, error) {
		var out []ssh.Signer
		if ag != nil {
			if s, err := ag.Signers(); err == nil {
				out = append(out, s...)
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
		return fmt.Errorf("HOST KEY CHANGED for %s — possible man-in-the-middle, refusing to connect. "+
			"If the host was reinstalled, confirm its new fingerprint with whoever runs it, then remove the old key "+
			"(cox desktop: Edit host → Test connection, or `cox ssh hosts forget_key %s`): %w", h.HostName, h.Name, err)
	case knownhosts.IsHostUnknown(err):
		return fmt.Errorf("host key for %s is not trusted yet: check its fingerprint and trust it in cox desktop "+
			"(Edit host → Test connection), or with `cox ssh hosts scan_key %s` and `cox ssh hosts trust_key %s --fingerprint SHA256:…`",
			h.HostName, h.Name, h.Name)
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
	res.ExitCode, err = exitCode(err)
	return res, err
}

// exitCode maps Session.Run's error to the remote exit code: -1 if the
// server sent no exit status; other errors are returned.
func exitCode(err error) (int, error) {
	var exitErr *ssh.ExitError
	switch {
	case err == nil:
	case errors.As(err, &exitErr):
		return exitErr.ExitStatus(), nil
	case errors.As(err, new(*ssh.ExitMissingError)):
		return -1, nil
	default:
		return 0, err
	}
	return 0, nil
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
