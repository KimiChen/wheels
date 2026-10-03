//go:build linux || darwin

package managed

import (
	"crypto/rand"
	"encoding/hex"
	"errors"
	"io"
	"os"
	"path/filepath"
	"strings"

	"golang.org/x/sys/unix"
)

// All file operations are relative to pinned directory descriptors. Walking
// each absolute component with O_NOFOLLOW also rejects ancestor symlinks.
type privateDir struct {
	file *os.File
	path string
}

func openPrivateDir(path string) (*privateDir, error) {
	fd, err := directoryFD(path, true)
	if err != nil {
		return nil, err
	}
	d := &privateDir{file: os.NewFile(uintptr(fd), "managed-directory"), path: path}
	if err := d.check(); err != nil {
		d.close()
		return nil, err
	}
	return d, nil
}

func directoryFD(path string, create bool) (int, error) {
	if !filepath.IsAbs(path) || filepath.Clean(path) != path || path == "/" {
		return -1, ErrUnsafePath
	}
	fd, err := unix.Open("/", unix.O_RDONLY|unix.O_DIRECTORY|unix.O_CLOEXEC, 0)
	if err != nil {
		return -1, ErrUnsafePath
	}
	parts := strings.Split(strings.TrimPrefix(path, "/"), "/")
	for i, part := range parts {
		next, openErr := unix.Openat(fd, part, unix.O_RDONLY|unix.O_DIRECTORY|unix.O_NOFOLLOW|unix.O_CLOEXEC, 0)
		if create && openErr == unix.ENOENT && i == len(parts)-1 {
			if mkdirErr := unix.Mkdirat(fd, part, 0700); mkdirErr != nil {
				unix.Close(fd)
				return -1, ErrUnsafePath
			}
			if syncErr := unix.Fsync(fd); syncErr != nil {
				unix.Close(fd)
				return -1, ErrStorage
			}
			next, openErr = unix.Openat(fd, part, unix.O_RDONLY|unix.O_DIRECTORY|unix.O_NOFOLLOW|unix.O_CLOEXEC, 0)
		}
		unix.Close(fd)
		if openErr != nil {
			return -1, ErrUnsafePath
		}
		fd = next
	}
	return fd, nil
}

func (d *privateDir) check() error {
	var st unix.Stat_t
	if unix.Fstat(int(d.file.Fd()), &st) != nil || st.Mode&unix.S_IFMT != unix.S_IFDIR || st.Mode&0777 != 0700 || st.Uid != uint32(os.Geteuid()) {
		return ErrUnsafePath
	}
	fd, err := directoryFD(d.path, false)
	if err != nil {
		return ErrUnsafePath
	}
	defer unix.Close(fd)
	var current unix.Stat_t
	if unix.Fstat(fd, &current) != nil || current.Dev != st.Dev || current.Ino != st.Ino {
		return ErrUnsafePath
	}
	return nil
}

func (d *privateDir) subdir(name string) (*privateDir, error) {
	if err := d.check(); err != nil {
		return nil, err
	}
	if err := unix.Mkdirat(int(d.file.Fd()), name, 0700); err != nil && err != unix.EEXIST {
		return nil, ErrStorage
	}
	if err := d.file.Sync(); err != nil {
		return nil, ErrStorage
	}
	fd, err := unix.Openat(int(d.file.Fd()), name, unix.O_RDONLY|unix.O_DIRECTORY|unix.O_NOFOLLOW|unix.O_CLOEXEC, 0)
	if err != nil {
		return nil, ErrUnsafePath
	}
	next := &privateDir{file: os.NewFile(uintptr(fd), "managed-subdirectory"), path: filepath.Join(d.path, name)}
	if err := next.check(); err != nil {
		next.close()
		return nil, err
	}
	return next, nil
}

func validFile(f *os.File, max int64) (os.FileInfo, error) {
	st, err := f.Stat()
	var raw unix.Stat_t
	if err != nil || !st.Mode().IsRegular() || st.Mode().Perm() != 0600 || st.Size() < 0 || st.Size() > max || unix.Fstat(int(f.Fd()), &raw) != nil || raw.Uid != uint32(os.Geteuid()) || raw.Nlink != 1 {
		return nil, ErrUnsafePath
	}
	return st, nil
}

func (d *privateDir) read(name string, max int64) (StoreSnapshot, error) {
	if err := d.check(); err != nil {
		return StoreSnapshot{}, err
	}
	fd, err := unix.Openat(int(d.file.Fd()), name, unix.O_RDONLY|unix.O_NOFOLLOW|unix.O_NONBLOCK|unix.O_CLOEXEC, 0)
	if err == unix.ENOENT {
		return StoreSnapshot{}, nil
	}
	if err != nil {
		return StoreSnapshot{}, ErrUnsafePath
	}
	f := os.NewFile(uintptr(fd), "managed-private-file")
	defer f.Close()
	before, err := validFile(f, max)
	if err != nil {
		return StoreSnapshot{}, err
	}
	data, err := io.ReadAll(io.LimitReader(f, max+1))
	if err != nil || int64(len(data)) > max {
		return StoreSnapshot{}, ErrStorage
	}
	after, err := validFile(f, max)
	if err != nil || before.Size() != after.Size() || !before.ModTime().Equal(after.ModTime()) {
		return StoreSnapshot{}, ErrConflict
	}
	return StoreSnapshot{Exists: true, Bytes: data}, nil
}

