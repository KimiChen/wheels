//go:build linux || darwin

package managed

import (
	"errors"
	"io"
	"os"
	"path/filepath"
	"time"

	"golang.org/x/sys/unix"
)

func openExistingPrivateDir(path string) (*privateDir, error) {
	fd, err := directoryFD(path, false)
	if err != nil {
		return nil, err
	}
	d := &privateDir{file: os.NewFile(uintptr(fd), "checkpoint-directory"), path: path}
	if err = d.check(); err != nil {
		d.close()
		return nil, err
	}
	return d, nil
}
func createPrivateDirectory(path string) (*privateDir, error) {
	if !filepath.IsAbs(path) || filepath.Clean(path) != path || path == "/" {
		return nil, ErrUnsafePath
	}
	fd, err := directoryFD(filepath.Dir(path), false)
	if err != nil {
		return nil, err
	}
	defer unix.Close(fd)
	if unix.Mkdirat(fd, filepath.Base(path), 0700) != nil {
		return nil, ErrUnsafePath
	}
	if unix.Fsync(fd) != nil {
		return nil, ErrStorage
	}
	childFD, err := unix.Openat(fd, filepath.Base(path), unix.O_RDONLY|unix.O_DIRECTORY|unix.O_NOFOLLOW|unix.O_CLOEXEC, 0)
	if err != nil {
		return nil, ErrUnsafePath
	}
	dir := &privateDir{file: os.NewFile(uintptr(childFD), "checkpoint-directory"), path: path}
	if err = dir.check(); err != nil {
		dir.close()
		return nil, err
	}
	return dir, nil
}
func (d *privateDir) optionalSubdir(name string) (*privateDir, error) {
	if err := d.check(); err != nil {
		return nil, err
	}
	fd, err := unix.Openat(int(d.file.Fd()), name, unix.O_RDONLY|unix.O_DIRECTORY|unix.O_NOFOLLOW|unix.O_CLOEXEC, 0)
	if errors.Is(err, unix.ENOENT) {
		return nil, nil
	}
	if err != nil {
		return nil, ErrUnsafePath
	}
	next := &privateDir{file: os.NewFile(uintptr(fd), "checkpoint-subdirectory"), path: filepath.Join(d.path, name)}
	if err = next.check(); err != nil {
		next.close()
		return nil, err
	}
	return next, nil
}
func (d *privateDir) existingSubdir(name string) (*privateDir, error) {
	next, err := d.optionalSubdir(name)
	if err == nil && next == nil {
		return nil, ErrRecovery
	}
	return next, err
}
func (d *privateDir) readMetadata(name string, maximum int64) (StoreSnapshot, int64, error) {
	if err := d.check(); err != nil {
		return StoreSnapshot{}, 0, err
	}
	fd, err := unix.Openat(int(d.file.Fd()), name, unix.O_RDONLY|unix.O_NONBLOCK|unix.O_NOFOLLOW|unix.O_CLOEXEC, 0)
	if errors.Is(err, unix.ENOENT) {
		return StoreSnapshot{}, 0, nil
	}
	if err != nil {
		return StoreSnapshot{}, 0, ErrUnsafePath
	}
	file := os.NewFile(uintptr(fd), "checkpoint-private-file")
	defer file.Close()
	before, err := validFile(file, maximum)
	if err != nil {
		return StoreSnapshot{}, 0, err
	}
	var original unix.Stat_t
	if unix.Fstat(fd, &original) != nil {
		return StoreSnapshot{}, 0, ErrStorage
	}
	data, err := io.ReadAll(io.LimitReader(file, maximum+1))
	if err != nil || int64(len(data)) > maximum {
		return StoreSnapshot{}, 0, ErrStorage
	}
	after, err := validFile(file, maximum)
	var named unix.Stat_t
	if err != nil || before.Size() != after.Size() || !before.ModTime().Equal(after.ModTime()) || unix.Fstatat(int(d.file.Fd()), name, &named, unix.AT_SYMLINK_NOFOLLOW) != nil || named.Dev != original.Dev || named.Ino != original.Ino {
		return StoreSnapshot{}, 0, ErrConflict
	}
	if err := d.check(); err != nil {
		return StoreSnapshot{}, 0, err
	}
	return StoreSnapshot{Exists: true, Bytes: data}, before.ModTime().UnixNano(), nil
}
func (d *privateDir) restoreTime(name string, modified int64) error {
	if modified <= 0 {
		return ErrInvalid
	}
	if err := d.check(); err != nil {
		return err
	}
	stamp := unix.NsecToTimespec(modified)
	if unix.UtimesNanoAt(int(d.file.Fd()), name, []unix.Timespec{stamp, stamp}, unix.AT_SYMLINK_NOFOLLOW) != nil {
		return ErrStorage
	}
	fd, err := unix.Openat(int(d.file.Fd()), name, unix.O_RDONLY|unix.O_NOFOLLOW|unix.O_CLOEXEC, 0)
	if err != nil {
		return ErrStorage
	}
	file := os.NewFile(uintptr(fd), "restore-mtime")
	defer file.Close()
	info, err := validFile(file, MaxCheckpointFileBytes)
	if err != nil || !info.ModTime().Equal(time.Unix(0, modified)) {
		return ErrRecovery
	}
	if file.Sync() != nil || d.file.Sync() != nil {
		return ErrStorage
	}
	return nil
}
