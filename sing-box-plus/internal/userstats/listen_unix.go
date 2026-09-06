//go:build unix

package userstats

import (
	"io/fs"
	"net"
	"os"
	"path/filepath"
	"syscall"

	E "github.com/sagernet/sing/common/exceptions"
)

// listenUnix 绑定一个本机 UDS，并在绑定前后做 README §4.5 要求的全部检查：
// 父目录、符号链接、旧 socket 与 inode 替换。
func listenUnix(path string, mode os.FileMode) (net.Listener, error) {
	if err := checkParentDirectory(path); err != nil {
		return nil, err
	}
	info, err := os.Lstat(path)
	switch {
	case err == nil:
		if info.Mode()&os.ModeSymlink != 0 {
			return nil, E.New("拒绝绑定符号链接路径：", path)
		}
		if info.Mode()&os.ModeSocket == 0 {
			return nil, E.New("路径已存在且不是 socket，拒绝覆盖：", path)
		}
		// 只有确认是旧 socket 才删除。
		if err = os.Remove(path); err != nil {
			return nil, E.Cause(err, "删除遗留 socket ", path)
		}
	case os.IsNotExist(err):
	default:
		return nil, E.Cause(err, "检查 socket 路径 ", path)
	}

	listener, err := net.Listen("unix", path)
	if err != nil {
		return nil, E.Cause(err, "绑定 socket ", path)
	}
	if err = os.Chmod(path, mode); err != nil {
		listener.Close()
		return nil, E.Cause(err, "设置 socket 权限 ", path)
	}
	// inode 替换检查：绑定与 chmod 之间若被换掉，我们改的就是别人的文件。
	bound, err := os.Lstat(path)
	if err != nil {
		listener.Close()
		return nil, E.Cause(err, "复核 socket ", path)
	}
	if bound.Mode()&os.ModeSocket == 0 || bound.Mode().Perm() != mode.Perm() {
		listener.Close()
		return nil, E.New("socket 在绑定后被替换或权限不符：", path)
	}
	if unixListener, ok := listener.(*net.UnixListener); ok {
		// 由本进程负责在 Close 时删除 socket 文件。
		unixListener.SetUnlinkOnClose(true)
	}
	return listener, nil
}

// checkParentDirectory 要求父目录存在、是目录、且不是符号链接。
func checkParentDirectory(path string) error {
	parent := filepath.Dir(path)
	info, err := os.Lstat(parent)
	if err != nil {
		return E.Cause(err, "检查父目录 ", parent)
	}
	if info.Mode()&os.ModeSymlink != 0 {
		return E.New("父目录是符号链接，拒绝使用：", parent)
	}
	if !info.IsDir() {
		return E.New("父目录不是目录：", parent)
	}
	return nil
}

// checkFileOwner 复核文件属主为当前 euid。
func checkFileOwner(info fs.FileInfo) error {
	raw, ok := info.Sys().(*syscall.Stat_t)
	if !ok {
		return nil
	}
	if int(raw.Uid) != os.Geteuid() {
		return E.New("文件属主不是当前 euid：uid=", raw.Uid, " euid=", os.Geteuid())
	}
	return nil
}

func platformSupported() error { return nil }
