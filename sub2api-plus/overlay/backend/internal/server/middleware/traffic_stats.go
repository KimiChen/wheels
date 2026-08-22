package middleware

import (
	"io"
	"net/http"
	"strings"

	"github.com/Wei-Shaw/sub2api/internal/config"
	"github.com/Wei-Shaw/sub2api/internal/pkg/trafficstats"
	"github.com/gin-gonic/gin"
)

// gatewayTrafficExactPaths 是直接注册在引擎上的根级网关路由（精确匹配）。
var gatewayTrafficExactPaths = map[string]struct{}{
	"/responses":        {},
	"/alpha/search":     {},
	"/chat/completions": {},
	"/embeddings":       {},
}

// gatewayTrafficPathPrefixes 是网关路由分组及根级通配路由的路径前缀。
// 与 RegisterGatewayRoutes 中的路由一一对应；上游新增网关路由时需同步补充。
var gatewayTrafficPathPrefixes = []string{
	"/v1/",
	"/v1beta/",
	"/backend-api/codex/",
	"/antigravity/v1/",
	"/antigravity/v1beta/",
	"/responses/",
	"/images/",
	"/videos/",
}

// IsGatewayTrafficPath 判断请求路径是否属于网关流量统计范围。
func IsGatewayTrafficPath(path string) bool {
	if _, ok := gatewayTrafficExactPaths[path]; ok {
		return true
	}
	for _, prefix := range gatewayTrafficPathPrefixes {
		if strings.HasPrefix(path, prefix) {
			return true
		}
	}
	return false
}

// TrafficStatsGateway 与 TrafficStats 行为一致，但仅对网关路径生效，其他路径直接放行。
// 在引擎级一次性注册（RegisterGatewayRoutes 顶部 r.Use）后即可覆盖全部网关路由，
// 避免在每条路由/每个分组上插入中间件，降低与上游代码的冲突面。
func TrafficStatsGateway(cfg config.TrafficConfig) gin.HandlerFunc {
	inner := TrafficStats(cfg)
	return func(c *gin.Context) {
		if c == nil || c.Request == nil || !IsGatewayTrafficPath(c.Request.URL.Path) {
			c.Next()
			return
		}
		inner(c)
	}
}

// TrafficStats records app-observable HTTP bytes and estimates TLS/TCP/IP overhead.
func TrafficStats(cfg config.TrafficConfig) gin.HandlerFunc {
	statsCfg := trafficstats.NormalizeConfig(trafficstats.Config{
		Enabled:               cfg.Enabled,
		Source:                cfg.Source,
		TLSRecordPayloadBytes: cfg.TLSRecordPayloadBytes,
		TLSRecordOverhead:     cfg.TLSRecordOverheadBytes,
		TCPIPHeaderBytes:      cfg.TCPIPHeaderBytes,
		TCPPayloadBytes:       cfg.TCPPayloadBytes,
	})
	if !statsCfg.Enabled {
		return func(c *gin.Context) { c.Next() }
	}
	return func(c *gin.Context) {
		if c == nil || c.Request == nil {
			c.Next()
			return
		}
		counter := trafficstats.NewCounter(statsCfg, c.Request)
		if c.Request.Body != nil {
			c.Request.Body = &trafficCountingReadCloser{ReadCloser: c.Request.Body, counter: counter}
		}
		c.Writer = &trafficResponseWriter{
			ResponseWriter: c.Writer,
			counter:        counter,
			proto:          c.Request.Proto,
		}
		c.Request = c.Request.WithContext(trafficstats.IntoContext(c.Request.Context(), counter))
		c.Next()
		counter.FinalizeResponse(c.Writer.Status(), c.Writer.Header(), c.Request.Proto)
	}
}

type trafficCountingReadCloser struct {
	io.ReadCloser
	counter *trafficstats.Counter
}

func (r *trafficCountingReadCloser) Read(p []byte) (int, error) {
	n, err := r.ReadCloser.Read(p)
	if n > 0 {
		r.counter.AddRequestBody(int64(n))
	}
	return n, err
}

type trafficResponseWriter struct {
	gin.ResponseWriter
	counter *trafficstats.Counter
	proto   string
}

func (w *trafficResponseWriter) WriteHeader(statusCode int) {
	w.counter.FinalizeResponse(statusCode, w.Header(), w.proto)
	w.ResponseWriter.WriteHeader(statusCode)
}

func (w *trafficResponseWriter) WriteHeaderNow() {
	status := w.Status()
	if status <= 0 {
		status = http.StatusOK
	}
	w.counter.FinalizeResponse(status, w.Header(), w.proto)
	w.ResponseWriter.WriteHeaderNow()
}

func (w *trafficResponseWriter) Write(data []byte) (int, error) {
	status := w.Status()
	if status <= 0 {
		status = http.StatusOK
	}
	w.counter.FinalizeResponse(status, w.Header(), w.proto)
	n, err := w.ResponseWriter.Write(data)
	if n > 0 {
		w.counter.AddResponseBody(int64(n))
	}
	return n, err
}

func (w *trafficResponseWriter) WriteString(s string) (int, error) {
	status := w.Status()
	if status <= 0 {
		status = http.StatusOK
	}
	w.counter.FinalizeResponse(status, w.Header(), w.proto)
	n, err := w.ResponseWriter.WriteString(s)
	if n > 0 {
		w.counter.AddResponseBody(int64(n))
	}
	return n, err
}
