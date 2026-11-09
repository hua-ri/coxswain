// Package file registers the SFTP file Actions: list, stat, read and
// write files on saved SSH hosts, and upload / download them as
// transfers.
package file

import (
	"bytes"
	"context"
	"crypto/rand"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"os"
	"path"
	"sort"
	"strconv"
	"strings"
	"time"

	"github.com/hua-ri/coxswain/internal/action"
	"github.com/hua-ri/coxswain/internal/inventory"
	"github.com/hua-ri/coxswain/internal/policy"
	"github.com/hua-ri/coxswain/internal/shaper"
	"github.com/hua-ri/coxswain/internal/sshx"
	"github.com/hua-ri/coxswain/internal/transfer"
	"github.com/pkg/sftp"
)

const (
	// maxEntries bounds a listing.
	maxEntries = 1000
	// maxRead is the largest file file.read returns.
	maxRead = 1 << 20
)

// humanLimits effectively disables shaping for people.
var humanLimits = shaper.Limits{MaxBytes: 4 << 20, MaxLines: 1 << 30}

type Provider struct {
	Hosts     *inventory.Store
	Dialer    *sshx.Dialer
	Pool      *sshx.Pool // reuses connections if set (in `cox server`)
	Artifacts *shaper.Artifacts
	Transfers *transfer.Manager
	// DownloadDir is where file.download saves files, one directory per
	// host (<cox home>/downloads).
	DownloadDir string
	// UploadDir is the only place agents may upload from (<cox home>/uploads).
	UploadDir string
	CoxHome   string // nothing in it but uploads and downloads is fit to upload
	UserHome  string // for ~ in local paths

	pace func(ctx context.Context) // tests: called before each chunk of a transfer
}

func (p *Provider) Register(r *action.Registry) {
	r.Register(p.list())
	r.Register(p.stat())
	r.Register(p.read())
	r.Register(p.write())
	r.Register(p.mkdir())
	r.Register(p.remove())
	r.Register(p.upload())
	r.Register(p.download())
}

var (
	hostProperty = map[string]any{"type": "string", "description": "saved host name (see ssh_hosts_list)"}
	pathProperty = map[string]any{"type": "string", "description": "remote path; relative paths and ~/ start in the login directory"}
)

// Entry describes one remote file.
type Entry struct {
	Name    string    `json:"name"`
	Type    string    `json:"type"` // file, dir, symlink or other
	Size    int64     `json:"size"`
	Mode    string    `json:"mode"` // as ls -l shows it, e.g. -rw-r--r--
	ModTime time.Time `json:"mod_time"`
	Link    string    `json:"link,omitempty"` // a symlink's target (file.stat only)
}

func entry(fi os.FileInfo) Entry {
	e := Entry{Name: fi.Name(), Size: fi.Size(), Mode: fi.Mode().String(), ModTime: fi.ModTime().UTC()}
	switch m := fi.Mode(); {
	case m.IsRegular():
		e.Type = "file"
	case m.IsDir():
		e.Type = "dir"
	case m&fs.ModeSymlink != 0:
		e.Type = "symlink"
	default:
		e.Type = "other"
	}
	return e
}

// remotePath turns a user's path into an SFTP path: "~" and "~/x" are
// relative to the login directory, where SFTP starts.
func remotePath(p string) (string, error) {
	switch {
	case p == "":
		return "", errors.New("path is required")
	case p == "~":
		return ".", nil
	case strings.HasPrefix(p, "~/"):
		return "./" + strings.TrimLeft(p[2:], "/"), nil
	}
	return p, nil
}

type pathInput struct {
	Host string `json:"host"`
	Path string `json:"path"`
}

func hostTarget(h inventory.Host) action.Target {
	return action.Target{Kind: "host", ID: h.Name, Tags: h.Tags}
}

// label rates an invocation on one remote path.
func (p *Provider) label(verb string, risk func(path string) action.Risk) func(context.Context, json.RawMessage) (action.Assessment, error) {
	return func(_ context.Context, raw json.RawMessage) (action.Assessment, error) {
		var in pathInput
		if err := action.Decode(raw, &in); err != nil {
			return action.Assessment{}, err
		}
		if _, err := remotePath(in.Path); err != nil {
			return action.Assessment{}, err
		}
		h, err := p.Hosts.Get(in.Host)
		if err != nil {
			return action.Assessment{}, err
		}
		return action.Assessment{Risk: risk(in.Path), Targets: []action.Target{hostTarget(h)},
			Preview: fmt.Sprintf("%s %s:%s", verb, h.Name, in.Path)}, nil
	}
}

