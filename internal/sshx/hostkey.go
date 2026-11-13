package sshx

import (
	"bufio"
	"bytes"
	"context"
	"errors"
	"fmt"
	"net"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"time"

	"github.com/hua-ri/coxswain/internal/filelock"
	"github.com/hua-ri/coxswain/internal/inventory"
	"github.com/skeema/knownhosts"
	"golang.org/x/crypto/ssh"
	"golang.org/x/crypto/ssh/agent"
	xknownhosts "golang.org/x/crypto/ssh/knownhosts"
)

// Host key status, as ScanKey reports it.
const (
	KeyTrusted = "trusted" // in a known_hosts file
	KeyUnknown = "unknown" // in none: the user may trust it
	KeyChanged = "changed" // a different key is recorded: never trusted automatically
)

// ErrKeyChanged is returned by TrustKey for a host whose recorded key
// differs: the old entry has to be removed first (ForgetKey).
var ErrKeyChanged = errors.New("HOST KEY CHANGED")

// HostKey is the key a host presented, and what the known_hosts files
// say about it.
type HostKey struct {
	Host        string `json:"host"`    // the hop it belongs to: the host, or one of its jump hosts
	Address     string `json:"address"` // host:port
	Type        string `json:"type"`
	Fingerprint string `json:"fingerprint"` // SHA256:…
	Status      string `json:"status"`
	// Via names the jump hosts it was reached through.
	Via []string `json:"via,omitempty"`
	// Known are the recorded keys for the address (changed keys only).
	Known []KnownKey `json:"known,omitempty"`
}

// KnownKey is one recorded key.
type KnownKey struct {
	File        string `json:"file"`
	Line        int    `json:"line"`
	Type        string `json:"type"`
	Fingerprint string `json:"fingerprint"`
	// Coxswain: the entry is in Coxswain's own file, so ForgetKey can
	// remove it. Others (~/.ssh/known_hosts) are the user's to edit.
	Coxswain bool `json:"coxswain"`
}

// errScanned stops a handshake once the host key is seen.
var errScanned = errors.New("host key scanned")

// ScanKey connects to h (through its jump hosts) and returns the first
// host key in the chain that is not trusted, or h's own key if every key
// is. It authenticates only to jump hosts whose keys are trusted, never
// to the host whose key it returns.
func (d *Dialer) ScanKey(ctx context.Context, h inventory.Host) (HostKey, error) {
	k, _, err := d.scan(ctx, h)
	return k, err
}

// scan is ScanKey, also returning the key itself.
func (d *Dialer) scan(ctx context.Context, h inventory.Host) (HostKey, ssh.PublicKey, error) {
	hops, err := d.chain(h)
	if err != nil {
		return HostKey{}, nil, err
	}
	db, err := d.db()
	if err != nil {
		return HostKey{}, nil, err
	}
	var ag agent.ExtendedAgent
	if d.AgentSocket != "" && len(hops) > 1 {
		if c, err := net.Dial("unix", d.AgentSocket); err == nil {
			defer c.Close()
			ag = agent.NewClient(c)
		}
	}
	var clients []*ssh.Client
	defer func() {
		for i := len(clients) - 1; i >= 0; i-- {
			clients[i].Close()
		}
	}()
	var via []string
	for i, hop := range hops {
		var prev *ssh.Client
		if n := len(clients); n > 0 {
			prev = clients[n-1]
		}
		k, key, c, err := d.scanHop(ctx, prev, hop, db, i == len(hops)-1, ag)
		if err != nil {
			return HostKey{}, nil, fmt.Errorf("%s: %w", hop.Name, err)
		}
		if c == nil {
			k.Via = slices.Clone(via)
			return k, key, nil
		}
		clients = append(clients, c)
		via = append(via, hop.Name)
	}
	return HostKey{}, nil, errors.New("empty ProxyJump chain")
}

// scanHop does the handshake with one hop. If its key is not trusted, or
// it is the last hop, it returns the key and no client; otherwise it
// authenticates and returns the client to go on through.
func (d *Dialer) scanHop(ctx context.Context, via *ssh.Client, hop inventory.Host, db *knownhosts.HostKeyDB, last bool,
	ag agent.ExtendedAgent) (HostKey, ssh.PublicKey, *ssh.Client, error) {
	addr := hopAddr(hop)
	algos := db.HostKeyAlgorithms(addr)
	check := db.HostKeyCallback()
	for {
		var got ssh.PublicKey
		var verdict error
		cfg, err := d.clientConfig(hop, addr, ag)
		if err != nil {
			return HostKey{}, nil, nil, err
		}
		cfg.HostKeyAlgorithms = algos
		cfg.HostKeyCallback = func(hostname string, remote net.Addr, key ssh.PublicKey) error {
			got, verdict = key, check(hostname, remote, key)
			if verdict != nil || last {
				return errScanned
			}
			return nil
		}
		raw, err := d.rawDial(ctx, via, addr)
		if err != nil {
			return HostKey{}, nil, nil, err
		}
		c, chans, reqs, err := ssh.NewClientConn(raw, addr, cfg)
		if got != nil && (verdict != nil || last) {
			raw.Close()
			return d.hostKey(hop, addr, got, verdict), got, nil, nil
		}
		if err != nil {
			raw.Close()
			// The host no longer has a key of the recorded type: look at
			// what it has instead (it will show as changed).
			if got == nil && algos != nil && strings.Contains(err.Error(), "no common algorithm") {
				algos = nil
				continue
			}
			return HostKey{}, nil, nil, explainHostKeyError(err, hop)
		}
		raw.SetDeadline(time.Time{})
		return HostKey{}, nil, ssh.NewClient(c, chans, reqs), nil
	}
}

