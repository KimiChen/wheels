// capacity is a bounded synthetic load test for a real frp-monitor-server binary.
// Build this file from the prepared upstream Go module; see CAPACITY.md.
package main

import (
	"bufio"
	"context"
	"crypto/rand"
	"crypto/sha256"
	"database/sql"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/url"
	"os"
	"os/exec"
	"os/signal"
	"path/filepath"
	"runtime"
	"sort"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"syscall"
	"time"

	"github.com/fatedier/frp/extension/frpmonitor/shared"
	"github.com/gorilla/websocket"
	_ "modernc.org/sqlite"
)

const maxResponse = 8 << 20

type options struct {
	server, parent, nodes               string
	seconds, grace, maximum, rss, procs int
	cpu                                 float64
}
type credential struct {
	ID   string `json:"agent_id"`
	Name string `json:"name"`
	Hash string `json:"token_sha256"`
}
type counters struct{ sent, writeErrors, readErrors, events, sseErrors atomic.Uint64 }
type storage struct {
	State   string `json:"state"`
	Dropped uint64 `json:"dropped"`
}
type result struct {
	Profile        string  `json:"profile"`
	Nodes          int     `json:"nodes"`
	Seconds        int     `json:"measurement_seconds"`
	Expected       uint64  `json:"expected_reports"`
	Sent           uint64  `json:"successful_socket_writes"`
	Persisted      uint64  `json:"persisted_reports"`
	StoredNodes    int     `json:"persisted_nodes"`
	WriteErrors    uint64  `json:"write_errors"`
	ReadErrors     uint64  `json:"unexpected_connection_closes"`
	APIRequests    int     `json:"public_api_requests"`
	APIErrors      int     `json:"public_api_errors"`
	APIP50MS       float64 `json:"public_api_p50_ms"`
	APIP95MS       float64 `json:"public_api_p95_ms"`
	APIMaxMS       float64 `json:"public_api_max_ms"`
	SSEEvents      uint64  `json:"sse_snapshots"`
	SSEErrors      uint64  `json:"sse_errors"`
	Fresh          int     `json:"final_fresh_online_nodes"`
	Storage        storage `json:"storage"`
	Resources      bool    `json:"linux_process_resources_available"`
	CPUSeconds     float64 `json:"server_cpu_seconds"`
	CPUAverage     float64 `json:"server_average_cpu_percent_one_core"`
	RSSPeak        uint64  `json:"server_peak_rss_bytes_sampled"`
	HarnessRSSPeak uint64  `json:"harness_peak_rss_bytes_sampled"`
	DBPeak         uint64  `json:"database_wal_shm_peak_bytes_sampled"`
	DBFinal        uint64  `json:"database_final_bytes"`
	Integrity      string  `json:"sqlite_integrity_check"`
	WallSeconds    float64 `json:"case_wall_seconds"`
	Passed         bool    `json:"passed"`
}
type snapshot struct {
	Nodes []struct {
		Session   string `json:"session"`
		Freshness string `json:"freshness"`
	} `json:"nodes"`
}
type process struct {
	command *exec.Cmd
	done    chan struct{}
	err     error
	stopped bool
}

func main() {
	var o options
	flag.StringVar(&o.server, "server", "", "path to the real monitor-server binary")
	flag.StringVar(&o.parent, "directory", "", "existing private output directory (0700, never production data)")
	flag.StringVar(&o.nodes, "nodes", "100,500", "comma-separated sequential case sizes, each 1..500")
	flag.IntVar(&o.seconds, "seconds", 60, "whole seconds at one report per node per second, 2..300")
	flag.IntVar(&o.grace, "grace", 5, "drain seconds before SIGTERM, 2..8")
	flag.IntVar(&o.maximum, "max-seconds", 300, "hard overall wall time including startup, 15..1800")
	flag.IntVar(&o.rss, "max-rss-mib", 768, "abort if sampled server RSS exceeds this, 32..4096")
	flag.Float64Var(&o.cpu, "max-cpu-seconds", 180, "abort case after this much measured server CPU time")
	flag.IntVar(&o.procs, "gomaxprocs", 2, "Go parallelism for child and harness; cgroup required for a hard CPU limit")
	flag.Parse()
	if err := execute(o); err != nil {
		// The harness does not include HTTP headers, configuration, tokens or child logs in errors.
		fmt.Fprintln(os.Stderr, "capacity: "+err.Error())
		os.Exit(1)
	}
}

