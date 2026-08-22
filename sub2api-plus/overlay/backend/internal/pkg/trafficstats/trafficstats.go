// Package trafficstats estimates gateway request/response wire traffic.
package trafficstats

import (
	"context"
	"fmt"
	"io"
	"math"
	"net/http"
	"strconv"
	"strings"
	"sync/atomic"

	"github.com/Wei-Shaw/sub2api/internal/pkg/ctxkey"
)

const (
	defaultSource                = "app_estimate"
	defaultTLSRecordPayloadBytes = 16 * 1024
	defaultTLSRecordOverhead     = 21
	defaultTCPIPHeaderBytes      = 40
	defaultTCPPayloadBytes       = 1460
)

// Config controls byte estimation.
type Config struct {
	Enabled               bool
	Source                string
	TLSRecordPayloadBytes int
	TLSRecordOverhead     int
	TCPIPHeaderBytes      int
	TCPPayloadBytes       int
}

// Snapshot is the final traffic estimate stored on a usage log.
type Snapshot struct {
	RequestBytes          int64
	ResponseBytes         int64
	UpstreamRequestBytes  int64
	UpstreamResponseBytes int64
	Source                string
	Estimated             bool
	RequestBase           int64
	ResponseBase          int64
	UpstreamRequestBase   int64
	UpstreamResponseBase  int64
	TLSEstimated          bool
	UpstreamTLSEstimated  bool
	PacketOverhead        bool
}

// Counter tracks one HTTP request.
type Counter struct {
	cfg Config
	tls bool

	requestHeaderBytes       int64
	requestBodyDeclaredBytes int64
	requestBodyBytes         atomic.Int64
	responseHeaderBytes      atomic.Int64
	responseBodyBytes        atomic.Int64
	responseFinalized        atomic.Bool

	upstreamTLS                       atomic.Bool
	upstreamRequestHeaderBytes        atomic.Int64
	upstreamRequestBodyDeclaredBytes  atomic.Int64
	upstreamRequestBodyBytes          atomic.Int64
	upstreamResponseHeaderBytes       atomic.Int64
	upstreamResponseBodyDeclaredBytes atomic.Int64
	upstreamResponseBodyBytes         atomic.Int64
}

// NormalizeConfig fills safe defaults.
func NormalizeConfig(cfg Config) Config {
	if strings.TrimSpace(cfg.Source) == "" {
		cfg.Source = defaultSource
	}
	if cfg.TLSRecordPayloadBytes <= 0 {
		cfg.TLSRecordPayloadBytes = defaultTLSRecordPayloadBytes
	}
	if cfg.TLSRecordOverhead < 0 {
		cfg.TLSRecordOverhead = 0
	}
	if cfg.TCPIPHeaderBytes < 0 {
		cfg.TCPIPHeaderBytes = 0
	}
	if cfg.TCPPayloadBytes <= 0 {
		cfg.TCPPayloadBytes = defaultTCPPayloadBytes
	}
	return cfg
}

// NewCounter creates a counter and records request line/header bytes.
func NewCounter(cfg Config, r *http.Request) *Counter {
	cfg = NormalizeConfig(cfg)
	c := &Counter{cfg: cfg}
	if r != nil {
		c.tls = requestUsesTLS(r)
		c.requestHeaderBytes = estimateRequestHeadBytes(r)
		if r.ContentLength > 0 {
			c.requestBodyDeclaredBytes = r.ContentLength
		}
	}
	return c
}

// IntoContext stores a counter in context.
func IntoContext(ctx context.Context, counter *Counter) context.Context {
	if ctx == nil {
		ctx = context.Background()
	}
	if counter == nil {
		return ctx
	}
	return context.WithValue(ctx, ctxkey.TrafficCounter, counter)
}

// FromContext returns a counter from context.
func FromContext(ctx context.Context) (*Counter, bool) {
	if ctx == nil {
		return nil, false
	}
	counter, ok := ctx.Value(ctxkey.TrafficCounter).(*Counter)
	return counter, ok && counter != nil
}

// AddRequestBody records request body bytes read by the application.
func (c *Counter) AddRequestBody(n int64) {
	if c == nil || n <= 0 {
		return
	}
	c.requestBodyBytes.Add(n)
}