func readRisk(string) action.Risk { return action.Read }

// writeRisk is destructive for system paths and credential stores.
func writeRisk(p string) action.Risk {
	if policy.IsProtected(p) {
		return action.Destructive
	}
	return action.Write
}

// session opens SFTP to the host. The session is closed when ctx ends,
// which aborts a call in progress.
func (p *Provider) session(ctx context.Context, name string) (inventory.Host, *sftp.Client, func(broken bool), error) {
	h, c, _, release, err := p.sessionConn(ctx, name)
	return h, c, release, err
}

// sessionConn is session, also returning the connection.
func (p *Provider) sessionConn(ctx context.Context, name string) (inventory.Host, *sftp.Client, *sshx.Conn, func(broken bool), error) {
	h, err := p.Hosts.Get(name)
	if err != nil {
		return h, nil, nil, nil, err
	}
	c, conn, release, err := sshx.SFTP(ctx, p.Pool, p.Dialer, h)
	if err != nil {
		return h, nil, nil, nil, err
	}
	stop := context.AfterFunc(ctx, func() { c.Close() })
	return h, c, conn, func(broken bool) {
		if !stop() {
			broken = true // closed under a call
		}
		release(broken)
	}, nil
}

// failed reports whether err means the SFTP session itself broke, as
// opposed to an error about a file.
func failed(err error) bool {
	var st *sftp.StatusError
	return err != nil && !errors.As(err, &st) && !errors.Is(err, os.ErrNotExist) && !errors.Is(err, os.ErrPermission)
}

// ListOutput is the result of file.list.
type ListOutput struct {
	Host      string            `json:"host"`
	HostNote  string            `json:"host_note,omitempty"`
	HostTags  map[string]string `json:"host_tags,omitempty"`
	Path      string            `json:"path"`
	Entries   []Entry           `json:"entries"`
	Total     int               `json:"total"`
	Truncated bool              `json:"truncated,omitempty"`
	Hint      string            `json:"hint,omitempty"`
}

func (p *Provider) list() *action.Action {
	return &action.Action{
		ID:      "file.list",
		Summary: "List a directory on a saved SSH host",
		Description: "List a remote directory over SFTP: name, type, size, permissions and modification time, sorted by name. " +
			"Hidden entries (starting with .) only with all=true. At most 1000 entries are returned.",
		Input: map[string]any{
			"type": "object",
			"properties": map[string]any{
				"host": hostProperty,
				"path": pathProperty,
				"all":  map[string]any{"type": "boolean", "description": "include hidden entries"},
			},
			"required": []string{"host", "path"},
		},
		Positional:  []string{"host", "path"},
		Annotations: action.Annotations{ReadOnly: true, Idempotent: true, OpenWorld: true},
		Label:       p.label("list", readRisk),
		Run: func(ctx context.Context, raw json.RawMessage) (any, error) {
			var in struct {
				pathInput
				All bool `json:"all"`
			}
			if err := action.Decode(raw, &in); err != nil {
				return nil, err
			}
			rp, err := remotePath(in.Path)
			if err != nil {
				return nil, err
			}
			h, c, release, err := p.session(ctx, in.Host)
			if err != nil {
				return nil, err
			}
			infos, err := c.ReadDir(rp)
			release(failed(err))
			if err != nil {
				return nil, fmt.Errorf("list %s: %w", in.Path, err)
			}
			out := ListOutput{Host: h.Name, HostNote: h.Note, HostTags: h.Tags, Path: in.Path, Entries: []Entry{}}
			for _, fi := range infos {
				if !in.All && strings.HasPrefix(fi.Name(), ".") {
					continue
				}
				out.Entries = append(out.Entries, entry(fi))
			}
			sort.Slice(out.Entries, func(i, j int) bool { return out.Entries[i].Name < out.Entries[j].Name })
			out.Total = len(out.Entries)
			if out.Total > maxEntries {
				out.Entries, out.Truncated = out.Entries[:maxEntries], true
				out.Hint = fmt.Sprintf("showing the first %d of %d entries; list a subdirectory, or use ssh_exec with find/ls and a filter",
					maxEntries, out.Total)
			}
			return out, nil
		},
	}
}

// StatOutput is the result of file.stat.
type StatOutput struct {
	Host     string            `json:"host"`
	HostNote string            `json:"host_note,omitempty"`
	HostTags map[string]string `json:"host_tags,omitempty"`
	Path     string            `json:"path"`
	Entry    Entry             `json:"entry"`
}