func execute(o options) error {
	if o.server == "" || o.parent == "" || o.seconds < 2 || o.seconds > 300 || o.grace < 2 || o.grace > 8 || o.maximum < 15 || o.maximum > 1800 || o.rss < 32 || o.rss > 4096 || o.cpu < 1 || o.cpu > 3600 || o.procs < 1 || o.procs > 16 {
		return errors.New("invalid bounded options")
	}
	if len(strings.Split(o.nodes, ",")) > 4 {
		return errors.New("at most four sequential cases are allowed")
	}
	var sizes []int
	for _, raw := range strings.Split(o.nodes, ",") {
		n, err := strconv.Atoi(raw)
		if err != nil || n < 1 || n > 500 {
			return errors.New("node count must be 1..500")
		}
		sizes = append(sizes, n)
	}
	var err error
	o.server, err = filepath.Abs(o.server)
	if err != nil {
		return errors.New("invalid binary path")
	}
	o.parent, err = filepath.Abs(o.parent)
	if err != nil {
		return errors.New("invalid output directory")
	}
	resolved, err := filepath.EvalSymlinks(o.parent)
	if err != nil || resolved != o.parent {
		return errors.New("output directory must exist and contain no symlinks")
	}
	info, err := os.Lstat(o.parent)
	if err != nil || !info.IsDir() || info.Mode().Perm()&0077 != 0 {
		return errors.New("output directory must be private (0700)")
	}
	runtime.GOMAXPROCS(o.procs)
	base, cancel := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer cancel()
	ctx, timeout := context.WithTimeout(base, time.Duration(o.maximum)*time.Second)
	defer timeout()
	for _, n := range sizes {
		r, err := runCase(ctx, o, n)
		if err != nil {
			return fmt.Errorf("%d-node case: %w", n, err)
		}
		if err := json.NewEncoder(os.Stdout).Encode(r); err != nil {
			return errors.New("cannot write result")
		}
		if !r.Passed {
			return errors.New("acceptance checks failed; inspect the non-secret JSON result")
		}
	}
	return nil
}

func private(path string, value []byte) error { return os.WriteFile(path, value, 0600) }
func randomToken() (string, error) {
	b := make([]byte, 32)
	if _, err := rand.Read(b); err != nil {
		return "", err
	}
	return base64.RawURLEncoding.EncodeToString(b), nil
}
func ok[T any](v T) shared.Field[T]       { return shared.Field[T]{Value: &v, Quality: shared.QualityOK} }
func unsupported[T any]() shared.Field[T] { return shared.Field[T]{Quality: shared.QualityUnsupported} }
func facts() shared.Facts {
	return shared.Facts{Scope: shared.ScopeHost, Hostname: ok("synthetic-capacity"), OS: ok("Synthetic Linux"), Kernel: ok("synthetic"), Arch: ok("amd64"), Virt: ok("synthetic"), CPUName: ok("synthetic"), CPUCores: ok(uint32(4)), AgentVersion: ok("capacity-harness"), IPv4: unsupported[string](), IPv6: unsupported[string](), MemTotal: ok(uint64(8 << 30)), SwapTotal: ok(uint64(0)), DiskTotal: ok(uint64(100 << 30))}
}
func metrics(sequence uint64) shared.Metrics {
	return shared.Metrics{Scope: shared.ScopeHost, CPU: ok(float64(sequence % 75)), Load: ok([]float64{.1, .2, .3}), MemTotal: ok(uint64(8 << 30)), MemUsed: ok(uint64(2 << 30)), SwapTotal: ok(uint64(0)), SwapUsed: ok(uint64(0)), DiskTotal: ok(uint64(100 << 30)), DiskUsed: ok(uint64(10 << 30)), NetRX: ok(uint64(1024)), NetTX: ok(uint64(512)), NetRXTotal: ok(sequence * 1024), NetTXTotal: ok(sequence * 512), BootID: ok("synthetic-boot"), Iface: ok("synthetic0"), Uptime: ok(sequence), TCP: ok(uint64(5)), UDP: ok(uint64(1)), Procs: ok(uint64(50))}
}
func meta(session string, sequence uint64) shared.Meta {
	return shared.Meta{Schema: shared.SchemaVersion, SessionID: session, Sequence: sequence, CollectedAt: time.Now().UTC().Format(time.RFC3339Nano)}
}
func wait(ctx context.Context, d time.Duration) error {
	timer := time.NewTimer(d)
	defer timer.Stop()
	select {
	case <-ctx.Done():
		return errors.New("time budget exhausted or interrupted")
	case <-timer.C:
		return nil
	}
}
func reserve() (net.Listener, int, error) {
	l, err := net.Listen("tcp4", "127.0.0.1:0")
	if err != nil {
		return nil, 0, err
	}
	return l, l.Addr().(*net.TCPAddr).Port, nil
}

