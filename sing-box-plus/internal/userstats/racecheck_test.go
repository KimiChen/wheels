package userstats

import (
	"context"
	"os"
	"os/exec"
	"strings"
	"testing"

	box "github.com/sagernet/sing-box"
	"github.com/sagernet/sing-box/log"
	"github.com/sagernet/sing-box/option"
	singjson "github.com/sagernet/sing/common/json"
	"github.com/sagernet/sing/service"

	"sing-box-plus/internal/testenv"
)

const lateAppendEnv = "SING_BOX_PLUS_LATE_APPEND_CHILD"

// TestAppendTrackerAfterStartIsRacy 是 §4.7「注入点时序是硬约束」的负向用例。
//
// route.Router.AppendTracker 是无锁 append（route/router.go:272，trackers 字段无配套锁），
// 而数据面在 route/route.go:170 与 :302 无锁遍历该切片。因此「Start() 之前追加」不是风格偏好
// 而是正确性约束。本用例把违规写法放进子进程执行，父进程断言 race detector 确实报了竞争——
// 直接在本进程里做会让整个测试套变红，那样就没人敢保留这条断言了。
func TestAppendTrackerAfterStartIsRacy(t *testing.T) {
	if os.Getenv(lateAppendEnv) == "1" {
		runLateAppendChild(t)
		return
	}
	if !raceEnabled {
		t.Skip("未启用 -race：本用例只在 race detector 下有意义")
	}
	executable, err := os.Executable()
	if err != nil {
		t.Fatalf("定位测试二进制失败：%v", err)
	}
	command := exec.Command(executable, "-test.run", "^TestAppendTrackerAfterStartIsRacy$", "-test.v")
	command.Env = append(os.Environ(), lateAppendEnv+"=1", "GORACE=halt_on_error=1")
	output, err := command.CombinedOutput()
	if err == nil {
		t.Fatalf("Start() 之后追加 tracker 本应被 race detector 报出，子进程却正常退出：\n%s", output)
	}
	if !strings.Contains(string(output), "DATA RACE") {
		t.Fatalf("子进程失败但不是因为数据竞争：\n%s", output)
	}
}

// runLateAppendChild 故意在 Start() 之后追加 tracker，并同时驱动真实流量。
func runLateAppendChild(t *testing.T) {
	dir := shortTempDir(t)
	sock := sockPath(dir, "stats.sock")
	serverPort := testenv.FreePort(t)
	echoHost, echoPort := testenv.StartTCPEcho(t)

	ctx := serverContext(context.Background())
	options, err := singjson.UnmarshalExtendedContext[option.Options](ctx, []byte(vlessServerConfig(serverPort, sock)))
	if err != nil {
		t.Fatalf("解析配置失败：%v", err)
	}
	config, err := Validate(options)
	if err != nil {
		t.Fatalf("校验失败：%v", err)
	}
	registry, err := NewRegistry(config.NodeID, config.MaxIdentities)
	if err != nil {
		t.Fatalf("创建 registry 失败：%v", err)
	}
	if err = registry.Reconcile(config.Inbounds, true); err != nil {
		t.Fatalf("对账失败：%v", err)
	}
	ctx = service.ContextWith(ctx, registry)
	instanceCtx, cancel := context.WithCancel(ctx)
	defer cancel()
	instance, err := box.New(box.Options{Context: instanceCtx, Options: options})
	if err != nil {
		t.Fatalf("创建 Box 失败：%v", err)
	}
	defer instance.Close()
	if err = instance.Start(); err != nil {
		t.Fatalf("启动失败：%v", err)
	}
	testenv.WaitPort(t, "127.0.0.1", serverPort)
	clientPort := testenv.FreePort(t)
	testenv.StartUpstreamClient(t, vlessClientConfig(clientPort, serverPort, testUUID1, echoHost, echoPort))

	done := make(chan struct{})
	go func() {
		defer close(done)
		for index := 0; index < 200; index++ {
			_ = tryRoundTrip(clientPort, []byte("ping"))
		}
	}()
	for index := 0; index < 200; index++ {
		// 违规写法：Start() 之后追加。
		instance.Router().AppendTracker(NewTracker(registry, log.NewNOPFactory().Logger()))
	}
	<-done
}