// AddResponseBody records response body bytes written by the application.
func (c *Counter) AddResponseBody(n int64) {
	if c == nil || n <= 0 {
		return
	}
	c.responseBodyBytes.Add(n)
}

// RecordUpstreamRequest records request line/header bytes and declared body size for one upstream request.
func (c *Counter) RecordUpstreamRequest(req *http.Request) {
	if c == nil || req == nil {
		return
	}
	if requestURLUsesTLS(req) {
		c.upstreamTLS.Store(true)
	}
	c.upstreamRequestHeaderBytes.Add(estimateRequestHeadBytes(req))
	if req.ContentLength > 0 {
		c.upstreamRequestBodyDeclaredBytes.Add(req.ContentLength)
	}
}

// AddUpstreamRequestBody records upstream request body bytes read by net/http.
func (c *Counter) AddUpstreamRequestBody(n int64) {
	if c == nil || n <= 0 {
		return
	}
	c.upstreamRequestBodyBytes.Add(n)
}

// RecordUpstreamResponse records upstream response status line, headers, and declared body size.
func (c *Counter) RecordUpstreamResponse(resp *http.Response) {
	if c == nil || resp == nil {
		return
	}
	c.upstreamResponseHeaderBytes.Add(estimateResponseHeadBytes(resp.StatusCode, resp.Header, resp.Proto))
	if resp.ContentLength > 0 {
		c.upstreamResponseBodyDeclaredBytes.Add(resp.ContentLength)
	}
}

// AddUpstreamResponseBody records upstream response body bytes read from the wire.
func (c *Counter) AddUpstreamResponseBody(n int64) {
	if c == nil || n <= 0 {
		return
	}
	c.upstreamResponseBodyBytes.Add(n)
}

// CountingReadCloser wraps a body and calls add with every positive read size.
type CountingReadCloser struct {
	io.ReadCloser
	Add func(int64)
}

func (r *CountingReadCloser) Read(p []byte) (int, error) {
	n, err := r.ReadCloser.Read(p)
	if n > 0 && r.Add != nil {
		r.Add(int64(n))
	}
	return n, err
}

// FinalizeResponse records response status line and headers once.
func (c *Counter) FinalizeResponse(status int, header http.Header, proto string) {
	if c == nil || !c.responseFinalized.CompareAndSwap(false, true) {
		return
	}
	if status <= 0 {
		status = http.StatusOK
	}
	c.responseHeaderBytes.Store(estimateResponseHeadBytes(status, header, proto))
}

// Snapshot returns estimated wire bytes for both directions.
func (c *Counter) Snapshot() Snapshot {
	if c == nil {
		return Snapshot{}
	}
	requestBase := c.requestHeaderBytes + maxInt64(c.requestBodyDeclaredBytes, c.requestBodyBytes.Load())
	responseBase := c.responseHeaderBytes.Load() + c.responseBodyBytes.Load()
	upstreamRequestBase := c.upstreamRequestHeaderBytes.Load() + maxInt64(c.upstreamRequestBodyDeclaredBytes.Load(), c.upstreamRequestBodyBytes.Load())
	upstreamResponseBase := c.upstreamResponseHeaderBytes.Load() + maxInt64(c.upstreamResponseBodyDeclaredBytes.Load(), c.upstreamResponseBodyBytes.Load())
	upstreamTLS := c.upstreamTLS.Load()
	return Snapshot{
		RequestBytes:          estimateWireBytes(requestBase, c.cfg, c.tls),
		ResponseBytes:         estimateWireBytes(responseBase, c.cfg, c.tls),
		UpstreamRequestBytes:  estimateWireBytes(upstreamRequestBase, c.cfg, upstreamTLS),
		UpstreamResponseBytes: estimateWireBytes(upstreamResponseBase, c.cfg, upstreamTLS),
		Source:                c.cfg.Source,
		Estimated:             true,
		RequestBase:           requestBase,
		ResponseBase:          responseBase,
		UpstreamRequestBase:   upstreamRequestBase,
		UpstreamResponseBase:  upstreamResponseBase,
		TLSEstimated:          c.tls && c.cfg.TLSRecordPayloadBytes > 0 && c.cfg.TLSRecordOverhead > 0,
		UpstreamTLSEstimated:  upstreamTLS && c.cfg.TLSRecordPayloadBytes > 0 && c.cfg.TLSRecordOverhead > 0,
		PacketOverhead:        c.cfg.TCPIPHeaderBytes > 0 && c.cfg.TCPPayloadBytes > 0,
	}
}

