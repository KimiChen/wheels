package userstats

import (
	"bufio"
	"errors"
	"io"
	"net"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/sagernet/sing-box/log"
)

// 本文件实现 README §4.5 的传输层：HTTP/1.1-over-Unix-stream，每连接单请求单响应，
// 禁 keep-alive 与 query。刻意不用 net/http：请求大小、并发数、读写超时、方法与版本的
// 失败关闭判定都必须逐条可断言，而 net/http 会替我们「宽容地」处理掉其中一部分。

const (
	statusOK                  = 200
	statusBadRequest          = 400
	statusNotFound            = 404
	statusMethodNotAllowed    = 405
	statusRequestTimeout      = 408
	statusPayloadTooLarge     = 413
	statusConflict            = 409
	statusTooManyRequests     = 429
	statusInternalServerError = 500
	statusVersionNotSupported = 505
)

var statusText = map[int]string{
	statusOK:                  "OK",
	statusBadRequest:          "Bad Request",
	statusNotFound:            "Not Found",
	statusMethodNotAllowed:    "Method Not Allowed",
	statusRequestTimeout:      "Request Timeout",
	statusConflict:            "Conflict",
	statusPayloadTooLarge:     "Payload Too Large",
	statusTooManyRequests:     "Too Many Requests",
	statusInternalServerError: "Internal Server Error",
	statusVersionNotSupported: "HTTP Version Not Supported",
}

const (
	maxRequestLineBytes = 8 * 1024
	maxHeaderCount      = 64
	maxHeaderLineBytes  = 8 * 1024
)

type request struct {
	method string
	path   string
	body   []byte
}

type handlerFunc func(req *request) (status int, body []byte)

type serverLimits struct {
	readTimeout     time.Duration
	writeTimeout    time.Duration
	maxConcurrency  int
	maxRequestBytes int
	allowBody       bool
}

// udsServer 是一个最小 HTTP/1.1 服务端。
//
// 单个畸形、超时或超限的请求只影响该连接，不影响代理转发（README §4.5）。
type udsServer struct {
	name     string
	listener net.Listener
	limits   serverLimits
	handler  handlerFunc
	logger   log.ContextLogger
	onFatal  func(error)

	slots     chan struct{}
	closeOnce sync.Once
	closed    chan struct{}
	waitGroup sync.WaitGroup
}

func newUDSServer(name string, listener net.Listener, limits serverLimits, handler handlerFunc, logger log.ContextLogger, onFatal func(error)) *udsServer {
	return &udsServer{
		name:     name,
		listener: listener,
		limits:   limits,
		handler:  handler,
		logger:   logger,
		onFatal:  onFatal,
		slots:    make(chan struct{}, limits.maxConcurrency),
		closed:   make(chan struct{}),
	}
}

func (s *udsServer) Serve() {
	s.waitGroup.Add(1)
	go s.acceptLoop()
}

// acceptLoop 与数据面同受监督：连续 accept() 失败时上报致命错误，由 main 让整个进程失败退出，
// 避免「代理仍在转发但统计已消失」（README §4.6 第 5 条）。
func (s *udsServer) acceptLoop() {
	defer s.waitGroup.Done()
	defer func() {
		if recovered := recover(); recovered != nil {
			s.fatal(errors.New(s.name + " accept 循环 panic"))
		}
	}()
	var consecutiveFailures int
	for {
		conn, err := s.listener.Accept()
		if err != nil {
			select {
			case <-s.closed:
				return
			default:
			}
			consecutiveFailures++
			if consecutiveFailures >= 8 {
				s.fatal(err)
				return
			}
			time.Sleep(time.Duration(consecutiveFailures) * 10 * time.Millisecond)
			continue
		}
		consecutiveFailures = 0
		select {
		case s.slots <- struct{}{}:
		default:
			// 并发上限：立即以 429 拒绝，不排队。429 与连接被直接关闭对采集端而言
			// 都是「可重试且不得入账」（README §5.1）。
			s.writeResponse(conn, statusTooManyRequests, mustJSON(newErrorBody(statusTooManyRequests)))
			_ = conn.Close()
			continue
		}
		s.waitGroup.Add(1)
		go func() {
			defer s.waitGroup.Done()
			defer func() { <-s.slots }()
			defer func() {
				if recovered := recover(); recovered != nil && s.logger != nil {
					s.logger.Error(s.name, " 连接处理 panic：", recovered)
				}
			}()
			s.handleConn(conn)
		}()
	}
}

func (s *udsServer) fatal(err error) {
	if s.logger != nil {
		s.logger.Error(s.name, " 致命错误：", err)
	}
	if s.onFatal != nil {
		s.onFatal(err)
	}
}

func (s *udsServer) Close() error {
	var err error
	s.closeOnce.Do(func() {
		close(s.closed)
		err = s.listener.Close()
		s.waitGroup.Wait()
	})
	return err
}

func (s *udsServer) handleConn(conn net.Conn) {
	defer conn.Close()
	_ = conn.SetReadDeadline(time.Now().Add(s.limits.readTimeout))
	reader := bufio.NewReaderSize(conn, 16*1024)
	req, status := s.readRequest(reader)
	if status != statusOK {
		s.writeResponse(conn, status, mustJSON(newErrorBody(status)))
		s.lingeringDrain(conn)
		return
	}
	respStatus, body := s.handler(req)
	s.writeResponse(conn, respStatus, body)
}

