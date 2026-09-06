// 本文件复制自上游 sing-box 的 cmd/sing-box/cmd_run.go。
//
// 复制而非 import：github.com/sagernet/sing-box/cmd/sing-box 是 package main，Go 禁止导入；
// 而本项目必须保持 run / check / format / version 的 argv 与退出码与上游兼容（README §4.7）。
//
// 来源：github.com/SagerNet/sing-box@0b8995879f29a9b98ee027bc17b75e101445b238（v1.14.0）
// 复制日期：2026-09-06
// 本项目修改：
//   1. readConfigAt 在解码前调用 userstats.ScanRawConfig，把「未编译 with_user_stats」与
//      「配置含 ssm-api」两类失败替换成可读错误（§4.6 第 3 条、第 8 条第一层）；
//   2. 新增 prepareStats：在**进入 run 循环之前**创建进程级 registry 并捕获
//      runtime_id / started_at_unix_ms / sequence，注册致命错误回调（§4.4、§4.6 第 5 条）；
//   3. 新增 reconcileStats：每次（重）建 Box 之前按新配置校验、比对重载不变量并对账
//      active / tombstone（§4.3、§4.4 第 2 条、§4.6 第 11 条）；
//   4. create() 在 box.New() 之后、Start() 之前 AppendTracker——注入点时序是硬约束，
//      Start() 之后追加已实测触发数据竞争（§4.7）。
//
// 本文件是 GPLv3 衍生物，义务见本子目录的 LICENSE 与 THIRD_PARTY_NOTICES.md。

package main

import (
	"context"
	"io"
	"os"
	"os/signal"
	"path/filepath"
	runtimeDebug "runtime/debug"
	"sort"
	"strings"
	"syscall"
	"time"

	"github.com/sagernet/sing-box"
	C "github.com/sagernet/sing-box/constant"
	"github.com/sagernet/sing-box/log"
	"github.com/sagernet/sing-box/option"
	E "github.com/sagernet/sing/common/exceptions"
	"github.com/sagernet/sing/common/json"
	"github.com/sagernet/sing/common/json/badjson"
	"github.com/sagernet/sing/service"

	"sing-box-plus/internal/userstats"

	"github.com/spf13/cobra"
)

// 进程级统计状态。两者都必须活得比任何一个 Box 长：SIGHUP 在同一进程内重建整个 Box
// 与全部 service 实例，挂在 service 上的状态每次重载都会重置（README §4.4）。
var (
	statsRegistry *userstats.Registry
	statsConfig   *userstats.Config
)

var commandRun = &cobra.Command{
	Use:   "run",
	Short: "Run service",
	Run: func(cmd *cobra.Command, args []string) {
		err := run()
		if err != nil {
			log.Fatal(err)
		}
	},
}

func init() {
	mainCommand.AddCommand(commandRun)
}

type OptionsEntry struct {
	content []byte
	path    string
	options option.Options
}

func readConfigAt(path string) (*OptionsEntry, error) {
	var (
		configContent []byte
		err           error
	)
	if path == "stdin" {
		configContent, err = io.ReadAll(os.Stdin)
	} else {
		configContent, err = os.ReadFile(path)
	}
	if err != nil {
		return nil, E.Cause(err, "read config at ", path)
	}
	// 解码前先扫描原始 JSON：上游对未注册 service 类型的错误文案把 service 误写成 "inbound"，
	// 而 ssm-api 未注册时的失败文本同样不可依赖（§4.6 第 3 条、第 8 条第一层）。
	if err = userstats.ScanRawConfig(configContent, statsRegistered); err != nil {
		return nil, E.Cause(err, "check config at ", path)
	}
	options, err := json.UnmarshalExtendedContext[option.Options](globalCtx, configContent)
	if err != nil {
		return nil, E.Cause(err, "decode config at ", path)
	}
	return &OptionsEntry{
		content: configContent,
		path:    path,
		options: options,
	}, nil
}

func readConfig() ([]*OptionsEntry, error) {
	var optionsList []*OptionsEntry
	for _, path := range configPaths {
		optionsEntry, err := readConfigAt(path)
		if err != nil {
			return nil, err
		}
		optionsList = append(optionsList, optionsEntry)
	}
	for _, directory := range configDirectories {
		entries, err := os.ReadDir(directory)
		if err != nil {
			return nil, E.Cause(err, "read config directory at ", directory)
		}
		for _, entry := range entries {
			if !strings.HasSuffix(entry.Name(), ".json") || entry.IsDir() {
				continue
			}
			optionsEntry, err := readConfigAt(filepath.Join(directory, entry.Name()))
			if err != nil {
				return nil, err
			}
			optionsList = append(optionsList, optionsEntry)
		}
	}
	sort.Slice(optionsList, func(i, j int) bool {
		return optionsList[i].path < optionsList[j].path
	})
	return optionsList, nil
}

func readConfigAndMerge() (option.Options, error) {
	optionsList, err := readConfig()
	if err != nil {
		return option.Options{}, err
	}
	return mergeOptionsList(optionsList)
}

func mergeOptionsList(optionsList []*OptionsEntry) (option.Options, error) {
	if len(optionsList) == 1 {
		return optionsList[0].options, nil
	}
	var (
		mergedMessage json.RawMessage
		err           error
	)
	for _, options := range optionsList {
		mergedMessage, err = badjson.MergeJSON(globalCtx, options.options.RawMessage, mergedMessage, false)
		if err != nil {
			return option.Options{}, E.Cause(err, "merge config at ", options.path)
		}
	}
	var mergedOptions option.Options
	err = mergedOptions.UnmarshalJSONContext(globalCtx, mergedMessage)
	if err != nil {
		return option.Options{}, E.Cause(err, "unmarshal merged config")
	}
	return mergedOptions, nil
}

