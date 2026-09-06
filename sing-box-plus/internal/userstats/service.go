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

func (s *Service) Start(stage adapter.StartStage) error {
	if stage != adapter.StartStateStart {
		return nil
	}
	if s.registry.IsValidationOnly() {
		// check 路径只构造不启动：绝不绑定 socket、绝不开审计文件。
		return nil
	}
	if s.options.AccessLog != nil {
		audit, err := newAuditWriter(s.registry.NodeID(), s.registry.RuntimeID(), *s.options.AccessLog, s.logger)
		if err != nil {
			return err
		}
		s.audit = audit
		s.registry.setAudit(audit)
	}
	mode := os.FileMode(0o600)
	if s.options.SocketMode == "0660" {
		mode = os.FileMode(0o660)
	}
	listener, err := listenUnix(s.options.ListenPath, mode)
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
		s.registry.configureQuota(*s.options.QuotaControl)
		quotaListener, quotaErr := listenUnix(s.options.QuotaControl.ListenPath, mode)
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
// 路径版本号与 schema_version 同步推进：本项目不提供 /v1/snapshot，请求该路径返回 404，
// 使误配的采集器立即失败，而不是读到半兼容的 body（README §4.5）。
func (s *Service) handleSnapshotRoute(req *request) (int, []byte) {
	switch req.path {
	case "/v2/snapshot":
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
			return 503, mustJSON(healthBody{SchemaVersion: SchemaVersion, Status: "unhealthy"})
		}
		return statusOK, mustJSON(healthBody{SchemaVersion: SchemaVersion, Status: "ok"})
	default:
		return statusNotFound, mustJSON(newErrorBody(statusNotFound))
	}
}
