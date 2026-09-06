//go:build unix

package userstats

import (
	"io/fs"
	"net"
	"os"
	"path/filepath"
	"syscall"
	"time"

	E "github.com/sagernet/sing/common/exceptions"
)

// maxUnixPathBytes 取两个平台里更严格的那个，使同一份配置在 Linux 与 darwin 上行为一致。
const maxUnixPathBytes = 103

// listenUnix 绑定一个本机 UDS，并在绑定前后做 README §4.5 要求的全部检查：
// 父目录、符号链接、旧 socket 与 inode 替换。
func listenUnix(path string, mode os.FileMode) (net.Listener, error) {
	// AF_UNIX 的 sun_path 有硬上限（Linux 107、darwin 103 可用字节）。超限时内核只回
	// EINVAL，运维看到的是 "bind: invalid argument"，完全无法归因到路径长度上。
	if len(path) > maxUnixPathBytes {
		return nil, E.New("socket 路径超过 ", maxUnixPathBytes, " 字节上限（实际 ", len(path),
			"）：AF_UNIX 的 sun_path 放不下，请改用更短的路径，例如 /run/sing-box-plus/")
	}
	if err := checkParentDirectory(path); err != nil {
		return nil, err
	}
	// lockfile 先于一切检查取得，并在进程存活期间一直持有。
	//
	// 没有它的话，「探测旧 socket → 删除 → bind」三步之间存在 TOCTOU 窗口：
	// 两个进程可以同时判定对方的 socket 是遗留物，然后互相删掉、各自 bind，
	// 结果是采集端随机连到其中一个，另一个的计数永远无人采集。
	lockFile, err := acquireLock(path)
	if err != nil {
		return nil, err
	}
	defer func() {
		if lockFile != nil {
			lockFile.Close()
		}
	}()

	info, err := os.Lstat(path)
	switch {
	case err == nil:
		if info.Mode()&os.ModeSymlink != 0 {
			return nil, E.New("拒绝绑定符号链接路径：", path)
		}
		if info.Mode()&os.ModeSocket == 0 {
			return nil, E.New("路径已存在且不是 socket，拒绝覆盖：", path)
		}
		// 是 socket 还不够——必须确认它没有人在监听。
		// 直接删掉一个活着的 socket 等于静默劫持另一个进程的端点：
		// 它会继续跑、继续计数，而采集端从此连到我们这里，两边的账都不完整。
		if probeConn, probeErr := net.DialTimeout("unix", path, 500*time.Millisecond); probeErr == nil {
			probeConn.Close()
			return nil, E.New("socket 上已有进程在监听，拒绝覆盖：", path,
				"（同一 listen_path 只能有一个进程）")
		}
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
	// 绑定成功，锁必须继续持有到 listener 关闭为止。
	held := lockFile
	lockFile = nil
	return &lockedListener{Listener: listener, lock: held}, nil
}

// lockedListener 把 lockfile 的生命周期绑到 listener 上。
type lockedListener struct {
	net.Listener
	lock *os.File
}

func (l *lockedListener) Close() error {
	err := l.Listener.Close()
	if l.lock != nil {
		_ = syscall.Flock(int(l.lock.Fd()), syscall.LOCK_UN)
		_ = l.lock.Close()
		l.lock = nil
	}
	return err
}

// acquireLock 以非阻塞方式取得 <path>.lock 的排他锁。
//
// 故意不删除 lock 文件：删除会让「持有锁的进程」与「文件」解绑，另一个进程可以创建同名新文件
// 并成功加锁，锁就失去意义了。留下一个 0 字节文件是这类锁的正常形态。
func acquireLock(path string) (*os.File, error) {
	lockPath := path + ".lock"
	if len(lockPath) > maxUnixPathBytes+16 {
		return nil, E.New("socket 锁文件路径过长：", lockPath)
	}
	file, err := os.OpenFile(lockPath, os.O_CREATE|os.O_RDWR, 0o600)
	if err != nil {
		return nil, E.Cause(err, "打开 socket 锁文件 ", lockPath)
	}
	if err = syscall.Flock(int(file.Fd()), syscall.LOCK_EX|syscall.LOCK_NB); err != nil {
		file.Close()
		return nil, E.New("socket 锁文件已被占用：", lockPath,
			"（同一 listen_path 上已有另一个 sing-box-plus 进程）")
	}
	return file, nil
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