func (s *udsServer) readRequest(reader *bufio.Reader) (*request, int) {
	line, status := readLimitedLine(reader, maxRequestLineBytes)
	if status != statusOK {
		return nil, status
	}
	parts := strings.Split(line, " ")
	if len(parts) != 3 {
		return nil, statusBadRequest
	}
	method, target, version := parts[0], parts[1], parts[2]
	if version != "HTTP/1.1" {
		return nil, statusVersionNotSupported
	}
	// 禁 query：带 query 的请求一律 400，不做「忽略未知参数」的宽容处理。
	if strings.ContainsAny(target, "?#") {
		return nil, statusBadRequest
	}
	if target == "" || target[0] != '/' {
		return nil, statusBadRequest
	}

	var contentLength int
	var sawContentLength bool
	for index := 0; ; index++ {
		if index > maxHeaderCount {
			return nil, statusBadRequest
		}
		headerLine, headerStatus := readLimitedLine(reader, maxHeaderLineBytes)
		if headerStatus != statusOK {
			return nil, headerStatus
		}
		if headerLine == "" {
			break
		}
		colon := strings.IndexByte(headerLine, ':')
		if colon <= 0 {
			return nil, statusBadRequest
		}
		name := strings.ToLower(strings.TrimSpace(headerLine[:colon]))
		value := strings.TrimSpace(headerLine[colon+1:])
		switch name {
		case "content-length":
			parsed, err := strconv.Atoi(value)
			if err != nil || parsed < 0 {
				return nil, statusBadRequest
			}
			contentLength = parsed
			sawContentLength = true
		case "transfer-encoding":
			// 不支持分块：本端点只接受一次性、长度已知的请求体。
			return nil, statusBadRequest
		}
	}

	if !s.limits.allowBody {
		if sawContentLength && contentLength > 0 {
			return nil, statusBadRequest
		}
		return &request{method: method, path: target}, statusOK
	}
	if contentLength > s.limits.maxRequestBytes {
		return nil, statusPayloadTooLarge
	}
	body := make([]byte, contentLength)
	if contentLength > 0 {
		if _, err := io.ReadFull(reader, body); err != nil {
			if isTimeout(err) {
				return nil, statusRequestTimeout
			}
			return nil, statusBadRequest
		}
	}
	return &request{method: method, path: target, body: body}, statusOK
}

func (s *udsServer) writeResponse(conn net.Conn, status int, body []byte) {
	_ = conn.SetWriteDeadline(time.Now().Add(s.limits.writeTimeout))
	text := statusText[status]
	if text == "" {
		text = "Unknown"
	}
	header := "HTTP/1.1 " + strconv.Itoa(status) + " " + text + "\r\n" +
		"Content-Type: application/json\r\n" +
		"Content-Length: " + strconv.Itoa(len(body)) + "\r\n" +
		"Connection: close\r\n" +
		"\r\n"
	if _, err := conn.Write([]byte(header)); err != nil {
		return
	}
	_, _ = conn.Write(body)
}

// readLimitedLine 读一行 CRLF 结尾的文本，长度有界。
//
// 刻意不用 bufio.Reader.ReadLine：它在已经读到部分数据时会把底层错误**置为 nil**
// （src/bufio/bufio.go 的 ReadLine：len(line) != 0 时 err = nil）。
// 于是「客户端写了半个请求行就挂住」会被读成一次成功的完整行——超时被误报成 400，
// 而截断的请求行还会被当作合法输入继续解析。
// lingeringDrain 在错误响应之后有界地丢弃客户端仍在发送的数据。
//
// 关闭一个接收队列里还有数据的 socket 会发 RST，客户端的接收缓冲连同我们刚写回的错误响应
// 一起被丢掉——运维看到的是 "connection reset by peer" 而不是 413。
// 这里读掉一小段再关，能让「略微超限」的请求正常收到响应。
//
// 它**消不掉**这个窗口：真正超出很多的请求体在有界预算内排不完，关闭时仍会 RST。
// 因此 docs/API.md 明确要求客户端把「连接被重置」与错误码同等对待。
// 预算刻意很小——为一个已经被判定超限的请求无限读下去，正是限额要防的事。
func (s *udsServer) lingeringDrain(conn net.Conn) {
	const budget = 256 * 1024
	_ = conn.SetReadDeadline(time.Now().Add(200 * time.Millisecond))
	discard := make([]byte, 32*1024)
	var total int
	for total < budget {
		n, err := conn.Read(discard)
		total += n
		if err != nil {
			return
		}
	}
}

func readLimitedLine(reader *bufio.Reader, limit int) (string, int) {
	var builder strings.Builder
	for {
		chunk, err := reader.ReadSlice('\n')
		if len(chunk) > 0 {
			if builder.Len()+len(chunk) > limit {
				return "", statusPayloadTooLarge
			}
			builder.Write(chunk)
		}
		switch {
		case err == nil:
			line := strings.TrimSuffix(builder.String(), "\n")
			return strings.TrimSuffix(line, "\r"), statusOK
		case errors.Is(err, bufio.ErrBufferFull):
			// 行还没读完，继续；长度上限已在上面逐段核过。
			continue
		case isTimeout(err):
			return "", statusRequestTimeout
		default:
			// 包括 EOF：没有 CRLF 就不是一个完整的行，不得当作合法输入。
			return "", statusBadRequest
		}
	}
}

func isTimeout(err error) bool {
	var netErr net.Error
	return errors.As(err, &netErr) && netErr.Timeout()
}

func mustJSON(value any) []byte {
	body, err := marshalJSONLine(value)
	if err != nil {
		return []byte("{\"schema_version\":2,\"error\":{\"code\":500}}\n")
	}
	return body
}
