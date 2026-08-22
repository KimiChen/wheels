package repository

import (
	"net/http"

	"github.com/Wei-Shaw/sub2api/internal/pkg/trafficstats"
)

// httpClientWithTrafficRecording 在 HTTP 客户端最外层安装上游流量统计（fork 定制）。
// 语义与原先在 Do/DoWithTLS 中显式调用记录函数一致：
// 每个逻辑请求只统计一次，Grok 访问拒绝回退等内部重试不会重复计数。
// 通过客户端包装而非修改上游执行路径，将与上游的冲突面收敛到一行。
func httpClientWithTrafficRecording(client *http.Client) *http.Client {
	if client == nil {
		return nil
	}
	clone := *client
	base := clone.Transport
	if base == nil {
		base = http.DefaultTransport
	}
	clone.Transport = &trafficRecordingTransport{base: base}
	return &clone
}

type trafficRecordingTransport struct {
	base http.RoundTripper
}

func (t *trafficRecordingTransport) RoundTrip(req *http.Request) (*http.Response, error) {
	counter, ok := trafficstats.FromContext(req.Context())
	if !ok {
		return t.base.RoundTrip(req)
	}

	counter.RecordUpstreamRequest(req)
	if req.Body != nil {
		req.Body = &trafficstats.CountingReadCloser{ReadCloser: req.Body, Add: counter.AddUpstreamRequestBody}
	}

	resp, err := t.base.RoundTrip(req)
	if err != nil {
		return nil, err
	}
	if resp != nil {
		counter.RecordUpstreamResponse(resp)
		if resp.Body != nil {
			resp.Body = &trafficstats.CountingReadCloser{ReadCloser: resp.Body, Add: counter.AddUpstreamResponseBody}
		}
	}
	return resp, nil
}