func (d *Dialer) hostKey(hop inventory.Host, addr string, key ssh.PublicKey, verdict error) HostKey {
	k := HostKey{Host: hop.Name, Address: addr, Type: key.Type(), Fingerprint: ssh.FingerprintSHA256(key), Status: KeyTrusted}
	var ke *xknownhosts.KeyError
	switch {
	case verdict == nil:
	case knownhosts.IsHostUnknown(verdict):
		k.Status = KeyUnknown
	case errors.As(verdict, &ke):
		k.Status = KeyChanged
		for _, w := range ke.Want {
			k.Known = append(k.Known, KnownKey{File: w.Filename, Line: w.Line, Type: w.Key.Type(),
				Fingerprint: ssh.FingerprintSHA256(w.Key), Coxswain: d.TrustFile != "" && w.Filename == d.TrustFile})
		}
	default:
		k.Status = KeyChanged // anything else the database objects to is not trusted either
	}
	return k
}

// TrustKey scans h again (see ScanKey) and records the key of the first
// untrusted host in its chain in TrustFile, if its fingerprint is the one
// the user confirmed. A changed key is never trusted (ErrKeyChanged).
func (d *Dialer) TrustKey(ctx context.Context, h inventory.Host, fingerprint string) (HostKey, error) {
	if err := d.checkTrustFile(); err != nil {
		return HostKey{}, err
	}
	k, key, err := d.scan(ctx, h)
	if err != nil {
		return HostKey{}, err
	}
	switch k.Status {
	case KeyTrusted:
		return k, nil
	case KeyChanged:
		return k, fmt.Errorf("%w for %s (%s): it now presents %s, but a different key is recorded%s. "+
			"This can be a man-in-the-middle attack. If the host was reinstalled, confirm the new fingerprint, "+
			"remove the old key, then trust the new one", ErrKeyChanged, k.Host, k.Address, k.Fingerprint, knownWhere(k.Known))
	}
	if strings.TrimSpace(fingerprint) != k.Fingerprint {
		return k, fmt.Errorf("the key of %s (%s) is %s, which does not match the fingerprint you confirmed (%s); "+
			"nothing was trusted. Test the connection again", k.Host, k.Address, k.Fingerprint, fingerprint)
	}
	line := knownhosts.Line([]string{k.Address}, key) + "\n"
	err = d.editTrustFile(func(b []byte) []byte {
		if len(b) > 0 && !bytes.HasSuffix(b, []byte("\n")) {
			b = append(b, '\n')
		}
		return append(b, line...)
	})
	if err != nil {
		return HostKey{}, err
	}
	k.Status = KeyTrusted
	return k, nil
}

// ForgetKey removes address's entries from TrustFile and returns how many
// it removed. address must be on the way to h (h's own address if
// empty). Entries in other files (~/.ssh/known_hosts) are left alone.
func (d *Dialer) ForgetKey(h inventory.Host, address string) (int, error) {
	if err := d.checkTrustFile(); err != nil {
		return 0, err
	}
	hops, err := d.chain(h)
	if err != nil {
		return 0, err
	}
	if address == "" {
		address = hopAddr(h)
	}
	ok := false
	for _, hop := range hops {
		ok = ok || hopAddr(hop) == address
	}
	if !ok {
		return 0, fmt.Errorf("%s is not on the way to %s", address, h.Name)
	}
	want := knownhosts.Normalize(address)
	removed := 0
	err = d.editTrustFile(func(b []byte) []byte {
		var out bytes.Buffer
		sc := bufio.NewScanner(bytes.NewReader(b))
		sc.Buffer(nil, 1<<20)
		for sc.Scan() {
			line := sc.Text()
			fields := strings.Fields(line)
			if len(fields) >= 2 && !strings.HasPrefix(fields[0], "#") && !strings.HasPrefix(fields[0], "@") &&
				slices.Contains(strings.Split(fields[0], ","), want) {
				removed++
				continue
			}
			out.WriteString(line + "\n")
		}
		return out.Bytes()
	})
	return removed, err
}

func (d *Dialer) checkTrustFile() error {
	if d.TrustFile == "" {
		return errors.New("Coxswain has no known_hosts file of its own here; host keys cannot be trusted")
	}
	for dir := filepath.Dir(d.TrustFile); ; dir = filepath.Dir(dir) {
		if filepath.Base(dir) == ".ssh" {
			return fmt.Errorf("refusing to write %s: files under ~/.ssh are yours, Coxswain only reads them", d.TrustFile)
		}
		if parent := filepath.Dir(dir); parent == dir {
			return nil
		}
	}
}

// editTrustFile rewrites TrustFile (mode 0600, its directory 0700) under
// its lock.
func (d *Dialer) editTrustFile(edit func([]byte) []byte) error {
	if err := os.MkdirAll(filepath.Dir(d.TrustFile), 0o700); err != nil {
		return err
	}
	return filelock.With(d.TrustFile+".lock", func() error {
		b, err := os.ReadFile(d.TrustFile)
		if err != nil && !errors.Is(err, os.ErrNotExist) {
			return err
		}
		return filelock.WriteAtomic(d.TrustFile, edit(b))
	})
}

func knownWhere(known []KnownKey) string {
	var parts []string
	for _, k := range known {
		where := fmt.Sprintf("%s line %d", k.File, k.Line)
		if !k.Coxswain {
			where += " (your file: Coxswain does not edit it; remove the line with ssh-keygen -R)"
		}
		parts = append(parts, where)
	}
	if len(parts) == 0 {
		return ""
	}
	return " in " + strings.Join(parts, ", ")
}
