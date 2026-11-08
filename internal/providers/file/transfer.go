package file

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"hash"
	"io"
	"io/fs"
	"os"
	"path"
	"path/filepath"
	"regexp"
	"strings"

	"github.com/hua-ri/coxswain/internal/action"
	"github.com/hua-ri/coxswain/internal/inventory"
	"github.com/hua-ri/coxswain/internal/sshx"
	"github.com/hua-ri/coxswain/internal/transfer"
)

// chunk is how much a transfer copies per step; progress and
// cancellation are checked between steps.
const chunk = 256 << 10

// TransferOutput is the result of file.upload and file.download.
type TransferOutput struct {
	transfer.Transfer
	HostNote string            `json:"host_note,omitempty"`
	HostTags map[string]string `json:"host_tags,omitempty"`
}

// localPath expands ~ in a local path.
func (p *Provider) localPath(s string) string {
	switch {
	case s == "~":
		return p.UserHome
	case strings.HasPrefix(s, "~/"):
		return filepath.Join(p.UserHome, s[2:])
	}
	return s
}

type uploadInput struct {
	Host       string `json:"host"`
	LocalPath  string `json:"local_path"`
	RemotePath string `json:"remote_path"`
	Resume     bool   `json:"resume"`
}

var transferHint = "The user sees its progress in the Coxswain desktop app and can cancel it; " +
	"a cancelled or failed transfer can be continued with resume=true."

func (p *Provider) upload() *action.Action {
	return &action.Action{
		ID:      "file.upload",
		Summary: "Upload a local file to a saved SSH host",
		Description: "Copy a file from this machine (absolute path or ~/…) to a remote path over SFTP, replacing it. " +
			"Returns the size and SHA-256; the remote copy is verified with sha256sum when the host has it. " +
			"resume=true continues a partial upload (a smaller remote file) instead of starting over. " + transferHint + " " +
			"Hosts tagged env=prod are production: confirm with the user before changing them. Respect the host note.",
		Input: map[string]any{
			"type": "object",
			"properties": map[string]any{
				"host":        hostProperty,
				"local_path":  map[string]any{"type": "string", "description": "file on this machine: absolute or ~/…"},
				"remote_path": pathProperty,
				"resume":      map[string]any{"type": "boolean", "description": "continue a partial upload"},
			},
			"required": []string{"host", "local_path", "remote_path"},
		},
		Positional:  []string{"host", "local_path", "remote_path"},
		Annotations: action.Annotations{Destructive: true, OpenWorld: true},
		Label: func(_ context.Context, raw json.RawMessage) (action.Assessment, error) {
			var in uploadInput
			if err := action.Decode(raw, &in); err != nil {
				return action.Assessment{}, err
			}
			if _, err := remotePath(in.RemotePath); err != nil {
				return action.Assessment{}, err
			}
			h, err := p.Hosts.Get(in.Host)
			if err != nil {
				return action.Assessment{}, err
			}
			// Uploading a credential store sends it away.
			risk := max(writeRisk(in.RemotePath), writeRisk(in.LocalPath), writeRisk(p.localPath(in.LocalPath)))
			return action.Assessment{Risk: risk, Targets: []action.Target{hostTarget(h)},
				Preview: fmt.Sprintf("upload %s to %s:%s", in.LocalPath, h.Name, in.RemotePath)}, nil
		},
		Run: func(ctx context.Context, raw json.RawMessage) (any, error) {
			var in uploadInput
			if err := action.Decode(raw, &in); err != nil {
				return nil, err
			}
			return p.runUpload(ctx, in)
		},
	}
}

