//go:build !unix

package userstats

import (
	"io/fs"
	"net"
	"os"

	E "github.com/sagernet/sing/common/exceptions"
)

// 非 Unix 平台没有本项目要求的 UDS 语义（权限位、unlink-on-close、inode 复核），
// 因此 user_stats 在这些平台上必须明确报错而不是降级（README §4.6 第 3 条）。
func listenUnix(path string, mode os.FileMode) (net.Listener, error) {
	return nil, errPlatformUnsupported
}

func checkParentDirectory(path string) error { return errPlatformUnsupported }

func checkFileOwner(info fs.FileInfo) error { return errPlatformUnsupported }

func platformSupported() error { return errPlatformUnsupported }

var errPlatformUnsupported = E.New("user_stats 只支持 Unix 平台（需要 Unix domain socket 与文件权限语义）")
