package sshtest

import (
	"errors"
	"io"
	"io/fs"
	"os"
	"strings"
	"time"

	"github.com/pkg/sftp"
	"golang.org/x/crypto/ssh"
)

// serveSFTP runs an SFTP server on ch whose "/" is root. Every path is
// resolved inside root with os.Root, so neither ".." nor symlinks reach
// the rest of the filesystem.
func serveSFTP(ch ssh.Channel, root string) {
	r, err := os.OpenRoot(root)
	if err != nil {
		ch.Close()
		return
	}
	defer r.Close()
	h := &rooted{root: r}
	srv := sftp.NewRequestServer(ch, sftp.Handlers{FileGet: h, FilePut: h, FileCmd: h, FileList: h})
	srv.Serve()
	srv.Close()
}

type rooted struct{ root *os.Root }

// name maps an SFTP path (always clean and absolute) to a name in root.
func name(p string) string {
	p = strings.TrimPrefix(p, "/")
	if p == "" {
		return "."
	}
	return p
}

func (h *rooted) Fileread(r *sftp.Request) (io.ReaderAt, error) {
	return h.root.Open(name(r.Filepath))
}

func (h *rooted) Filewrite(r *sftp.Request) (io.WriterAt, error) {
	return h.open(r)
}

func (h *rooted) OpenFile(r *sftp.Request) (sftp.WriterAtReaderAt, error) {
	return h.open(r)
}

func (h *rooted) open(r *sftp.Request) (*os.File, error) {
	f := r.Pflags()
	flag := 0
	switch {
	case f.Read && (f.Write || f.Append):
		flag = os.O_RDWR
	case f.Write || f.Append:
		flag = os.O_WRONLY
	}
	// O_APPEND is not set even for Append: the client writes at explicit
	// offsets (WriteAt), which O_APPEND forbids.
	if f.Creat {
		flag |= os.O_CREATE
	}
	if f.Trunc {
		flag |= os.O_TRUNC
	}
	if f.Excl {
		flag |= os.O_EXCL
	}
	perm := fs.FileMode(0o644)
	if a := r.AttrFlags(); a.Permissions {
		perm = r.Attributes().FileMode().Perm()
	}
	return h.root.OpenFile(name(r.Filepath), flag, perm)
}

func (h *rooted) Filecmd(r *sftp.Request) error {
	switch r.Method {
	case "Setstat":
		if r.AttrFlags().Permissions {
			if err := h.root.Chmod(name(r.Filepath), r.Attributes().FileMode().Perm()); err != nil {
				return err
			}
		}
		if r.AttrFlags().Size {
			f, err := h.root.OpenFile(name(r.Filepath), os.O_WRONLY, 0)
			if err != nil {
				return err
			}
			defer f.Close()
			return f.Truncate(int64(r.Attributes().Size))
		}
		return nil
	case "Rename":
		// SFTP v3 rename does not replace an existing file.
		if _, err := h.root.Lstat(name(r.Target)); err == nil {
			return os.ErrExist
		}
		return h.root.Rename(name(r.Filepath), name(r.Target))
	case "Rmdir", "Remove":
		return h.root.Remove(name(r.Filepath))
	case "Mkdir":
		return h.root.Mkdir(name(r.Filepath), 0o755)
	case "Symlink":
		return h.root.Symlink(r.Filepath, name(r.Target))
	}
	return errors.New("unsupported: " + r.Method)
}

// PosixRename replaces the target, like posix-rename@openssh.com.
func (h *rooted) PosixRename(r *sftp.Request) error {
	return h.root.Rename(name(r.Filepath), name(r.Target))
}

func (h *rooted) Filelist(r *sftp.Request) (sftp.ListerAt, error) {
	switch r.Method {
	case "List":
		f, err := h.root.Open(name(r.Filepath))
		if err != nil {
			return nil, err
		}
		defer f.Close()
		list, err := f.Readdir(-1)
		if err != nil {
			return nil, err
		}
		return lister(list), nil
	case "Stat":
		fi, err := h.root.Stat(name(r.Filepath))
		if err != nil {
			return nil, err
		}
		return lister{fi}, nil
	case "Readlink":
		target, err := h.root.Readlink(name(r.Filepath))
		if err != nil {
			return nil, err
		}
		return lister{linkInfo(target)}, nil
	}
	return nil, errors.New("unsupported: " + r.Method)
}

func (h *rooted) Lstat(r *sftp.Request) (sftp.ListerAt, error) {
	fi, err := h.root.Lstat(name(r.Filepath))
	if err != nil {
		return nil, err
	}
	return lister{fi}, nil
}

type lister []os.FileInfo

func (l lister) ListAt(out []os.FileInfo, off int64) (int, error) {
	if off >= int64(len(l)) {
		return 0, io.EOF
	}
	n := copy(out, l[off:])
	if n < len(out) {
		return n, io.EOF
	}
	return n, nil
}

// linkInfo carries a symlink's target as a file name, which is how the
// request server answers Readlink.
type linkInfo string

func (l linkInfo) Name() string     { return string(l) }
func (linkInfo) Size() int64        { return 0 }
func (linkInfo) Mode() fs.FileMode  { return fs.ModeSymlink }
func (linkInfo) ModTime() time.Time { return time.Time{} }
func (linkInfo) IsDir() bool        { return false }
func (linkInfo) Sys() any           { return nil }
