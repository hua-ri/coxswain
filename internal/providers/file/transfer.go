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
	"os"
	"path"
	"path/filepath"
	"regexp"
	"strconv"
	"strings"

	"github.com/hua-ri/coxswain/internal/action"
	"github.com/hua-ri/coxswain/internal/inventory"
	"github.com/hua-ri/coxswain/internal/policy"
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

// localSecrets are credential stores and browser data under the user's
// home (and its Library on macOS).
var localSecrets = []string{
	".netrc", ".git-credentials", ".docker", ".config/gh", ".config/gcloud", ".config/hub", ".azure", ".npmrc",
	".pypirc", ".cargo/credentials", ".cargo/credentials.toml", ".password-store", ".terraform.d", ".vault-token",
	".pgpass", ".my.cnf", ".aws", ".kube", ".gnupg", ".ssh",
	"Library/Keychains", "Library/Cookies", "Library/Safari", "Library/Application Support/Google/Chrome",
	"Library/Application Support/Firefox", "Library/Application Support/BraveSoftware",
	"Library/Application Support/Microsoft Edge", "Library/Application Support/com.apple.TCC",
}

// under reports whether p is root or inside it.
func under(p, root string) bool {
	if root == "" {
		return false
	}
	rel, err := filepath.Rel(root, p)
	return err == nil && rel != ".." && !strings.HasPrefix(rel, ".."+string(filepath.Separator))
}

// resolved returns p and, if different, p with symlinks resolved.
func resolved(p string) []string {
	out := []string{filepath.Clean(p)}
	if r, err := filepath.EvalSymlinks(p); err == nil && r != out[0] {
		out = append(out, r)
	}
	return out
}

// sensitiveLocal reports whether uploading the local file s would send
// a secret away: a credential store, browser data, a protected path or
// anything of Coxswain's own (but its upload and download directories).
// Symlinks are followed.
func (p *Provider) sensitiveLocal(s string) bool {
	if policy.IsProtected(s) {
		return true
	}
	for _, path := range resolved(p.localPath(s)) {
		if policy.IsProtected(path) {
			return true
		}
		for _, cox := range resolved(p.CoxHome) {
			if under(path, cox) && !under(path, filepath.Join(cox, "uploads")) && !under(path, filepath.Join(cox, "downloads")) {
				return true
			}
		}
		for _, home := range resolved(p.UserHome) {
			for _, rel := range localSecrets {
				if under(path, filepath.Join(home, rel)) {
					return true
				}
			}
		}
	}
	return false
}