func (p *process) stop() error {
	if p.stopped {
		return nil
	}
	p.stopped = true
	select {
	case <-p.done:
		return errors.New("server exited before shutdown")
	default:
	}
	_ = p.command.Process.Signal(syscall.SIGTERM)
	select {
	case <-p.done:
		if p.err != nil {
			return errors.New("server did not exit cleanly")
		}
		return nil
	case <-time.After(5 * time.Second):
		_ = p.command.Process.Kill()
		<-p.done
		return errors.New("server required forced shutdown")
	}
}

func getJSON(ctx context.Context, client *http.Client, endpoint string, dst any) error {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, endpoint, nil)
	if err != nil {
		return errors.New("invalid API request")
	}
	resp, err := client.Do(req)
	if err != nil {
		return errors.New("API request failed")
	}
	defer resp.Body.Close()
	if resp.StatusCode != 200 {
		return errors.New("API returned non-200")
	}
	data, err := io.ReadAll(io.LimitReader(resp.Body, maxResponse+1))
	if err != nil || len(data) > maxResponse {
		return errors.New("API response limit or read failure")
	}
	if json.Unmarshal(data, dst) != nil {
		return errors.New("API returned invalid JSON")
	}
	return nil
}

func runCase(ctx context.Context, o options, count int) (result, error) {
	begin := time.Now()
	r := result{Profile: "synthetic-metrics-1Hz-loopback-WS-no-TLS-no-FRP-tunnels", Nodes: count, Seconds: o.seconds, Expected: uint64(count * o.seconds)}
	dir, err := os.MkdirTemp(o.parent, fmt.Sprintf("case-%d-", count))
	if err != nil {
		return r, errors.New("cannot create private case directory")
	}
	// Case directories remain private for inspection; no production path is read or modified.
	credentials := make([]credential, count)
	tokens := make([]string, count)
	for i := range credentials {
		token, err := randomToken()
		if err != nil {
			return r, errors.New("random source failed")
		}
		tokens[i] = token
		sum := sha256.Sum256([]byte(token))
		credentials[i] = credential{ID: fmt.Sprintf("capacity-%04d", i), Name: fmt.Sprintf("Synthetic %04d", i), Hash: hex.EncodeToString(sum[:])}
	}
	encoded, _ := json.Marshal(credentials)
	credFile := filepath.Join(dir, "credentials.json")
	if private(credFile, encoded) != nil {
		return r, errors.New("cannot write credentials")
	}
	frp, frpPort, err := reserve()
	if err != nil {
		return r, errors.New("cannot reserve FRP port")
	}
	defer frp.Close()
	listener, port, err := reserve()
	if err != nil {
		return r, errors.New("cannot reserve monitor port")
	}
	defer listener.Close()
	frpToken, err := randomToken()
	if err != nil {
		return r, errors.New("random source failed")
	}
	database := filepath.Join(dir, "history.sqlite")
	config := fmt.Sprintf("bindAddr = \"127.0.0.1\"\nproxyBindAddr = \"127.0.0.1\"\nbindPort = %d\nauth.method = \"token\"\nauth.token = %q\nlog.to = \"console\"\nlog.level = \"warn\"\nlog.disablePrintColor = true\n[monitor]\nenabled = true\nbindAddr = \"127.0.0.1\"\nbindPort = %d\nserverID = \"capacity-synthetic\"\ncredentialsFile = %q\nreportIntervalSeconds = 1\ndatabaseFile = %q\nretentionDays = 1\n", frpPort, frpToken, port, credFile, database)
	configFile := filepath.Join(dir, "server.toml")
	if private(configFile, []byte(config)) != nil {
		return r, errors.New("cannot write private server config")
	}
	log, err := os.OpenFile(filepath.Join(dir, "server.log"), os.O_CREATE|os.O_EXCL|os.O_WRONLY, 0600)
	if err != nil {
		return r, errors.New("cannot create private log")
	}
	defer log.Close()
	cmd := exec.Command(o.server, "-c", configFile)
	cmd.Dir = dir
	cmd.Stdout = log
	cmd.Stderr = log
	cmd.Env = []string{"PATH=/usr/bin:/bin", "GOMAXPROCS=" + strconv.Itoa(o.procs)}
	frp.Close()
	listener.Close()
	if cmd.Start() != nil {
		return r, errors.New("cannot start server")
	}
	p := &process{command: cmd, done: make(chan struct{})}
	go func() { p.err = cmd.Wait(); close(p.done) }()
	defer p.stop()
	transport := &http.Transport{MaxIdleConns: 8, MaxIdleConnsPerHost: 4}
	defer transport.CloseIdleConnections()
	client := &http.Client{Timeout: 3 * time.Second, Transport: transport}
	base := fmt.Sprintf("http://127.0.0.1:%d", port)
	ready := false
	for i := 0; i < 100; i++ {
		var s snapshot
		if getJSON(ctx, client, base+"/api/public/v1/nodes", &s) == nil && len(s.Nodes) == count {
			ready = true
			break
		}
		select {
		case <-p.done:
			return r, errors.New("server exited during startup")
		default:
		}
		if err := wait(ctx, 100*time.Millisecond); err != nil {
			return r, err
		}
	}
	if !ready {
		return r, errors.New("public API did not become ready")
	}
	loadCtx, cancel := context.WithCancel(ctx)
	defer cancel()
	var c counters
	var shutting atomic.Bool
	var readers sync.WaitGroup
	connections := make([]*websocket.Conn, 0, count)
	defer func() {
		shutting.Store(true)
		for _, conn := range connections {
			_ = conn.Close()
		}
		readers.Wait()
	}()
	dialer := websocket.Dialer{HandshakeTimeout: 3 * time.Second}
	for i, token := range tokens {
		conn, response, err := dialer.DialContext(ctx, fmt.Sprintf("ws://127.0.0.1:%d/agent/v1/ws", port), http.Header{"Authorization": []string{"Bearer " + token}})
		if response != nil && err != nil {
			response.Body.Close()
		}
		if err != nil {
			return r, errors.New("synthetic handshake failed")
		}
		connections = append(connections, conn)
		conn.SetReadLimit(shared.MaxFrameBytes)
		hello := shared.Hello{Meta: meta(fmt.Sprintf("capacity-session-%04d", i), 1), Capabilities: []string{"metrics.v1"}, Facts: facts()}
		if hello.Validate() != nil {
			return r, errors.New("invalid synthetic hello")
		}
		_ = conn.SetWriteDeadline(time.Now().Add(3 * time.Second))
		if conn.WriteJSON(map[string]any{"jsonrpc": "2.0", "id": "hello", "method": "hello", "params": hello}) != nil {
			return r, errors.New("hello write failed")
		}
		_ = conn.SetReadDeadline(time.Now().Add(3 * time.Second))
		var answer struct {
			Result shared.HelloResult `json:"result"`
		}
		if conn.ReadJSON(&answer) != nil || answer.Result.SessionID != hello.SessionID || answer.Result.ReportInterval != 1 {
			return r, errors.New("hello not accepted at 1 Hz")
		}
		_ = conn.SetReadDeadline(time.Time{})
		readers.Add(1)
		go func(conn *websocket.Conn) {
			defer readers.Done()
			for {
				if _, _, err := conn.ReadMessage(); err != nil {
					if !shutting.Load() {
						c.readErrors.Add(1)
					}
					return
				}
			}
		}(conn)
		if err := wait(ctx, 55*time.Millisecond); err != nil {
			return r, err
		}
	}
	// All connections are established before the measurement, respecting handshake admission limits.
	json.NewEncoder(os.Stdout).Encode(map[string]any{"phase": "measuring", "nodes": count, "seconds": o.seconds})
	measuredAt := time.Now()
	ticks := clockTicks()
	baseline, _, procErr := procStats(cmd.Process.Pid, ticks)
	r.Resources = procErr == nil
	if runtime.GOOS == "linux" && !r.Resources {
		return r, errors.New("Linux process resource measurements unavailable")
	}
	var background sync.WaitGroup
	background.Add(1)
	go func() { defer background.Done(); stream(loadCtx, base, count, &c) }()
	var writers sync.WaitGroup
	for i, conn := range connections {
		writers.Add(1)
		go func(i int, conn *websocket.Conn) {
			defer writers.Done()
			offset := time.Duration(i) * time.Second / time.Duration(count)
			for second := 0; second < o.seconds; second++ {
				target := measuredAt.Add(offset + time.Duration(second)*time.Second)
				if err := wait(loadCtx, time.Until(target)); err != nil {
					return
				}
				m := metrics(uint64(second + 2))
				report := shared.Report{Meta: meta(fmt.Sprintf("capacity-session-%04d", i), uint64(second+2)), Metrics: &m}
				_ = conn.SetWriteDeadline(time.Now().Add(2 * time.Second))
				if conn.WriteJSON(map[string]any{"jsonrpc": "2.0", "method": "report", "params": report}) != nil {
					c.writeErrors.Add(1)
					return
				}
				c.sent.Add(1)
			}
		}(i, conn)
	}
	// Ensure both producers and consumers have ended before any error-return cleanup.
	defer func() { cancel(); writers.Wait(); background.Wait() }()
	var latencies []float64
	historyURL := base + "/api/public/v1/nodes/capacity-0000/history?window=1h"
	for second := 0; second < o.seconds; second++ {
		if err := wait(ctx, time.Until(measuredAt.Add(time.Duration(second+1)*time.Second))); err != nil {
			return r, err
		}
		start := time.Now()
		var s snapshot
		err := getJSON(ctx, client, base+"/api/public/v1/nodes", &s)
		r.APIRequests++
		if err != nil || len(s.Nodes) != count {
			r.APIErrors++
		} else {
			latencies = append(latencies, float64(time.Since(start).Microseconds())/1000)
		}
		if second%5 == 0 {
			var h struct {
				Storage storage `json:"storage"`
			}
			if getJSON(ctx, client, historyURL, &h) != nil {
				r.APIErrors++
			} else {
				r.Storage = h.Storage
			}
		}
		cpu, rss, err := procStats(cmd.Process.Pid, ticks)
		if r.Resources {
			if err != nil {
				return r, errors.New("server process statistics disappeared")
			}
			r.CPUSeconds = cpu - baseline
			r.RSSPeak = max(r.RSSPeak, rss)
			_, own, _ := procStats(os.Getpid(), ticks)
			r.HarnessRSSPeak = max(r.HarnessRSSPeak, own)
			if rss > uint64(o.rss)<<20 {
				return r, errors.New("server exceeded RSS budget")
			}
			if r.CPUSeconds > o.cpu {
				return r, errors.New("server exceeded CPU time budget")
			}
		}
		r.DBPeak = max(r.DBPeak, dbSize(database))
		select {
		case <-p.done:
			return r, errors.New("server exited under load")
		default:
		}
	}
	writers.Wait()
	if err := wait(ctx, time.Duration(o.grace)*time.Second); err != nil {
		return r, err
	}
	var final snapshot
	if getJSON(ctx, client, base+"/api/public/v1/nodes", &final) != nil {
		return r, errors.New("final public snapshot unavailable")
	}
	for _, n := range final.Nodes {
		if n.Session == "online" && n.Freshness == "fresh" {
			r.Fresh++
		}
	}
	var history struct {
		Storage storage `json:"storage"`
	}
	if getJSON(ctx, client, historyURL, &history) != nil {
		return r, errors.New("final history unavailable")
	}
	r.Storage = history.Storage
	cancel()
	background.Wait()
	shutting.Store(true)
	for _, conn := range connections {
		conn.Close()
	}
	readers.Wait()
	r.Sent = c.sent.Load()
	r.WriteErrors = c.writeErrors.Load()
	r.ReadErrors = c.readErrors.Load()
	r.SSEEvents = c.events.Load()
	r.SSEErrors = c.sseErrors.Load()
	r.CPUAverage = 100 * r.CPUSeconds / float64(o.seconds)
	sort.Float64s(latencies)
	if len(latencies) > 0 {
		r.APIP50MS = latencies[(len(latencies)*50+99)/100-1]
		r.APIP95MS = latencies[(len(latencies)*95+99)/100-1]
		r.APIMaxMS = latencies[len(latencies)-1]
	}
	if err := p.stop(); err != nil {
		return r, err
	}
	if err := inspectDB(ctx, database, &r); err != nil {
		return r, err
	}
	r.DBFinal = dbSize(database)
	r.WallSeconds = time.Since(begin).Seconds()
	r.Passed = r.Sent == r.Expected && r.Persisted == r.Sent && r.StoredNodes == count && r.WriteErrors == 0 && r.ReadErrors == 0 && r.APIErrors == 0 && r.SSEErrors == 0 && r.SSEEvents >= uint64(o.seconds/2) && r.Fresh == count && r.Storage.State == "ready" && r.Storage.Dropped == 0 && r.Integrity == "ok"
	data, _ := json.MarshalIndent(r, "", "  ")
	if private(filepath.Join(dir, "result.json"), data) != nil {
		return r, errors.New("cannot persist result")
	}
	return r, nil
}

