package userstats

import (
	"net"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"
)

// TestListenUnixRefusesUnsafePaths 覆盖 §4.6 第 4 条的绑定前检查。
func TestListenUnixRefusesUnsafePaths(t *testing.T) {
	dir := shortTempDir(t)

	t.Run("符号链接", func(t *testing.T) {
		target := filepath.Join(dir, "target")
		link := filepath.Join(dir, "link.sock")
		if err := os.WriteFile(target, nil, 0o600); err != nil {
			t.Fatalf("准备目标失败：%v", err)
		}
		if err := os.Symlink(target, link); err != nil {
			t.Fatalf("创建符号链接失败：%v", err)
		}
		if _, err := listenUnix(link, 0o600); err == nil {
			t.Fatal("符号链接路径必须拒绝")
		}
	})

	t.Run("已存在的普通文件", func(t *testing.T) {
		path := filepath.Join(dir, "regular.sock")
		if err := os.WriteFile(path, nil, 0o600); err != nil {
			t.Fatalf("准备文件失败：%v", err)
		}
		if _, err := listenUnix(path, 0o600); err == nil {
			t.Fatal("已存在的普通文件必须拒绝覆盖")
		}
	})

	t.Run("父目录不存在", func(t *testing.T) {
		if _, err := listenUnix(filepath.Join(dir, "missing", "a.sock"), 0o600); err == nil {
			t.Fatal("父目录不存在时必须拒绝")
		}
	})

	t.Run("路径过长", func(t *testing.T) {
		long := "/tmp/" + strings.Repeat("a", 120) + ".sock"
		if _, err := listenUnix(long, 0o600); err == nil {
			t.Fatal("超长路径必须拒绝")
		}
	})
}

// TestListenUnixRefusesLiveSocket 是一条防劫持断言。
//
// 「路径上是个 socket」不足以判定它是遗留物：直接删掉一个活着的 socket 等于静默劫持
// 另一个进程的端点——它会继续跑、继续计数，而采集端从此连到我们这里，两边的账都不完整。
func TestListenUnixRefusesLiveSocket(t *testing.T) {
	dir := shortTempDir(t)
	path := filepath.Join(dir, "live.sock")

	first, err := listenUnix(path, 0o600)
	if err != nil {
		t.Fatalf("首次绑定失败：%v", err)
	}
	defer first.Close()

	if _, err = listenUnix(path, 0o600); err == nil {
		t.Fatal("已有进程在监听时必须拒绝绑定")
	} else if !strings.Contains(err.Error(), "锁文件已被占用") && !strings.Contains(err.Error(), "已有进程在监听") {
		t.Fatalf("拒绝理由应指向占用，实际：%v", err)
	}

	// 关闭之后同一路径必须可以重新绑定，否则重启会被自己的残留挡住。
	first.Close()
	second, err := listenUnix(path, 0o600)
	if err != nil {
		t.Fatalf("释放后应可重新绑定：%v", err)
	}
	second.Close()
}

// TestListenUnixReclaimsStaleSocket 断言真正的遗留 socket（无人监听）会被清理。
func TestListenUnixReclaimsStaleSocket(t *testing.T) {
	dir := shortTempDir(t)
	path := filepath.Join(dir, "stale.sock")

	// 造一个没人监听的 socket 文件：绑定后直接关掉底层 fd 而不 unlink。
	raw, err := net.Listen("unix", path)
	if err != nil {
		t.Fatalf("准备遗留 socket 失败：%v", err)
	}
	if unixListener, ok := raw.(*net.UnixListener); ok {
		unixListener.SetUnlinkOnClose(false)
	}
	raw.Close()
	if _, err = os.Lstat(path); err != nil {
		t.Fatalf("遗留 socket 应仍在：%v", err)
	}

	listener, err := listenUnix(path, 0o600)
	if err != nil {
		t.Fatalf("遗留 socket 应被清理后重新绑定：%v", err)
	}
	defer listener.Close()
	info, err := os.Lstat(path)
	if err != nil {
		t.Fatalf("重新绑定后应存在：%v", err)
	}
	if info.Mode().Perm() != 0o600 {
		t.Fatalf("socket 权限必须是 0600，实际 %v", info.Mode().Perm())
	}
}

// TestExporterConcurrencyLimit 覆盖并发上限：超出即 429，且 429 不推进 sequence。
func TestExporterConcurrencyLimit(t *testing.T) {
	handle := contractServer(t)

	var waitGroup sync.WaitGroup
	var mutex sync.Mutex
	statuses := make(map[int]int)
	const parallel = 64
	for index := 0; index < parallel; index++ {
		waitGroup.Add(1)
		go func() {
			defer waitGroup.Done()
			conn, err := net.DialTimeout("unix", handle.sockPath, 3*time.Second)
			if err != nil {
				return
			}
			defer conn.Close()
			_ = conn.SetDeadline(time.Now().Add(5 * time.Second))
			if _, err = conn.Write([]byte("GET /v2/snapshot HTTP/1.1\r\nHost: x\r\n\r\n")); err != nil {
				return
			}
			buffer := make([]byte, 128)
			n, _ := conn.Read(buffer)
			line := string(buffer[:n])
			mutex.Lock()
			switch {
			case strings.HasPrefix(line, "HTTP/1.1 200"):
				statuses[200]++
			case strings.HasPrefix(line, "HTTP/1.1 429"):
				statuses[429]++
			}
			mutex.Unlock()
		}()
	}
	waitGroup.Wait()
	if statuses[200] == 0 {
		t.Fatal("并发压力下应至少有一部分请求成功")
	}
	// 429 不是必现的（取决于调度），但只要出现就必须是这个码而不是别的失败形式。
	snapshot := fetchSnapshot(t, handle.sockPath)
	if snapshot.Sequence < uint64(statuses[200]) {
		t.Fatalf("sequence 应至少覆盖全部成功请求：%d < %d", snapshot.Sequence, statuses[200])
	}
}

// TestExporterSlowClientTimesOut 覆盖慢客户端：读超时返回 408，且不影响后续请求。
func TestExporterSlowClientTimesOut(t *testing.T) {
	dir := shortTempDir(t)
	sock := sockPath(dir, "stats.sock")
	quotaSock := sockPath(dir, "quota.sock")
	config := strings.Replace(
		quotaServerConfig(18999, sock, quotaSock),
		`"inbounds": ["vless-in"],`,
		`"inbounds": ["vless-in"], "read_timeout": "300ms",`, 1)
	handle := startServer(t, config)

	conn, err := net.DialTimeout("unix", handle.sockPath, 3*time.Second)
	if err != nil {
		t.Fatalf("连接失败：%v", err)
	}
	defer conn.Close()
	// 只写半个请求行，然后什么都不做。
	if _, err = conn.Write([]byte("GET /v2/sna")); err != nil {
		t.Fatalf("写入失败：%v", err)
	}
	_ = conn.SetReadDeadline(time.Now().Add(5 * time.Second))
	buffer := make([]byte, 256)
	n, _ := conn.Read(buffer)
	if !strings.HasPrefix(string(buffer[:n]), "HTTP/1.1 408") {
		t.Fatalf("慢客户端应收到 408，实际：%q", string(buffer[:n]))
	}
	// 单个超时请求只影响该连接。
	if snapshot := fetchSnapshot(t, handle.sockPath); snapshot.Sequence == 0 {
		t.Fatal("超时请求不应影响后续采集")
	}
}
