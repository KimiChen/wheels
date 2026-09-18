package userstats

import (
	"context"
	"os"
	"time"

	"github.com/sagernet/sing-box/adapter"
	boxService "github.com/sagernet/sing-box/adapter/service"
	"github.com/sagernet/sing-box/log"
	E "github.com/sagernet/sing/common/exceptions"
	"github.com/sagernet/sing/service"
)

// RegisterService 把自有 user_stats 类型注册进调用方持有的 service registry。
//
// 调用方必须自建 registry 且**不注册** ssmapi，使含 ssm-api 的配置在解析期即失败关闭
// （README §4.6 第 8 条第一层）。
func RegisterService(registry *boxService.Registry) {
	boxService.Register[Options](registry, TypeUserStats, NewService)
}

// Service 是 services[] 中的 user_stats 实例。
//
// 它只负责监听 UDS、读取进程级 registry 并序列化，**不得自行持有或初始化**
// runtime_id / started_at_unix_ms / sequence——SIGHUP 在同一进程内重建全部 service 实例，
// 挂在实例上的状态每次重载都会重置（README §4.4）。
type Service struct {
	tag      string
	options  Options
	logger   log.ContextLogger
	registry *Registry

	exporter *udsServer
	quota    *udsServer
	audit    *auditWriter
}

func NewService(ctx context.Context, logger log.ContextLogger, tag string, options Options) (adapter.Service, error) {
	if err := platformSupported(); err != nil {
		return nil, err
	}
	if err := options.normalize(); err != nil {
		return nil, err
	}
	registry := service.FromContext[*Registry](ctx)
	if registry == nil {
		return nil, E.New("上下文中没有进程级统计 registry：user_stats 禁止自建信封字段（README §4.4、§4.7）")
	}
	if !registry.IsValidationOnly() && registry.NodeID() != options.NodeID {
		return nil, E.New("user_stats.node_id 与进程级 registry 不一致：", options.NodeID, " != ", registry.NodeID())
	}
	return &Service{
		tag:      tag,
		options:  options,
		logger:   logger,
		registry: registry,
	}, nil
}

func (s *Service) Type() string { return TypeUserStats }
func (s *Service) Tag() string  { return s.tag }

// Start 分两阶段。
//
// 审计与配额必须在 Initialize 阶段挂上，而不是 Start 阶段：上游在同一次
// adapter.Start(StartStateStart, s.inbound, s.service) 里先起 inbound 再起 service
// （box.go:578，adapter/lifecycle.go 顺序遍历），而 inbound 在该阶段就绑定监听。
// 两者都放在 Start 阶段的话，从最后一个 listener 起来到本函数返回之间存在一个窗口：
// 期间 admit() 因策略未装而放行任何身份，auditWriterRef() 为 nil 使该窗口内建立的
// 连接**永远**不产生审计记录——即便写入器随后就绪。窗口包含开审计目录、查重名与
// listenUnix 里那次 500ms 的 stale socket 探测，不是微秒级。
//
// Initialize 阶段由 box.go:542 的 preStart() 对含 s.service 在内的全部组件执行，
// 早于任何 inbound 绑定，因此把两者移到这里能把窗口完全消掉，且无需给上游打补丁。
func (s *Service) Start(stage adapter.StartStage) error {
	if s.registry.IsValidationOnly() {
		// check 路径只构造不启动：绝不绑定 socket、绝不开审计文件。
		return nil
	}
	switch stage {
	case adapter.StartStateInitialize:
		return s.attach()
	case adapter.StartStateStart:
		return s.listen()
	}
	return nil
}

// attach 在任何 inbound 绑定监听之前把审计与配额挂到进程级 registry 上。
//
// 注意错误路径：cmd_run.go 的 create() 在 Start 失败时不会调用 instance.Close()，
// 所以这里起的审计协程会随后续阶段失败而泄漏。当前无害——那条路径接着就是进程退出——
// 但若将来有人把「Start 失败不退出」改成可恢复，必须同时在这里补上回收。
func (s *Service) attach() error {
	if s.options.AccessLog != nil {
		audit, err := newAuditWriter(s.registry.NodeID(), s.registry.RuntimeID(),
			*s.options.AccessLog, s.registry.Identities(), s.logger,
			s.registry.noteAuditDropped)
		if err != nil {
			return err
		}
		s.audit = audit
		s.registry.setAudit(audit)
	}
	s.registry.configureQuota(s.options.QuotaControl)
	return nil
}