func (p *Provider) stat() *action.Action {
	return &action.Action{
		ID:          "file.stat",
		Summary:     "Show a remote file's type, size, permissions and modification time",
		Description: "Show the metadata of one remote path over SFTP (a symlink is described itself, with its target).",
		Input: map[string]any{
			"type":       "object",
			"properties": map[string]any{"host": hostProperty, "path": pathProperty},
			"required":   []string{"host", "path"},
		},
		Positional:  []string{"host", "path"},
		Annotations: action.Annotations{ReadOnly: true, Idempotent: true, OpenWorld: true},
		Label:       p.label("stat", readRisk),
		Run: func(ctx context.Context, raw json.RawMessage) (any, error) {
			var in pathInput
			if err := action.Decode(raw, &in); err != nil {
				return nil, err
			}
			rp, err := remotePath(in.Path)
			if err != nil {
				return nil, err
			}
			h, c, release, err := p.session(ctx, in.Host)
			if err != nil {
				return nil, err
			}
			fi, err := c.Lstat(rp)
			var link string
			if err == nil && fi.Mode()&fs.ModeSymlink != 0 {
				link, _ = c.ReadLink(rp)
			}
			release(failed(err))
			if err != nil {
				return nil, fmt.Errorf("stat %s: %w", in.Path, err)
			}
			e := entry(fi)
			e.Name, e.Link = path.Base(in.Path), link
			return StatOutput{Host: h.Name, HostNote: h.Note, HostTags: h.Tags, Path: in.Path, Entry: e}, nil
		},
	}
}

type readInput struct {
	pathInput
	Offset int `json:"offset"`
	Limit  int `json:"limit"`
}

// ReadOutput is the result of file.read.
type ReadOutput struct {
	Host       string            `json:"host"`
	HostNote   string            `json:"host_note,omitempty"`
	HostTags   map[string]string `json:"host_tags,omitempty"`
	Path       string            `json:"path"`
	Size       int64             `json:"size"`
	TotalLines int               `json:"total_lines"`
	Content    shaper.Shaped     `json:"content"`
}

// PrintCLI writes the content as-is.
func (o ReadOutput) PrintCLI(stdout, _ io.Writer) int {
	io.WriteString(stdout, o.Content.Text)
	return 0
}

func (p *Provider) read() *action.Action {
	return &action.Action{
		ID:      "file.read",
		Summary: "Read a text file on a saved SSH host",
		Description: "Read a remote text file (up to 1 MiB) over SFTP, optionally only lines offset..offset+limit-1. " +
			"Binary files and files over 1 MiB are refused: fetch those with file_download and inspect the local copy. " +
			"Large outputs are truncated; read more with artifact_read.",
		Input: map[string]any{
			"type": "object",
			"properties": map[string]any{
				"host":   hostProperty,
				"path":   pathProperty,
				"offset": map[string]any{"type": "integer", "minimum": 1, "description": "first line to return (1-based), default 1"},
				"limit":  map[string]any{"type": "integer", "minimum": 1, "description": "number of lines to return, default all"},
			},
			"required": []string{"host", "path"},
		},
		Positional:  []string{"host", "path"},
		Annotations: action.Annotations{ReadOnly: true, Idempotent: true, OpenWorld: true},
		Label:       p.label("read", readRisk),
		Run: func(ctx context.Context, raw json.RawMessage) (any, error) {
			var in readInput
			if err := action.Decode(raw, &in); err != nil {
				return nil, err
			}
			if in.Offset < 0 || in.Limit < 0 {
				return nil, errors.New("offset and limit must be positive")
			}
			rp, err := remotePath(in.Path)
			if err != nil {
				return nil, err
			}
			h, c, release, err := p.session(ctx, in.Host)
			if err != nil {
				return nil, err
			}
			data, err := readSmall(c, rp, in.Path)
			release(failed(err))
			if err != nil {
				return nil, err
			}
			lines := strings.SplitAfter(string(data), "\n")
			if lines[len(lines)-1] == "" {
				lines = lines[:len(lines)-1]
			}
			out := ReadOutput{Host: h.Name, HostNote: h.Note, HostTags: h.Tags, Path: in.Path, Size: int64(len(data)), TotalLines: len(lines)}
			from := max(in.Offset, 1) - 1
			to := len(lines)
			if in.Limit > 0 {
				to = min(from+in.Limit, len(lines))
			}
			text := ""
			if from < len(lines) {
				text = strings.Join(lines[from:to], "")
			}
			lim := shaper.Default
			if a, ok := action.ActorFrom(ctx); ok && a.Kind == action.Human {
				lim = humanLimits
			}
			out.Content, err = p.Artifacts.Shape(text, lim)
			return out, err
		},
	}
}