// openSource opens the local file to upload and returns its path. People
// may name any absolute (or ~/) path. Agents may upload only from
// UploadDir (a path relative to it, or inside it); the file is opened
// through an os.Root there, so symlinks cannot lead out of it. Agents
// copy files there with their own tools, under their own permission
// prompts.
func (p *Provider) openSource(ctx context.Context, s string) (*os.File, string, error) {
	src := p.localPath(s)
	if a, ok := action.ActorFrom(ctx); ok && a.Kind == action.Human {
		if !filepath.IsAbs(src) {
			return nil, "", fmt.Errorf("local_path must be absolute or start with ~/, not %q", s)
		}
		f, err := os.Open(src)
		return f, src, err
	}
	refuse := fmt.Errorf("agents may upload only files in %s: copy the file there with your own tools, "+
		"then upload it from there (local_path relative to that directory)", p.UploadDir)
	if p.UploadDir == "" {
		return nil, "", refuse
	}
	if err := os.MkdirAll(p.UploadDir, 0o700); err != nil {
		return nil, "", err
	}
	os.Chmod(p.UploadDir, 0o700)
	rel := src
	if filepath.IsAbs(src) {
		var err error
		if rel, err = filepath.Rel(p.UploadDir, filepath.Clean(src)); err != nil {
			return nil, "", refuse
		}
	}
	rel = filepath.Clean(rel)
	if rel == ".." || strings.HasPrefix(rel, ".."+string(filepath.Separator)) {
		return nil, "", refuse
	}
	root, err := os.OpenRoot(p.UploadDir)
	if err != nil {
		return nil, "", err
	}
	defer root.Close()
	f, err := root.Open(rel)
	if err != nil {
		var pe *os.PathError
		if errors.As(err, &pe) && strings.Contains(pe.Err.Error(), "escapes") {
			return nil, "", refuse
		}
		return nil, "", fmt.Errorf("%w (%s)", err, refuse)
	}
	return f, filepath.Join(p.UploadDir, rel), nil
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
		Description: "Copy a file from this machine to a remote path over SFTP, replacing it. Agents may upload only files in " +
			"~/.coxswain/uploads/ (local_path relative to it): copy the file there first. " +
			"Returns the size and SHA-256; the remote copy is verified with sha256sum when the host has it. " +
			"resume=true continues a partial upload (a smaller remote file) instead of starting over. " + transferHint + " " +
			"Hosts tagged env=prod are production: confirm with the user before changing them. Respect the host note.",
		Input: map[string]any{
			"type": "object",
			"properties": map[string]any{
				"host":        hostProperty,
				"local_path":  map[string]any{"type": "string", "description": "file in ~/.coxswain/uploads/ (relative to it); people may give any absolute or ~/ path"},
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
			risk := writeRisk(in.RemotePath)
			if p.sensitiveLocal(in.LocalPath) {
				risk = action.Destructive
			}
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
	lf, src, err := p.openSource(ctx, in.LocalPath)
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

	dst, fi, err := target(c, rp)
	if err != nil {
		broken = failed(err)
		return nil, err
	}
	if fi != nil && fi.IsDir() {
		return nil, fmt.Errorf("%s is a directory", in.RemotePath)
	}
	mode := lfi.Mode().Perm()
	if fi != nil {
		mode = fi.Mode().Perm()
	}
	// The upload goes to a private temporary file next to the target,
	// renamed over it once complete and verified. Its name is fixed, so a
	// cancelled upload can be resumed.
	tmp := path.Join(path.Dir(dst), "."+path.Base(dst)+".cox-upload")
	// One upload at a time per target: they would share the temporary file.
	key := path.Clean(dst)
	if dir, err := c.RealPath(path.Dir(dst)); err == nil {
		key = path.Join(dir, path.Base(dst))
	}
	unclaim, err := p.Transfers.Claim(h.Name + "\x00" + key)
	if err != nil {
		return nil, fmt.Errorf("%s on %s is already being uploaded; wait for that upload or cancel it", in.RemotePath, h.Name)
	}
	defer unclaim()
	var offset int64
	if in.Resume {
		if tfi, err := c.Lstat(tmp); err == nil {
			if !tfi.Mode().IsRegular() {
				return nil, fmt.Errorf("cannot resume: %s is not a regular file; delete it and upload again without resume", tmp)
			}
			if tfi.Size() < lfi.Size() {
				offset = tfi.Size()
			}
		}
	}
	if offset > 0 {
		if err := samePrefix(ctx, conn, tmp, lf, offset); err != nil {
			return nil, err
		}
	} else if err := c.Remove(tmp); err != nil && !errors.Is(err, os.ErrNotExist) && failed(err) {
		broken = true
		return nil, err
	}

	t := transfer.Transfer{Direction: transfer.Upload, Host: h.Name, LocalPath: src, RemotePath: in.RemotePath, Owner: owner(ctx)}
	res, err := p.Transfers.Run(ctx, t, []action.Target{hostTarget(h)}, func(ctx context.Context, pr *transfer.Progress) error {
		pr.Start(offset, lfi.Size())
		defer context.AfterFunc(ctx, func() { c.Close() })() // cancelling aborts a call in progress
		sum := sha256.New()
		if _, err := io.CopyN(sum, lf, offset); err != nil {
			return err
		}
		// A fresh upload creates the temporary file exclusively (whatever
		// was planted there was removed above); a resumed one continues
		// the regular file checked above.
		flags := os.O_WRONLY | os.O_CREATE | os.O_EXCL
		if offset > 0 {
			flags = os.O_WRONLY
		}
		rf, err := createPrivate(c, tmp, flags)
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
		local := hex.EncodeToString(sum.Sum(nil))
		pr.Update(func(t *transfer.Transfer) { t.SHA256 = local })
		if remote, ok := remoteSHA256(ctx, conn, tmp, -1); ok {
			match := remote == local
			pr.Update(func(t *transfer.Transfer) { t.Verified = &match })
			if !match {
				return fmt.Errorf("checksum mismatch: local %s, remote %s; %s is unchanged (upload again without resume)",
					local, remote, in.RemotePath)
			}
		} // else: no sha256sum on the host, unverified
		if err := c.Chmod(tmp, mode); err != nil {
			return err
		}
		return c.PosixRename(tmp, dst)
	})
	if err != nil {
		return nil, fmt.Errorf("%w; %s on %s is unchanged; the partial upload is %s (upload again with resume=true to continue it, or delete it)",
			err, in.RemotePath, h.Name, tmp)
	}
	return TransferOutput{Transfer: res, HostNote: h.Note, HostTags: h.Tags}, nil
}

// samePrefix checks that the first n bytes of the local file are those
// of the remote one, so a download can continue it.
// (For an upload, rp is the partial upload and local the source.)
func samePrefix(ctx context.Context, conn *sshx.Conn, rp string, local *os.File, n int64) error {
	sum := sha256.New()
	if _, err := io.Copy(sum, io.NewSectionReader(local, 0, n)); err != nil {
		return err
	}
	remote, ok := remoteSHA256(ctx, conn, rp, n)
	switch {
	case !ok:
		return fmt.Errorf("cannot check the partial file against %s (no head/sha256sum on the host); try again without resume", rp)
	case remote != hex.EncodeToString(sum.Sum(nil)):
		return fmt.Errorf("the partial file does not match %s; try again without resume", rp)
	}
	return nil
}

var sha256Line = regexp.MustCompile(`^([0-9a-f]{64})\s`)

// remoteSHA256 asks the host for the SHA-256 of a file, or of its first
// n bytes if n >= 0 (sha256sum, or shasum on macOS); ok is false if it
// cannot tell. The script runs under sh, whatever the login shell is.
func remoteSHA256(ctx context.Context, conn *sshx.Conn, rp string, n int64) (string, bool) {
	script := `sha256sum -- "$1" 2>/dev/null || shasum -a 256 -- "$1"`
	if n >= 0 {
		script = `test -f "$1" && head -c "$2" -- "$1" | { sha256sum 2>/dev/null || shasum -a 256; }`
	}
	cmd := "sh -c " + shellQuote(script) + " sh " + shellQuote(rp) + " " + strconv.FormatInt(max(n, 0), 10)
	res, err := sshx.Exec(ctx, conn.Client, cmd, 4096)
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
		Annotations: action.Annotations{OpenWorld: true}, // reads the host, writes a local file
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
	if rel, err := filepath.Rel(base, out); err != nil || rel == ".." || strings.HasPrefix(rel, ".."+string(filepath.Separator)) {
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
	h, c, conn, release, err := p.sessionConn(ctx, in.Host)
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
	if offset > 0 {
		lf, err := os.Open(dst)
		if err != nil {
			return nil, err
		}
		err = samePrefix(ctx, conn, rp, lf, offset)
		lf.Close()
		if err != nil {
			return nil, err
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