// listen 绑定两个 UDS 并开始服务。
func (s *Service) listen() error {
	mode := os.FileMode(0o600)
	if s.options.SocketMode == "0660" {
		mode = os.FileMode(0o660)
	}
	listener, err := listenUnix(s.options.ListenPath, mode, s.options.SocketGroup)
	if err != nil {
		s.closeAudit()
		return err
	}
	s.exporter = newUDSServer("user_stats exporter", listener, serverLimits{
		readTimeout:     time.Duration(s.options.ReadTimeout),
		writeTimeout:    time.Duration(s.options.WriteTimeout),
		maxConcurrency:  s.options.MaxConcurrency,
		maxRequestBytes: s.options.MaxRequestBytes,
		allowBody:       false,
	}, s.handleSnapshotRoute, s.logger, s.registry.fatal)
	s.exporter.Serve()

	if s.options.QuotaControl != nil {
		quotaListener, quotaErr := listenUnix(s.options.QuotaControl.ListenPath, mode, s.options.SocketGroup)
		if quotaErr != nil {
			_ = s.exporter.Close()
			s.closeAudit()
			return quotaErr
		}
		s.quota = newUDSServer("user_stats quota", quotaListener, serverLimits{
			readTimeout:     time.Duration(s.options.ReadTimeout),
			writeTimeout:    time.Duration(s.options.WriteTimeout),
			maxConcurrency:  s.options.MaxConcurrency,
			maxRequestBytes: s.options.QuotaControl.MaxRequestBytes,
			allowBody:       true,
		}, s.handleQuotaRoute, s.logger, s.registry.fatal)
		s.quota.Serve()
	}
	return nil
}

func (s *Service) Close() error {
	if s.quota != nil {
		_ = s.quota.Close()
		s.quota = nil
	}
	if s.exporter != nil {
		_ = s.exporter.Close()
		s.exporter = nil
	}
	s.closeAudit()
	return nil
}

func (s *Service) closeAudit() {
	if s.audit == nil {
		return
	}
	s.registry.setAudit(nil)
	_ = s.audit.Close()
	s.audit = nil
}

// handleSnapshotRoute 固定两条路由，且只接受 GET。
//
// 路径版本号与 schema_version 同步推进：本项目只提供当前版本，历史路径（/v1、/v2）一律 404，
// 使误配或未同步升级的采集器立即失败，而不是读到半兼容的 body（README §4.5）。
// health 从三位加到四位（audit_dropped）就是 v2→v3 那次推进的原因。
func (s *Service) handleSnapshotRoute(req *request) (int, []byte) {
	switch req.path {
	case "/v3/snapshot":
		if req.method != "GET" {
			return statusMethodNotAllowed, mustJSON(newErrorBody(statusMethodNotAllowed))
		}
		snapshot, err := s.registry.Snapshot()
		if err != nil {
			return statusInternalServerError, mustJSON(newErrorBody(statusInternalServerError))
		}
		body, err := marshalJSONLine(snapshot)
		if err != nil {
			return statusInternalServerError, mustJSON(newErrorBody(statusInternalServerError))
		}
		return statusOK, body
	case "/healthz":
		if req.method != "GET" {
			return statusMethodNotAllowed, mustJSON(newErrorBody(statusMethodNotAllowed))
		}
		// /healthz 不推进 sequence。
		if s.registry.unhealthy() {
			// 用常量而非字面量：statusText 里没有 503 时，writeResponse 会落到
			// 默认分支写出「HTTP/1.1 503 Unknown」——偏偏这是运维最常 grep 的那个码。
			return statusServiceUnavailable,
				mustJSON(healthBody{SchemaVersion: SchemaVersion, Status: "unhealthy"})
		}
		return statusOK, mustJSON(healthBody{SchemaVersion: SchemaVersion, Status: "ok"})
	default:
		return statusNotFound, mustJSON(newErrorBody(statusNotFound))
	}
}