func (p *Provider) runUpload(ctx context.Context, in uploadInput) (any, error) {
	rp, err := remotePath(in.RemotePath)
	if err != nil {
		return nil, err
	}
	src := p.localPath(in.LocalPath)
	if !filepath.IsAbs(src) {
		return nil, fmt.Errorf("local_path must be absolute or start with ~/, not %q", in.LocalPath)
	}
	lf, err := os.Open(src)
	if err != nil {
		return nil, err
	}
	defer lf.Close()
	lfi, err := lf.Stat()
	if err != nil {
		return nil, err
	}
	if !lfi.Mode().IsRegular() {
		return nil, fmt.Errorf("%s is not a regular file", in.LocalPath)
	}
	h, c, conn, release, err := p.sessionConn(ctx, in.Host)
	if err != nil {
		return nil, err
	}
	broken := false
	defer func() { release(broken) }()

	var offset int64
	rfi, err := c.Stat(rp)
	switch {
	case err == nil && rfi.IsDir():
		return nil, fmt.Errorf("%s is a directory", in.RemotePath)
	case err == nil && in.Resume && rfi.Size() < lfi.Size():
		offset = rfi.Size()
	case err != nil && !errors.Is(err, fs.ErrNotExist):
		broken = failed(err)
		return nil, err
	}
	created := err != nil

	t := transfer.Transfer{Direction: transfer.Upload, Host: h.Name, LocalPath: src, RemotePath: in.RemotePath, Owner: owner(ctx)}
	res, err := p.Transfers.Run(ctx, t, []action.Target{hostTarget(h)}, func(ctx context.Context, pr *transfer.Progress) error {
		pr.Start(offset, lfi.Size())
		defer context.AfterFunc(ctx, func() { c.Close() })() // cancelling aborts a call in progress
		sum := sha256.New()
		if _, err := io.CopyN(sum, lf, offset); err != nil {
			return err
		}
		flags := os.O_WRONLY | os.O_CREATE
		if offset == 0 {
			flags |= os.O_TRUNC
		}
		rf, err := c.OpenFile(rp, flags)
		if err != nil {
			return err
		}
		if _, err := rf.Seek(offset, io.SeekStart); err != nil {
			rf.Close()
			return err
		}
		err = p.copy(ctx, rf, lf, sum, pr)
		if cerr := rf.Close(); err == nil {
			err = cerr
		}
		if err != nil {
			broken = failed(err) || ctx.Err() != nil
			return err
		}
		if created {
			c.Chmod(rp, lfi.Mode().Perm())
		}
		local := hex.EncodeToString(sum.Sum(nil))
		pr.Update(func(t *transfer.Transfer) { t.SHA256 = local })
		remote, ok := remoteSHA256(ctx, conn, rp)
		if !ok {
			return nil // no sha256sum on the host: unverified
		}
		match := remote == local
		pr.Update(func(t *transfer.Transfer) { t.Verified = &match })
		if !match {
			return fmt.Errorf("checksum mismatch: local %s, remote %s (upload again without resume)", local, remote)
		}
		return nil
	})
	if err != nil {
		return nil, err
	}
	return TransferOutput{Transfer: res, HostNote: h.Note, HostTags: h.Tags}, nil
}

var sha256Line = regexp.MustCompile(`^([0-9a-f]{64})\s`)

// remoteSHA256 asks the host for a file's SHA-256 (sha256sum, or shasum
// on macOS); ok is false if it cannot tell.
func remoteSHA256(ctx context.Context, conn *sshx.Conn, rp string) (string, bool) {
	q := shellQuote(rp)
	res, err := sshx.Exec(ctx, conn.Client, "sha256sum -- "+q+" 2>/dev/null || shasum -a 256 -- "+q+" 2>/dev/null", 4096)
	if err != nil || res.ExitCode != 0 {
		return "", false
	}
	m := sha256Line.FindStringSubmatch(res.Stdout)
	if m == nil {
		return "", false
	}
	return m[1], true
}

func shellQuote(s string) string { return "'" + strings.ReplaceAll(s, "'", `'\''`) + "'" }

type downloadInput struct {
	Host       string `json:"host"`
	RemotePath string `json:"remote_path"`
	LocalPath  string `json:"local_path"`
	Resume     bool   `json:"resume"`
}

func (p *Provider) download() *action.Action {
	return &action.Action{
		ID:      "file.download",
		Summary: "Download a file from a saved SSH host",
		Description: "Copy a remote file to this machine over SFTP, by default to ~/.coxswain/downloads/<host>/<file name>; " +
			"local_path may name another file there (relative to the host's download directory). " +
			"Returns the local path, size and SHA-256, not the content: use it for large or binary files (logs, dumps) " +
			"and inspect the local copy. resume=true continues a partial download. " + transferHint,
		Input: map[string]any{
			"type": "object",
			"properties": map[string]any{
				"host":        hostProperty,
				"remote_path": pathProperty,
				"local_path":  map[string]any{"type": "string", "description": "where to save it, relative to the host's download directory"},
				"resume":      map[string]any{"type": "boolean", "description": "continue a partial download"},
			},
			"required": []string{"host", "remote_path"},
		},
		Positional:  []string{"host", "remote_path"},
		Annotations: action.Annotations{ReadOnly: true, OpenWorld: true},
		Label: func(_ context.Context, raw json.RawMessage) (action.Assessment, error) {
			var in downloadInput
			if err := action.Decode(raw, &in); err != nil {
				return action.Assessment{}, err
			}
			if _, err := remotePath(in.RemotePath); err != nil {
				return action.Assessment{}, err
			}
			h, err := p.Hosts.Get(in.Host)
			if err != nil {
				return action.Assessment{}, err
			}
			return action.Assessment{Risk: action.Read, Targets: []action.Target{hostTarget(h)},
				Preview: fmt.Sprintf("download %s:%s", h.Name, in.RemotePath)}, nil
		},
		Run: func(ctx context.Context, raw json.RawMessage) (any, error) {
			var in downloadInput
			if err := action.Decode(raw, &in); err != nil {
				return nil, err
			}
			return p.runDownload(ctx, in)
		},
	}
}