// readSmall reads a text file of at most maxRead bytes.
func readSmall(c *sftp.Client, rp, shown string) ([]byte, error) {
	fi, err := c.Stat(rp)
	if err != nil {
		return nil, fmt.Errorf("read %s: %w", shown, err)
	}
	if fi.IsDir() {
		return nil, fmt.Errorf("%s is a directory; use file_list", shown)
	}
	if fi.Size() > maxRead {
		return nil, fmt.Errorf("%s is %s; file_read reads files up to 1 MiB. Use file_download to fetch it and inspect the local copy",
			shown, size(fi.Size()))
	}
	f, err := c.Open(rp)
	if err != nil {
		return nil, fmt.Errorf("read %s: %w", shown, err)
	}
	defer f.Close()
	data, err := io.ReadAll(io.LimitReader(f, maxRead+1))
	if err != nil {
		return nil, fmt.Errorf("read %s: %w", shown, err)
	}
	if len(data) > maxRead {
		return nil, fmt.Errorf("%s is over 1 MiB; use file_download to fetch it and inspect the local copy", shown)
	}
	if bytes.IndexByte(data, 0) >= 0 {
		return nil, fmt.Errorf("%s looks binary; use file_download to fetch it and inspect the local copy", shown)
	}
	return data, nil
}

func size(n int64) string {
	switch {
	case n >= 1<<30:
		return fmt.Sprintf("%.1f GiB", float64(n)/(1<<30))
	case n >= 1<<20:
		return fmt.Sprintf("%.1f MiB", float64(n)/(1<<20))
	case n >= 1<<10:
		return fmt.Sprintf("%.1f KiB", float64(n)/(1<<10))
	}
	return fmt.Sprintf("%d bytes", n)
}

type writeInput struct {
	pathInput
	Content    string `json:"content"`
	Mode       string `json:"mode"`
	CreateOnly bool   `json:"create_only"`
}

// WriteOutput is the result of file.write.
type WriteOutput struct {
	Host     string            `json:"host"`
	HostNote string            `json:"host_note,omitempty"`
	HostTags map[string]string `json:"host_tags,omitempty"`
	Path     string            `json:"path"`
	Size     int               `json:"size"`
	Mode     string            `json:"mode"`
	Created  bool              `json:"created"` // the file did not exist
}

// redactContent replaces file.write's content with its size and SHA-256
// for the audit log.
// writeSchema is file.write's input.
var writeSchema = map[string]any{
	"type": "object",
	"properties": map[string]any{
		"host":        hostProperty,
		"path":        pathProperty,
		"content":     map[string]any{"type": "string", "description": "the complete new file content"},
		"mode":        map[string]any{"type": "string", "description": "octal permissions, e.g. 0644"},
		"create_only": map[string]any{"type": "boolean", "description": "fail if the file exists"},
	},
	"required": []string{"host", "path", "content"},
}

func redactContent(raw json.RawMessage) json.RawMessage {
	if action.ExactKeys(raw, writeSchema) != nil {
		return json.RawMessage(`{"redacted":"arguments with unknown keys"}`)
	}
	var m map[string]json.RawMessage
	if json.Unmarshal(raw, &m) != nil {
		return json.RawMessage(`{"redacted":true}`)
	}
	var content string
	if c, ok := m["content"]; ok {
		if json.Unmarshal(c, &content) != nil {
			content = string(c)
		}
		sum := sha256.Sum256([]byte(content))
		m["content"], _ = json.Marshal(map[string]any{"bytes": len(content), "sha256": hex.EncodeToString(sum[:])})
	}
	out, err := json.Marshal(m)
	if err != nil {
		return json.RawMessage(`{"redacted":true}`)
	}
	return out
}

func parseMode(s string) (fs.FileMode, error) {
	m, err := strconv.ParseUint(s, 8, 32)
	if err != nil || m > 0o7777 {
		return 0, fmt.Errorf("mode must be octal permissions such as 0644, not %q", s)
	}
	return fs.FileMode(m), nil
}

