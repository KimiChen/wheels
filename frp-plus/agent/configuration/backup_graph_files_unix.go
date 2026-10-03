//go:build linux || darwin

package configuration

import (
	"context"
	"errors"
	"io"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"syscall"

	"github.com/fatedier/frp/extension/frpmonitor/agent/backupmanifest"
	"golang.org/x/sys/unix"
)

type backupPin struct {
	path string
	file *os.File
}
type backupDiskView struct {
	policy   BackupGraphPolicy
	pins     map[string]*backupPin
	identity map[string]os.FileInfo
}

func backupDirectoryFD(path string) (*os.File, error) {
	fd, err := unix.Open("/", unix.O_RDONLY|unix.O_DIRECTORY|unix.O_CLOEXEC, 0)
	if err != nil {
		return nil, backupErr("source_unavailable")
	}
	for _, part := range strings.Split(strings.TrimPrefix(path, "/"), "/") {
		if part == "" {
			continue
		}
		next, e := unix.Openat(fd, part, unix.O_RDONLY|unix.O_DIRECTORY|unix.O_NOFOLLOW|unix.O_CLOEXEC, 0)
		unix.Close(fd)
		if e != nil {
			return nil, backupErr("source_unavailable")
		}
		fd = next
	}
	return os.NewFile(uintptr(fd), "backup-directory"), nil
}