// prepareStats 在进入 run 循环之前建立进程级 registry。
//
// runtime_id、started_at_unix_ms 与 sequence 三者在此刻一次性捕获，此后跨 SIGHUP 不变。
func prepareStats(options option.Options) error {
	config, err := userstats.Validate(options)
	if err != nil {
		return err
	}
	if config == nil {
		// 未配置 user_stats：不创建 registry、exporter 或任何附加包装（§4.6 第 6 条）。
		return nil
	}
	registry, err := userstats.NewRegistry(config.NodeID, config.MaxIdentities)
	if err != nil {
		return err
	}
	// exporter 与数据面同受监督：意外退出、panic 或连续 accept() 失败时整个进程失败退出，
	// 避免「代理仍在转发但统计已消失」（§4.6 第 5 条）。
	registry.SetFatalHandler(func(fatalErr error) {
		log.Fatal(E.Cause(fatalErr, "user_stats 导出面失效"))
	})
	// 启动期超出 max_identities 即失败关闭。
	if err = registry.Reconcile(config.Inbounds, true); err != nil {
		return err
	}
	statsRegistry = registry
	statsConfig = config
	// user_stats 实例从 ctx 取 registry，禁止自建（§4.7）。
	globalCtx = service.ContextWith(globalCtx, registry)
	return nil
}

// reconcileStats 在每次（重）建 Box 之前执行。
func reconcileStats(options option.Options) error {
	if statsRegistry == nil {
		return nil
	}
	config, err := userstats.Validate(options)
	if err != nil {
		return err
	}
	if config == nil {
		return E.New("重载不得移除 user_stats 配置：统计是硬依赖")
	}
	if err = userstats.CheckReloadInvariant(statsConfig, config); err != nil {
		return err
	}
	// SIGHUP 期超限只置 health.identity_limit_reached，不丢弃 lineage（§4.3 第 5 条）。
	if err = statsRegistry.Reconcile(config.Inbounds, false); err != nil {
		return err
	}
	statsConfig = config
	return nil
}

func create(options option.Options) (*box.Box, context.CancelFunc, error) {
	if err := reconcileStats(options); err != nil {
		return nil, nil, E.Cause(err, "reconcile user_stats")
	}
	if disableColor {
		if options.Log == nil {
			options.Log = &option.LogOptions{}
		}
		options.Log.DisableColor = true
	}
	ctx, cancel := context.WithCancel(globalCtx)
	instance, err := box.New(box.Options{
		Context:                    ctx,
		Options:                    options,
		NetworkNamespaceHolderArgs: []string{"/proc/self/exe", commandNetnsHolder.Use},
	})
	if err != nil {
		cancel()
		return nil, nil, E.Cause(err, "create service")
	}
	// 注入点时序是硬约束：route.Router.AppendTracker 是无锁 append，数据面无锁遍历该切片，
	// 因此必须在 Start() 之前追加。在 main 里追加（而不是在自有 service 的构造函数里）
	// 另有一个好处：上游两处 AppendTracker 都在 box.New 体内，只有在 New 之后追加，
	// 本项目的 tracker 才恒定是包装链最外层（README §4.1、§4.7）。
	if statsRegistry != nil {
		instance.Router().AppendTracker(userstats.NewTracker(statsRegistry, log.StdLogger()))
	}

	osSignals := make(chan os.Signal, 1)
	signal.Notify(osSignals, os.Interrupt, syscall.SIGTERM, syscall.SIGHUP)
	defer func() {
		signal.Stop(osSignals)
		close(osSignals)
	}()
	startCtx, finishStart := context.WithCancel(context.Background())
	go func() {
		_, loaded := <-osSignals
		if loaded {
			cancel()
			closeMonitor(startCtx)
		}
	}()
	err = instance.Start()
	finishStart()
	if err != nil {
		cancel()
		return nil, nil, E.Cause(err, "start service")
	}
	return instance, cancel, nil
}

func run() error {
	optionsList, err := readConfig()
	if err != nil {
		return err
	}
	options, err := mergeOptionsList(optionsList)
	if err != nil {
		return err
	}
	err = runInUserNamespaceIfNeeded(options, optionsList)
	if err != nil {
		return err
	}
	// 进程级 registry 必须在 run 循环之外创建、跨 SIGHUP 复用（§4.4）。
	err = prepareStats(options)
	if err != nil {
		return err
	}
	osSignals := make(chan os.Signal, 1)
	signal.Notify(osSignals, os.Interrupt, syscall.SIGTERM, syscall.SIGHUP)
	defer signal.Stop(osSignals)
	for {
		instance, cancel, createErr := create(options)
		if createErr != nil {
			return createErr
		}
		runtimeDebug.FreeOSMemory()
		for {
			osSignal := <-osSignals
			if osSignal == syscall.SIGHUP {
				err = check()
				if err != nil {
					log.Error(E.Cause(err, "reload service"))
					continue
				}
			}
			cancel()
			closeCtx, closed := context.WithCancel(context.Background())
			go closeMonitor(closeCtx)
			err = instance.Close()
			closed()
			if osSignal != syscall.SIGHUP {
				if err != nil {
					log.Error(E.Cause(err, "sing-box did not closed properly"))
				}
				return nil
			}
			break
		}
		options, err = readConfigAndMerge()
		if err != nil {
			return err
		}
	}
}

func closeMonitor(ctx context.Context) {
	time.Sleep(C.FatalStopTimeout)
	select {
	case <-ctx.Done():
		return
	default:
	}
	log.Fatal("sing-box did not close!")
}