func (p *Provider) write() *action.Action {
	return &action.Action{
		ID:      "file.write",
		Summary: "Write a text file on a saved SSH host",
		Description: "Replace (or create) a remote file with content, atomically: the content goes to a temporary file in " +
			"the same directory, which is then renamed over the target, so readers never see a half-written file. " +
			"Keeps the existing permissions unless mode (octal, e.g. 0644) is given; new files get 0644. " +
			"create_only=true refuses to replace an existing file. " +
			"Hosts tagged env=prod are production: confirm with the user before changing them. Respect the host note.",
		Input:       writeSchema,
		Positional:  []string{"host", "path"},
		Annotations: action.Annotations{Destructive: true, Idempotent: true, OpenWorld: true},
		Redact:      redactContent,
		Label: func(ctx context.Context, raw json.RawMessage) (action.Assessment, error) {
			if err := action.ExactKeys(raw, writeSchema); err != nil {
				return action.Assessment{}, err
			}
			var in writeInput
			if err := action.Decode(raw, &in); err != nil {
				return action.Assessment{}, err
			}
			as, err := p.label("write", writeRisk)(ctx, raw)
			if err == nil {
				as.Preview = fmt.Sprintf("write %d bytes to %s", len(in.Content), strings.TrimPrefix(as.Preview, "write "))
			}
			return as, err
		},
		Run: func(ctx context.Context, raw json.RawMessage) (any, error) {
			var in writeInput
			if err := action.Decode(raw, &in); err != nil {
				return nil, err
			}
			rp, err := remotePath(in.Path)
			if err != nil {
				return nil, err
			}
			var mode fs.FileMode
			if in.Mode != "" {
				if mode, err = parseMode(in.Mode); err != nil {
					return nil, err
				}
			}
			h, c, release, err := p.session(ctx, in.Host)
			if err != nil {
				return nil, err
			}
			out, err := p.writeAtomic(ctx, h, c, rp, []byte(in.Content), mode, in.CreateOnly)
			release(failed(err))
			if err != nil {
				return nil, fmt.Errorf("write %s: %w", in.Path, err)
			}
			out.Host, out.HostNote, out.HostTags, out.Path = h.Name, h.Note, h.Tags, in.Path
			return out, nil
		},
	}
}

// target resolves the file a write replaces: a symlink at rp is followed
// (one level), so the link stays. fi is nil if the file does not exist.
func target(c *sftp.Client, rp string) (string, os.FileInfo, error) {
	fi, err := c.Lstat(rp)
	switch {
	case errors.Is(err, fs.ErrNotExist):
		return rp, nil, nil
	case err != nil:
		return rp, nil, err
	case fi.Mode()&fs.ModeSymlink == 0:
		return rp, fi, nil
	}
	link, err := c.ReadLink(rp)
	if err != nil {
		return rp, nil, err
	}
	if !path.IsAbs(link) {
		link = path.Join(path.Dir(rp), link)
	}
	fi, err = c.Stat(link)
	if errors.Is(err, fs.ErrNotExist) {
		return link, nil, nil
	}
	return link, fi, err
}

// createPrivate opens (creating or truncating) a temporary file and
// makes it 0600 before anything is written to it.
func createPrivate(c *sftp.Client, tmp string, flags int) (*sftp.File, error) {
	f, err := c.OpenFile(tmp, flags)
	if err != nil {
		return nil, err
	}
	if err := f.Chmod(0o600); err != nil {
		f.Close()
		return nil, err
	}
	return f, nil
}

// writeAtomic writes data to a private temporary file next to rp, gives
// it its mode and renames it over rp. mode 0 keeps the existing
// permissions (0644 for a new file).
func (p *Provider) writeAtomic(ctx context.Context, h inventory.Host, c *sftp.Client, rp string, data []byte, mode fs.FileMode, createOnly bool) (WriteOutput, error) {
	out := WriteOutput{Size: len(data)}
	if createOnly {
		if _, err := c.Lstat(rp); err == nil {
			return out, fmt.Errorf("%w: it exists (create_only)", fs.ErrExist)
		}
	}
	rp, fi, err := target(c, rp)
	if err != nil {
		return out, err
	}
	if fi != nil && fi.IsDir() {
		return out, errors.New("it is a directory")
	}
	out.Created = fi == nil
	if mode == 0 {
		mode = 0o644
		if fi != nil {
			mode = fi.Mode().Perm()
		}
	}
	out.Mode = fmt.Sprintf("%04o", uint32(mode))

	var b [6]byte
	rand.Read(b[:])
	tmp := path.Join(path.Dir(rp), "."+path.Base(rp)+".cox-"+hex.EncodeToString(b[:]))
	f, err := createPrivate(c, tmp, os.O_WRONLY|os.O_CREATE|os.O_EXCL)
	if err != nil {
		return out, err
	}
	if p.pace != nil {
		p.pace(ctx)
	}
	_, err = f.Write(data)
	if cerr := f.Close(); err == nil {
		err = cerr
	}
	if err == nil {
		err = c.Chmod(tmp, mode)
	}
	if err == nil {
		if createOnly {
			err = c.Rename(tmp, rp) // SFTP rename does not replace a file
		} else {
			err = c.PosixRename(tmp, rp)
		}
	}
	if err != nil {
		p.removeQuietly(h, c, tmp)
		if createOnly {
			if _, serr := c.Lstat(rp); serr == nil {
				return out, fmt.Errorf("%w: it exists (create_only)", fs.ErrExist)
			}
		}
		return out, err
	}
	return out, nil
}