func newBackupDiskView(policy BackupGraphPolicy) (*backupDiskView, error) {
	v := &backupDiskView{policy: policy, pins: map[string]*backupPin{}, identity: map[string]os.FileInfo{}}
	paths := []string{}
	for _, root := range policy.Roots {
		paths = append(paths, root.Path)
	}
	for _, mapping := range policy.Mappings {
		paths = append(paths, filepath.Dir(mapping.Path))
	}
	for _, path := range paths {
		if v.pins[path] != nil {
			continue
		}
		file, err := backupDirectoryFD(path)
		if err != nil {
			v.close()
			return nil, err
		}
		info, err := file.Stat()
		if err != nil || !backupSafeInfo(info, true) {
			file.Close()
			v.close()
			return nil, backupErr("source_unsafe")
		}
		v.pins[path] = &backupPin{path: path, file: file}
	}
	cwd, _, err := v.parent(policy.WorkingDir, true)
	if err != nil {
		v.close()
		return nil, err
	}
	info, err := cwd.Stat()
	cwd.Close()
	if err != nil || !backupSafeInfo(info, true) {
		v.close()
		return nil, backupErr("source_unsafe")
	}
	v.identity[policy.WorkingDir] = info
	return v, nil
}
func backupSafeInfo(info os.FileInfo, directory bool) bool {
	if info == nil || directory != info.IsDir() || !directory && !info.Mode().IsRegular() || info.Mode().Perm()&0022 != 0 || info.Mode()&(os.ModeSetuid|os.ModeSetgid|os.ModeSticky) != 0 {
		return false
	}
	if !directory && (info.Mode().Perm()&0400 == 0 || info.Mode().Perm()&0111 != 0) {
		return false
	}
	stat, ok := info.Sys().(*syscall.Stat_t)
	return ok && stat.Uid == uint32(os.Geteuid()) && (directory || stat.Nlink == 1)
}
func (v *backupDiskView) close() {
	for _, pin := range v.pins {
		_ = pin.file.Close()
	}
}
func (v *backupDiskView) check() error {
	for _, pin := range v.pins {
		current, err := backupDirectoryFD(pin.path)
		if err != nil {
			return backupErr("source_changed")
		}
		a, ea := pin.file.Stat()
		b, eb := current.Stat()
		current.Close()
		if ea != nil || eb != nil || !os.SameFile(a, b) || !backupSafeInfo(b, true) {
			return backupErr("source_changed")
		}
	}
	return nil
}
func (v *backupDiskView) parent(path string, directory bool) (*os.File, string, error) {
	if _, err := v.policy.logical(path, directory); err != nil {
		return nil, "", err
	}
	var pin *backupPin
	for _, root := range v.policy.Roots {
		if backupWithin(path, root.Path) {
			pin = v.pins[root.Path]
			break
		}
	}
	if pin == nil {
		pin = v.pins[filepath.Dir(path)]
	}
	if pin == nil {
		return nil, "", backupErr("path_unauthorized")
	}
	rel, err := filepath.Rel(pin.path, path)
	if err != nil {
		return nil, "", backupErr("path_unauthorized")
	}
	parts := strings.Split(rel, string(filepath.Separator))
	limit := len(parts) - 1
	if directory {
		limit = len(parts)
	}
	fd, err := unix.Openat(int(pin.file.Fd()), ".", unix.O_RDONLY|unix.O_DIRECTORY|unix.O_CLOEXEC, 0)
	if err != nil {
		return nil, "", backupErr("source_unavailable")
	}
	for _, part := range parts[:limit] {
		if part == "." {
			continue
		}
		next, e := unix.Openat(fd, part, unix.O_RDONLY|unix.O_DIRECTORY|unix.O_NOFOLLOW|unix.O_CLOEXEC, 0)
		unix.Close(fd)
		if e != nil {
			return nil, "", backupErr("source_unavailable")
		}
		fd = next
	}
	name := ""
	if !directory {
		name = parts[len(parts)-1]
	}
	return os.NewFile(uintptr(fd), "backup-parent"), name, nil
}
func (v *backupDiskView) read(ctx context.Context, path string) (backupSourceFile, error) {
	if err := ctx.Err(); err != nil {
		return backupSourceFile{}, err
	}
	parent, name, err := v.parent(path, false)
	if err != nil {
		return backupSourceFile{}, err
	}
	defer parent.Close()
	fd, err := unix.Openat(int(parent.Fd()), name, unix.O_RDONLY|unix.O_NOFOLLOW|unix.O_NONBLOCK|unix.O_CLOEXEC, 0)
	if errors.Is(err, unix.ENOENT) {
		v.identity[path] = nil
		return backupSourceFile{}, nil
	}
	if err != nil {
		return backupSourceFile{}, backupErr("source_unsafe")
	}
	file := os.NewFile(uintptr(fd), "backup-source")
	defer file.Close()
	before, err := file.Stat()
	if err != nil || !backupSafeInfo(before, false) || before.ModTime().UnixNano() <= 0 {
		return backupSourceFile{}, backupErr("source_unsafe")
	}
	if before.Size() > v.policy.Limits.FileBytes {
		return backupSourceFile{}, backupErr("limit_exceeded")
	}
	data, err := io.ReadAll(io.LimitReader(file, v.policy.Limits.FileBytes+1))
	if err != nil {
		return backupSourceFile{}, backupErr("source_unavailable")
	}
	if int64(len(data)) > v.policy.Limits.FileBytes {
		return backupSourceFile{}, backupErr("limit_exceeded")
	}
	after, err := file.Stat()
	if err != nil || !backupSameInfo(before, after) {
		return backupSourceFile{}, backupErr("source_changed")
	}
	var pathStat unix.Stat_t
	stat := after.Sys().(*syscall.Stat_t)
	if unix.Fstatat(int(parent.Fd()), name, &pathStat, unix.AT_SYMLINK_NOFOLLOW) != nil || pathStat.Ino != stat.Ino || uint64(pathStat.Dev) != uint64(stat.Dev) {
		return backupSourceFile{}, backupErr("source_changed")
	}
	if err := ctx.Err(); err != nil {
		return backupSourceFile{}, err
	}
	v.identity[path] = after
	return backupSourceFile{data: data, exists: true, mode: uint32(after.Mode().Perm()), modified: after.ModTime().UnixNano()}, nil
}
func backupSameInfo(a, b os.FileInfo) bool {
	return a != nil && b != nil && os.SameFile(a, b) && a.Size() == b.Size() && a.Mode() == b.Mode() && a.ModTime().Equal(b.ModTime())
}
func (v *backupDiskView) directory(ctx context.Context, path string) ([]backupmanifest.Entry, error) {
	file, _, err := v.parent(path, true)
	if err != nil {
		return nil, err
	}
	defer file.Close()
	before, err := file.Stat()
	if err != nil || !backupSafeInfo(before, true) {
		return nil, backupErr("source_unsafe")
	}
	entries := []backupmanifest.Entry{}
	for {
		if err := ctx.Err(); err != nil {
			return nil, err
		}
		batch, err := file.ReadDir(64)
		if err != nil && err != io.EOF {
			return nil, backupErr("source_unavailable")
		}
		if len(entries)+len(batch) > v.policy.Limits.Entries {
			return nil, backupErr("limit_exceeded")
		}
		for _, entry := range batch {
			if !backupEntryName(entry.Name()) {
				return nil, backupErr("source_unsafe")
			}
			kind := "other"
			if entry.IsDir() {
				kind = "directory"
			} else if entry.Type().IsRegular() {
				kind = "file"
			}
			entries = append(entries, backupmanifest.Entry{Name: entry.Name(), Kind: kind})
		}
		if err == io.EOF {
			break
		}
	}
	after, err := file.Stat()
	if err != nil || !backupSameInfo(before, after) {
		return nil, backupErr("source_changed")
	}
	sort.Slice(entries, func(i, j int) bool { return entries[i].Name < entries[j].Name })
	v.identity[path] = after
	return entries, nil
}
func (v *backupDiskView) sameIdentity(other *backupDiskView) bool {
	if len(v.identity) != len(other.identity) {
		return false
	}
	for path, a := range v.identity {
		b, ok := other.identity[path]
		if !ok || (a == nil) != (b == nil) || a != nil && !backupSameInfo(a, b) {
			return false
		}
	}
	return true
}