// compare immediately before rename. Cooperative writers are excluded by the
// lifetime flock; native write endpoints must use this engine's owner barrier.
// Arbitrary same-UID writers cannot be made transactional by a file lock they
// ignore, so any detected raw-byte drift is rejected rather than overwritten.
func (d *privateDir) replace(name string, desired StoreSnapshot, expected string, max int64, hook func(string) error) error {
	if int64(len(desired.Bytes)) > max || (!desired.Exists && len(desired.Bytes) != 0) {
		return ErrInvalid
	}
	call := func(stage string) error {
		if hook != nil {
			return hook(stage)
		}
		return nil
	}
	if err := d.check(); err != nil {
		return err
	}
	current, err := d.read(name, max)
	if err != nil {
		return err
	}
	if expected != "" && Digest(current) != expected {
		return ErrConflict
	}
	if err := call("before_write"); err != nil {
		return ErrStorage
	}
	if !desired.Exists {
		if err := call("before_rename"); err != nil {
			return ErrStorage
		}
		current, err = d.read(name, max)
		if err != nil {
			return err
		}
		if expected != "" && Digest(current) != expected {
			return ErrConflict
		}
		if err := unix.Unlinkat(int(d.file.Fd()), name, 0); err != nil && err != unix.ENOENT {
			return ErrStorage
		}
		if err := call("after_rename"); err != nil {
			return ErrStorage
		}
		if err := d.file.Sync(); err != nil {
			return ErrStorage
		}
		if err := d.check(); err != nil {
			return err
		}
		return call("after_dir_sync")
	}
	var random [16]byte
	if _, err := rand.Read(random[:]); err != nil {
		return ErrStorage
	}
	tmp := ".txn-" + hex.EncodeToString(random[:])
	fd, err := unix.Openat(int(d.file.Fd()), tmp, unix.O_WRONLY|unix.O_CREAT|unix.O_EXCL|unix.O_NOFOLLOW|unix.O_CLOEXEC, 0600)
	if err != nil {
		return ErrStorage
	}
	f := os.NewFile(uintptr(fd), "managed-temporary-file")
	defer func() { f.Close(); unix.Unlinkat(int(d.file.Fd()), tmp, 0) }()
	if err := f.Chmod(0600); err != nil {
		return ErrStorage
	}
	if _, err := f.Write(desired.Bytes); err != nil {
		return ErrStorage
	}
	if err := f.Sync(); err != nil {
		return ErrStorage
	}
	if err := f.Close(); err != nil {
		return ErrStorage
	}
	if err := call("after_file_sync"); err != nil {
		return ErrStorage
	}
	if err := call("before_rename"); err != nil {
		return ErrStorage
	}
	current, err = d.read(name, max)
	if err != nil {
		return err
	}
	if expected != "" && Digest(current) != expected {
		return ErrConflict
	}
	if err := unix.Renameat(int(d.file.Fd()), tmp, int(d.file.Fd()), name); err != nil {
		return ErrStorage
	}
	if err := call("after_rename"); err != nil {
		return ErrStorage
	}
	if err := d.file.Sync(); err != nil {
		return ErrStorage
	}
	if err := d.check(); err != nil {
		return err
	}
	return call("after_dir_sync")
}

func (d *privateDir) lock() (*os.File, error) {
	fd, err := unix.Openat(int(d.file.Fd()), ".lock", unix.O_RDWR|unix.O_CREAT|unix.O_NOFOLLOW|unix.O_NONBLOCK|unix.O_CLOEXEC, 0600)
	if err != nil {
		return nil, ErrUnsafePath
	}
	f := os.NewFile(uintptr(fd), "managed-lock")
	if _, err := validFile(f, 0); err != nil {
		f.Close()
		return nil, err
	}
	if err := unix.Flock(fd, unix.LOCK_EX|unix.LOCK_NB); err != nil {
		f.Close()
		if errors.Is(err, unix.EWOULDBLOCK) {
			return nil, ErrLocked
		}
		return nil, ErrStorage
	}
	return f, nil
}

func (d *privateDir) names() ([]string, error) {
	if err := d.check(); err != nil {
		return nil, err
	}
	if _, err := d.file.Seek(0, 0); err != nil {
		return nil, ErrStorage
	}
	names, err := d.file.Readdirnames(16417)
	if err != nil && err != io.EOF {
		return nil, ErrStorage
	}
	if len(names) == 16417 {
		return nil, ErrStorage
	}
	return names, nil
}

func (d *privateDir) close() {
	if d != nil && d.file != nil {
		d.file.Close()
	}
}