// removeQuietly deletes a temporary file, over a new connection if the
// session broke (best effort).
func (p *Provider) removeQuietly(h inventory.Host, c *sftp.Client, tmp string) {
	err := c.Remove(tmp)
	if err == nil || !failed(err) {
		return
	}
	ctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
	defer cancel()
	conn, err := p.Dialer.Dial(ctx, h)
	if err != nil {
		return
	}
	defer conn.Close()
	if fresh, err := sftp.NewClient(conn.Client); err == nil {
		fresh.Remove(tmp)
		fresh.Close()
	}
}

// PathOutput is the result of file.mkdir and file.remove.
type PathOutput struct {
	Host string `json:"host"`
	Path string `json:"path"`
}

func (p *Provider) mkdir() *action.Action {
	return &action.Action{
		ID:          "file.mkdir",
		Summary:     "Create a directory on a saved SSH host",
		Description: "Create a remote directory over SFTP; parents=true also creates missing parents and accepts an existing directory (like mkdir -p).",
		Input: map[string]any{
			"type": "object",
			"properties": map[string]any{
				"host":    hostProperty,
				"path":    pathProperty,
				"parents": map[string]any{"type": "boolean", "description": "create missing parents; no error if it exists"},
			},
			"required": []string{"host", "path"},
		},
		Positional:  []string{"host", "path"},
		Annotations: action.Annotations{Destructive: true, Idempotent: true, OpenWorld: true},
		Label:       p.label("create directory", writeRisk),
		Run: func(ctx context.Context, raw json.RawMessage) (any, error) {
			var in struct {
				pathInput
				Parents bool `json:"parents"`
			}
			if err := action.Decode(raw, &in); err != nil {
				return nil, err
			}
			rp, err := remotePath(in.Path)
			if err != nil {
				return nil, err
			}
			h, c, release, err := p.session(ctx, in.Host)
			if err != nil {
				return nil, err
			}
			if in.Parents {
				err = c.MkdirAll(rp)
			} else {
				err = c.Mkdir(rp)
			}
			release(failed(err))
			if err != nil {
				return nil, fmt.Errorf("mkdir %s: %w", in.Path, err)
			}
			return PathOutput{Host: h.Name, Path: in.Path}, nil
		},
	}
}

func (p *Provider) remove() *action.Action {
	return &action.Action{
		ID:      "file.remove",
		Summary: "Delete a file or an empty directory on a saved SSH host",
		Description: "Delete one remote file, symlink or empty directory over SFTP. It does not delete directory trees. " +
			"Hosts tagged env=prod are production: confirm with the user before changing them. Respect the host note.",
		Input: map[string]any{
			"type":       "object",
			"properties": map[string]any{"host": hostProperty, "path": pathProperty},
			"required":   []string{"host", "path"},
		},
		Positional:  []string{"host", "path"},
		Annotations: action.Annotations{Destructive: true, Idempotent: true, OpenWorld: true},
		Label:       p.label("delete", func(string) action.Risk { return action.Destructive }),
		Run: func(ctx context.Context, raw json.RawMessage) (any, error) {
			var in pathInput
			if err := action.Decode(raw, &in); err != nil {
				return nil, err
			}
			rp, err := remotePath(in.Path)
			if err != nil {
				return nil, err
			}
			h, c, release, err := p.session(ctx, in.Host)
			if err != nil {
				return nil, err
			}
			err = c.Remove(rp)
			release(failed(err))
			if err != nil {
				return nil, fmt.Errorf("delete %s: %w", in.Path, err)
			}
			return PathOutput{Host: h.Name, Path: in.Path}, nil
		},
	}
}
