//go:build linux || darwin

package managed

import (
	"crypto/rand"
	"encoding/hex"
	"os"
	"path/filepath"

	"golang.org/x/sys/unix"
)

// Set bytes and nanosecond mtime on the private temporary inode before rename.
// A crash therefore leaves either the exact old or exact new planned state.
func replaceV2Timed(dir *privateDir, name string, desired StoreSnapshot, stamp int64, expected string, oldStamp int64, limit int64, hook func(string) error) error {
	if !desired.Exists && (len(desired.Bytes) != 0 || stamp != 0) || desired.Exists && stamp <= 0 || int64(len(desired.Bytes)) > limit {
		return ErrInvalid
	}
	check := func() error {
		current, currentStamp, err := dir.readMetadata(name, limit)
		if err != nil {
			return err
		}
		if Digest(current) != expected || currentStamp != oldStamp {
			return ErrConflict
		}
		return nil
	}
	if err := check(); err != nil {
		return err
	}
	call := func(stage string) error {
		if hook != nil {
			return hook(stage)
		}
		return nil
	}
	if !desired.Exists {
		if err := call("before_rename"); err != nil {
			return err
		}
		if err := check(); err != nil {
			return err
		}
		if err := unix.Unlinkat(int(dir.file.Fd()), name, 0); err != nil && err != unix.ENOENT {
			return ErrStorage
		}
		if err := call("after_rename"); err != nil {
			return err
		}
		if dir.file.Sync() != nil {
			return ErrStorage
		}
		return dir.check()
	}
	var nonce [16]byte
	if _, err := rand.Read(nonce[:]); err != nil {
		return ErrStorage
	}
	tmp := ".txn-" + hex.EncodeToString(nonce[:])
	fd, err := unix.Openat(int(dir.file.Fd()), tmp, unix.O_WRONLY|unix.O_CREAT|unix.O_EXCL|unix.O_NOFOLLOW|unix.O_CLOEXEC, 0600)
	if err != nil {
		return ErrStorage
	}
	file := os.NewFile(uintptr(fd), "restore-private-temporary")
	defer func() { file.Close(); unix.Unlinkat(int(dir.file.Fd()), tmp, 0) }()
	if file.Chmod(0600) != nil {
		return ErrStorage
	}
	if _, err = file.Write(desired.Bytes); err != nil {
		return ErrStorage
	}
	timespec := unix.NsecToTimespec(stamp)
	if unix.UtimesNanoAt(int(dir.file.Fd()), tmp, []unix.Timespec{timespec, timespec}, unix.AT_SYMLINK_NOFOLLOW) != nil || file.Sync() != nil {
		return ErrStorage
	}
	if err = call("after_file_sync"); err != nil {
		return err
	}
	if err = file.Close(); err != nil {
		return ErrStorage
	}
	if err = call("before_rename"); err != nil {
		return err
	}
	if err = check(); err != nil {
		return err
	}
	if unix.Renameat(int(dir.file.Fd()), tmp, int(dir.file.Fd()), name) != nil {
		return ErrStorage
	}
	if err = call("after_rename"); err != nil {
		return err
	}
	if dir.file.Sync() != nil {
		return ErrStorage
	}
	if err = dir.check(); err != nil {
		return err
	}
	return call("after_dir_sync")
}

func v2MatchingFiles(dir *privateDir, pattern string) ([]string, error) {
	names, err := dir.names()
	if err != nil {
		return nil, err
	}
	out := []string{}
	for _, name := range names {
		matches, err := filepath.Match(pattern, name)
		if err != nil {
			return nil, ErrRecovery
		}
		if !matches {
			continue
		}
		var st unix.Stat_t
		if unix.Fstatat(int(dir.file.Fd()), name, &st, unix.AT_SYMLINK_NOFOLLOW) != nil {
			return nil, ErrConflict
		}
		if st.Mode&unix.S_IFMT == unix.S_IFDIR {
			continue
		}
		if st.Mode&unix.S_IFMT != unix.S_IFREG {
			return nil, ErrUnsafePath
		}
		out = append(out, name)
	}
	return out, nil
}