func stream(ctx context.Context, base string, count int, c *counters) {
	req, _ := http.NewRequestWithContext(ctx, http.MethodGet, base+"/events/public", nil)
	client := &http.Client{Transport: &http.Transport{ResponseHeaderTimeout: 3 * time.Second}}
	defer client.CloseIdleConnections()
	resp, err := client.Do(req)
	if err != nil {
		if ctx.Err() == nil {
			c.sseErrors.Add(1)
		}
		return
	}
	defer resp.Body.Close()
	if resp.StatusCode != 200 {
		c.sseErrors.Add(1)
		return
	}
	scanner := bufio.NewScanner(resp.Body)
	scanner.Buffer(make([]byte, 64<<10), maxResponse)
	for scanner.Scan() {
		line := scanner.Bytes()
		if strings.HasPrefix(string(line), "data: ") {
			var s snapshot
			if json.Unmarshal(line[6:], &s) != nil || len(s.Nodes) != count {
				c.sseErrors.Add(1)
				return
			}
			c.events.Add(1)
		}
	}
	if ctx.Err() == nil {
		c.sseErrors.Add(1)
	}
}
func clockTicks() float64 {
	if runtime.GOOS != "linux" {
		return 100
	}
	out, err := exec.Command("getconf", "CLK_TCK").Output()
	if err != nil {
		return 100
	}
	n, err := strconv.ParseFloat(strings.TrimSpace(string(out)), 64)
	if err != nil || n <= 0 {
		return 100
	}
	return n
}
func procStats(pid int, ticks float64) (float64, uint64, error) {
	raw, err := os.ReadFile(fmt.Sprintf("/proc/%d/stat", pid))
	if err != nil {
		return 0, 0, err
	}
	end := strings.LastIndexByte(string(raw), ')')
	if end < 0 {
		return 0, 0, errors.New("bad proc stat")
	}
	fields := strings.Fields(string(raw[end+1:]))
	if len(fields) < 22 {
		return 0, 0, errors.New("short proc stat")
	}
	u, e1 := strconv.ParseUint(fields[11], 10, 64)
	s, e2 := strconv.ParseUint(fields[12], 10, 64)
	rss, e3 := strconv.ParseUint(fields[21], 10, 64)
	if e1 != nil || e2 != nil || e3 != nil {
		return 0, 0, errors.New("invalid proc stat")
	}
	return float64(u+s) / ticks, rss * uint64(os.Getpagesize()), nil
}
func dbSize(path string) uint64 {
	var size uint64
	for _, suffix := range []string{"", "-wal", "-shm"} {
		if info, err := os.Stat(path + suffix); err == nil {
			size += uint64(info.Size())
		}
	}
	return size
}
func inspectDB(ctx context.Context, path string, r *result) error {
	u := url.URL{Scheme: "file", Path: path}
	q := url.Values{"mode": []string{"ro"}, "_pragma": []string{"busy_timeout(1000)"}}
	u.RawQuery = q.Encode()
	db, err := sql.Open("sqlite", u.String())
	if err != nil {
		return errors.New("cannot open final database")
	}
	defer db.Close()
	queryCtx, cancel := context.WithTimeout(ctx, 5*time.Second)
	defer cancel()
	if db.QueryRowContext(queryCtx, "PRAGMA integrity_check").Scan(&r.Integrity) != nil {
		return errors.New("database integrity check failed")
	}
	rows, err := db.QueryContext(queryCtx, "SELECT node,data FROM metrics_1m")
	if err != nil {
		return errors.New("cannot inspect persisted samples")
	}
	defer rows.Close()
	nodes := map[string]bool{}
	for rows.Next() {
		var node, raw string
		if rows.Scan(&node, &raw) != nil {
			return errors.New("invalid persisted row")
		}
		var bucket struct{ Samples uint64 }
		if json.Unmarshal([]byte(raw), &bucket) != nil {
			return errors.New("invalid persisted aggregate")
		}
		r.Persisted += bucket.Samples
		nodes[node] = true
	}
	if rows.Err() != nil {
		return errors.New("database sample query failed")
	}
	r.StoredNodes = len(nodes)
	return nil
}