// downloadPath is where a download of rp from h goes. Agents may only
// write inside the download directory; the user may name any absolute
// path.
func (p *Provider) downloadPath(ctx context.Context, h inventory.Host, rp, local string) (string, error) {
	base := filepath.Join(p.DownloadDir, strings.NewReplacer("/", "_", `\`, "_").Replace(h.Name))
	if h.Name == "." || h.Name == ".." {
		base = filepath.Join(p.DownloadDir, "_"+h.Name)
	}
	if local == "" {
		name := path.Base(rp)
		if name == "." || name == "/" || name == ".." {
			return "", fmt.Errorf("cannot name a local file after %q; give local_path", rp)
		}
		return filepath.Join(base, name), nil
	}
	expanded := p.localPath(local)
	human := false
	if a, ok := action.ActorFrom(ctx); ok && a.Kind == action.Human {
		human = true
	}
	if filepath.IsAbs(expanded) {
		if human {
			return filepath.Clean(expanded), nil
		}
		return "", fmt.Errorf("agents download only into the download directory (%s); give local_path relative to it", base)
	}
	out := filepath.Join(base, expanded)
	if rel, err := filepath.Rel(p.DownloadDir, out); err != nil || rel == ".." || strings.HasPrefix(rel, ".."+string(filepath.Separator)) {
		return "", fmt.Errorf("local_path must stay inside the download directory (%s)", base)
	}
	return out, nil
}

func (p *Provider) runDownload(ctx context.Context, in downloadInput) (any, error) {
	rp, err := remotePath(in.RemotePath)
	if err != nil {
		return nil, err
	}
	h, err := p.Hosts.Get(in.Host)
	if err != nil {
		return nil, err
	}
	dst, err := p.downloadPath(ctx, h, rp, in.LocalPath)
	if err != nil {
		return nil, err
	}
	h, c, release, err := p.session(ctx, in.Host)
	if err != nil {
		return nil, err
	}
	broken := false
	defer func() { release(broken) }()
	rfi, err := c.Stat(rp)
	if err != nil {
		broken = failed(err)
		return nil, fmt.Errorf("download %s: %w", in.RemotePath, err)
	}
	if !rfi.Mode().IsRegular() {
		return nil, fmt.Errorf("%s is not a regular file", in.RemotePath)
	}
	if err := os.MkdirAll(filepath.Dir(dst), 0o700); err != nil {
		return nil, err
	}
	var offset int64
	if in.Resume {
		if lfi, err := os.Stat(dst); err == nil && lfi.Mode().IsRegular() && lfi.Size() < rfi.Size() {
			offset = lfi.Size()
		}
	}

	t := transfer.Transfer{Direction: transfer.Download, Host: h.Name, LocalPath: dst, RemotePath: in.RemotePath, Owner: owner(ctx)}
	res, err := p.Transfers.Run(ctx, t, []action.Target{hostTarget(h)}, func(ctx context.Context, pr *transfer.Progress) error {
		pr.Start(offset, rfi.Size())
		defer context.AfterFunc(ctx, func() { c.Close() })() // cancelling aborts a call in progress
		flags := os.O_RDWR | os.O_CREATE
		if offset == 0 {
			flags |= os.O_TRUNC
		}
		lf, err := os.OpenFile(dst, flags, 0o600)
		if err != nil {
			return err
		}
		defer lf.Close()
		sum := sha256.New()
		if _, err := io.CopyN(sum, lf, offset); err != nil {
			return err
		}
		rf, err := c.Open(rp)
		if err != nil {
			broken = failed(err)
			return err
		}
		defer rf.Close()
		if _, err := rf.Seek(offset, io.SeekStart); err != nil {
			return err
		}
		if err := p.copy(ctx, lf, rf, sum, pr); err != nil {
			broken = failed(err) || ctx.Err() != nil
			return err
		}
		if err := lf.Close(); err != nil {
			return err
		}
		s := hex.EncodeToString(sum.Sum(nil))
		pr.Update(func(t *transfer.Transfer) { t.SHA256 = s })
		return nil
	})
	if err != nil {
		return nil, err
	}
	return TransferOutput{Transfer: res, HostNote: h.Note, HostTags: h.Tags}, nil
}

// copy copies src to dst in chunks, hashing and counting what it copies,
// until src ends or ctx does.
func (p *Provider) copy(ctx context.Context, dst io.Writer, src io.Reader, sum hash.Hash, pr *transfer.Progress) error {
	buf := make([]byte, chunk)
	for {
		if p.pace != nil {
			p.pace(ctx)
		}
		if err := ctx.Err(); err != nil {
			return err
		}
		n, rerr := io.ReadFull(src, buf)
		if n > 0 {
			if _, err := dst.Write(buf[:n]); err != nil {
				return err
			}
			sum.Write(buf[:n])
			pr.Add(int64(n))
		}
		switch {
		case rerr == io.EOF, rerr == io.ErrUnexpectedEOF:
			return ctx.Err()
		case rerr != nil:
			return rerr
		}
	}
}

// owner is who a transfer belongs to: the calling client, not one of its
// sessions.
func owner(ctx context.Context) action.Actor {
	a, _ := action.ActorFrom(ctx)
	a.Session = ""
	return a
}