// SnapshotFromContext returns a traffic estimate from context.
func SnapshotFromContext(ctx context.Context) (Snapshot, bool) {
	counter, ok := FromContext(ctx)
	if !ok {
		return Snapshot{}, false
	}
	snapshot := counter.Snapshot()
	return snapshot, snapshot.RequestBytes > 0 || snapshot.ResponseBytes > 0 || snapshot.UpstreamRequestBytes > 0 || snapshot.UpstreamResponseBytes > 0
}

func requestUsesTLS(r *http.Request) bool {
	if r == nil {
		return false
	}
	if r.TLS != nil {
		return true
	}
	if strings.EqualFold(strings.TrimSpace(r.Header.Get("X-Forwarded-Proto")), "https") {
		return true
	}
	return strings.EqualFold(strings.TrimSpace(r.Header.Get("X-Forwarded-Ssl")), "on")
}

func requestURLUsesTLS(r *http.Request) bool {
	if r == nil || r.URL == nil {
		return false
	}
	return strings.EqualFold(strings.TrimSpace(r.URL.Scheme), "https")
}

func estimateRequestHeadBytes(r *http.Request) int64 {
	if r == nil {
		return 0
	}
	proto := strings.TrimSpace(r.Proto)
	if proto == "" {
		proto = "HTTP/1.1"
	}
	requestURI := "/"
	if r.URL != nil {
		requestURI = r.URL.RequestURI()
	}
	total := int64(len(r.Method) + 1 + len(requestURI) + 1 + len(proto) + 2)
	host := strings.TrimSpace(r.Host)
	if host == "" && r.URL != nil {
		host = strings.TrimSpace(r.URL.Host)
	}
	if host != "" {
		total += headerLineBytes("Host", host)
	}
	total += headerBytes(r.Header)
	if r.ContentLength >= 0 && r.Header.Get("Content-Length") == "" {
		total += headerLineBytes("Content-Length", strconv.FormatInt(r.ContentLength, 10))
	}
	if len(r.TransferEncoding) > 0 && r.Header.Get("Transfer-Encoding") == "" {
		total += headerLineBytes("Transfer-Encoding", strings.Join(r.TransferEncoding, ", "))
	}
	return total + 2
}

func estimateResponseHeadBytes(status int, header http.Header, proto string) int64 {
	proto = strings.TrimSpace(proto)
	if proto == "" {
		proto = "HTTP/1.1"
	}
	statusText := http.StatusText(status)
	if statusText == "" {
		statusText = "status"
	}
	total := int64(len(proto) + 1 + len(strconv.Itoa(status)) + 1 + len(statusText) + 2)
	total += headerBytes(header)
	if header == nil || header.Get("Date") == "" {
		total += int64(len("Date: Mon, 02 Jan 2006 15:04:05 GMT\r\n"))
	}
	return total + 2
}

func headerBytes(header http.Header) int64 {
	var total int64
	for name, values := range header {
		for _, value := range values {
			total += headerLineBytes(name, value)
		}
	}
	return total
}

func headerLineBytes(name, value string) int64 {
	return int64(len(name) + len(": ") + len(value) + len("\r\n"))
}

func estimateWireBytes(base int64, cfg Config, tls bool) int64 {
	if base <= 0 {
		return 0
	}
	wire := base
	if tls && cfg.TLSRecordPayloadBytes > 0 && cfg.TLSRecordOverhead > 0 {
		records := int64(math.Ceil(float64(base) / float64(cfg.TLSRecordPayloadBytes)))
		wire += records * int64(cfg.TLSRecordOverhead)
	}
	if cfg.TCPIPHeaderBytes > 0 && cfg.TCPPayloadBytes > 0 {
		packets := int64(math.Ceil(float64(wire) / float64(cfg.TCPPayloadBytes)))
		wire += packets * int64(cfg.TCPIPHeaderBytes)
	}
	return wire
}

func maxInt64(a, b int64) int64 {
	if a > b {
		return a
	}
	return b
}

func (s Snapshot) String() string {
	return fmt.Sprintf("request=%d response=%d source=%s estimated=%t", s.RequestBytes, s.ResponseBytes, s.Source, s.Estimated)
}
