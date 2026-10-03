//go:build linux || darwin

package migrationbundle

import (
	"io"
	"os"
	"path/filepath"
	"strings"

	"golang.org/x/sys/unix"
)

type directory struct {
	file *os.File
	path string
}

func pathFD(path string) (int, error) {
	if !filepath.IsAbs(path) || filepath.Clean(path) != path {
		return -1, ErrBundle
	}
	fd, err := unix.Open("/", unix.O_RDONLY|unix.O_DIRECTORY|unix.O_CLOEXEC, 0)
	if err != nil {
		return -1, ErrBundle
	}
	if path == "/" {
		return fd, nil
	}
	for _, part := range strings.Split(strings.TrimPrefix(path, "/"), "/") {
		next, err := unix.Openat(fd, part, unix.O_RDONLY|unix.O_DIRECTORY|unix.O_NOFOLLOW|unix.O_CLOEXEC, 0)
		unix.Close(fd)
		if err != nil {
			return -1, ErrBundle
		}
		fd = next
	}
	return fd, nil
}

func openDirectory(path string) (*directory, error) {
	fd, err := pathFD(path)
	if err != nil || path == "/" {
		if fd >= 0 {
			unix.Close(fd)
		}
		return nil, ErrBundle
	}
	d := &directory{file: os.NewFile(uintptr(fd), "migration-directory"), path: path}
	if d.check() != nil {
		d.close()
		return nil, ErrBundle
	}
	return d, nil
}

func createDirectory(path string) (*directory, error) {
	if !filepath.IsAbs(path) || filepath.Clean(path) != path || path == "/" {
		return nil, ErrBundle
	}
	parent, err := pathFD(filepath.Dir(path))
	if err != nil {
		return nil, ErrBundle
	}
	defer unix.Close(parent)
	if unix.Mkdirat(parent, filepath.Base(path), 0700) != nil || unix.Fsync(parent) != nil {
		return nil, ErrBundle
	}
	fd, err := unix.Openat(parent, filepath.Base(path), unix.O_RDONLY|unix.O_DIRECTORY|unix.O_NOFOLLOW|unix.O_CLOEXEC, 0)
	if err != nil {
		return nil, ErrBundle
	}
	d := &directory{file: os.NewFile(uintptr(fd), "migration-directory"), path: path}
	if d.check() != nil {
		d.close()
		return nil, ErrBundle
	}
	return d, nil
}

func (d *directory) check() error {
	var pinned, current unix.Stat_t
	if unix.Fstat(int(d.file.Fd()), &pinned) != nil || pinned.Mode&unix.S_IFMT != unix.S_IFDIR || pinned.Mode&0777 != 0700 || pinned.Uid != uint32(os.Geteuid()) {
		return ErrBundle
	}
	fd, err := pathFD(d.path)
	if err != nil {
		return ErrBundle
	}
	defer unix.Close(fd)
	if unix.Fstat(fd, &current) != nil || current.Dev != pinned.Dev || current.Ino != pinned.Ino {
		return ErrBundle
	}
	return nil
}

func (d *directory) write(name string, data []byte) error {
	if d.check() != nil || filepath.Base(name) != name {
		return ErrBundle
	}
	fd, err := unix.Openat(int(d.file.Fd()), name, unix.O_WRONLY|unix.O_CREAT|unix.O_EXCL|unix.O_NOFOLLOW|unix.O_CLOEXEC, 0600)
	if err != nil {
		return ErrBundle
	}
	f := os.NewFile(uintptr(fd), "migration-material")
	defer f.Close()
	var st unix.Stat_t
	if unix.Fstat(fd, &st) != nil || st.Mode&0777 != 0600 || st.Uid != uint32(os.Geteuid()) || st.Nlink != 1 {
		return ErrBundle
	}
	if _, err = f.Write(data); err != nil || f.Sync() != nil || unix.Fstat(fd, &st) != nil || st.Mode&0777 != 0600 || st.Nlink != 1 {
		return ErrBundle
	}
	return d.check()
}

func (d *directory) read(name string, max int) ([]byte, error) {
	if max < 0 || d.check() != nil || filepath.Base(name) != name {
		return nil, ErrBundle
	}
	fd, err := unix.Openat(int(d.file.Fd()), name, unix.O_RDONLY|unix.O_NOFOLLOW|unix.O_NONBLOCK|unix.O_CLOEXEC, 0)
	if err != nil {
		return nil, ErrBundle
	}
	f := os.NewFile(uintptr(fd), "migration-material")
	defer f.Close()
	var st unix.Stat_t
	if unix.Fstat(fd, &st) != nil || st.Mode&unix.S_IFMT != unix.S_IFREG || st.Mode&0777 != 0600 || st.Uid != uint32(os.Geteuid()) || st.Nlink != 1 || st.Size < 0 || st.Size > int64(max) {
		return nil, ErrBundle
	}
	before, err := f.Stat()
	if err != nil {
		return nil, ErrBundle
	}
	data, err := io.ReadAll(io.LimitReader(f, int64(max)+1))
	if err != nil || len(data) > max {
		return nil, ErrBundle
	}
	after, err := f.Stat()
	var pathStat unix.Stat_t
	if err != nil || before.Size() != after.Size() || !before.ModTime().Equal(after.ModTime()) || unix.Fstatat(int(d.file.Fd()), name, &pathStat, unix.AT_SYMLINK_NOFOLLOW) != nil || pathStat.Dev != st.Dev || pathStat.Ino != st.Ino || pathStat.Mode&0777 != 0600 || pathStat.Nlink != 1 || pathStat.Uid != uint32(os.Geteuid()) || d.check() != nil {
		return nil, ErrBundle
	}
	return data, nil
}

func (d *directory) only(allowed map[string]bool) error {
	// Open a fresh descriptor so prior operations never influence directory offset.
	fd, err := pathFD(d.path)
	if err != nil {
		return ErrBundle
	}
	f := os.NewFile(uintptr(fd), "migration-directory")
	defer f.Close()
	entries, err := f.ReadDir(len(allowed) + 1)
	if err != nil && err != io.EOF || len(entries) != len(allowed) {
		return ErrBundle
	}
	for _, entry := range entries {
		if !allowed[entry.Name()] || entry.IsDir() {
			return ErrBundle
		}
	}
	return d.check()
}

func (d *directory) sync() error {
	if d.check() != nil || d.file.Sync() != nil {
		return ErrBundle
	}
	return d.check()
}
func (d *directory) close() { _ = d.file.Close() }
