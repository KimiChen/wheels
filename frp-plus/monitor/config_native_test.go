package monitor

// This opt-in test uses the actual enhanced frpc/frps processes and management
// WebSocket. Only GitHub identity and a single lost HTTP-coordination reply are
// controlled by the fixture; no native inventory, journal or runtime is faked.
import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/fatedier/frp/extension/frpmonitor/shared"
)

type configNativeProcess struct {
	cmd    *exec.Cmd
	done   chan struct{}
	failed atomic.Bool
}

func startConfigNative(t *testing.T, ctx context.Context, binary, config, root, label string) *configNativeProcess {
	t.Helper()
	log, err := os.OpenFile(filepath.Join(root, label+".log"), os.O_CREATE|os.O_EXCL|os.O_WRONLY, 0600)
	if err != nil {
		t.Fatal("cannot create private native process log")
	}
	cmd := exec.CommandContext(ctx, binary, "-c", config)
	cmd.Dir = root
	cmd.Stdout = log
	cmd.Stderr = log
	// Avoid inheriting real deployment credentials or template variables.
	cmd.Env = []string{"PATH=" + os.Getenv("PATH"), "HOME=" + root, "TMPDIR=" + root, "LANG=C", "LC_ALL=C"}
	if err = cmd.Start(); err != nil {
		log.Close()
		t.Fatal("cannot start native E2E process")
	}
	p := &configNativeProcess{cmd: cmd, done: make(chan struct{})}
	go func() {
		if cmd.Wait() != nil {
			p.failed.Store(true)
		}
		log.Close()
		close(p.done)
	}()
	t.Cleanup(func() {
		select {
		case <-p.done:
			return
		default:
		}
		_ = cmd.Process.Signal(os.Interrupt)
		select {
		case <-p.done:
		case <-time.After(3 * time.Second):
			_ = cmd.Process.Kill()
			<-p.done
		}
	})
	return p
}
func configNativeWait(t *testing.T, ctx context.Context, label string, processes []*configNativeProcess, ready func() bool) {
	t.Helper()
	ticker := time.NewTicker(100 * time.Millisecond)
	defer ticker.Stop()
	for {
		if ready() {
			return
		}
		for _, p := range processes {
			select {
			case <-p.done:
				t.Fatal("native E2E process stopped before " + label)
			default:
			}
		}
		select {
		case <-ctx.Done():
			t.Fatal("native E2E deadline waiting for " + label)
		case <-ticker.C:
		}
	}
}
func configNativePort(t *testing.T) (net.Listener, int) {
	t.Helper()
	listener, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal("cannot reserve isolated loopback port")
	}
	return listener, listener.Addr().(*net.TCPAddr).Port
}
func configNativeOpen(port int) bool {
	c, e := net.DialTimeout("tcp", fmt.Sprintf("127.0.0.1:%d", port), 100*time.Millisecond)
	if e != nil {
		return false
	}
	c.Close()
	return true
}
func configNativeEcho(port int) bool {
	c, err := net.DialTimeout("tcp", fmt.Sprintf("127.0.0.1:%d", port), 300*time.Millisecond)
	if err != nil {
		return false
	}
	defer c.Close()
	_ = c.SetDeadline(time.Now().Add(500 * time.Millisecond))
	payload := []byte("frp-plus-admin-e2e\n")
	if _, err = c.Write(payload); err != nil {
		return false
	}
	got := make([]byte, len(payload))
	_, err = io.ReadFull(c, got)
	return err == nil && bytes.Equal(got, payload)
}
func configNativeHTTP(t *testing.T, ctx context.Context, s *Service, cookie *http.Cookie, csrf, method, path string, body any) (int, []byte) {
	t.Helper()
	var raw []byte
	var err error
	if body != nil {
		raw, err = json.Marshal(body)
		if err != nil {
			t.Fatal("cannot encode native E2E request")
		}
	}
	requestCtx, cancel := context.WithTimeout(ctx, 11*time.Second)
	defer cancel()
	req, err := http.NewRequestWithContext(requestCtx, method, "http://"+s.Address()+path, bytes.NewReader(raw))
	if err != nil {
		t.Fatal("cannot create native E2E request")
	}
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Origin", "http://"+s.Address())
	if cookie != nil {
		req.AddCookie(cookie)
	}
	if csrf != "" {
		req.Header.Set("X-CSRF-Token", csrf)
	}
	response, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal("native E2E admin request failed")
	}
	defer response.Body.Close()
	data, err := io.ReadAll(io.LimitReader(response.Body, 512*1024+1))
	if err != nil || len(data) > 512*1024 {
		t.Fatal("native E2E admin response exceeded bound")
	}
	return response.StatusCode, data
}
func configNativeDecode(t *testing.T, data []byte) configOperationResponse {
	t.Helper()
	var r configOperationResponse
	if json.Unmarshal(data, &r) != nil || r.Operation == nil {
		t.Fatal("native E2E operation response invalid")
	}
	return r
}

func TestConfigNativeAdminEndToEnd(t *testing.T) { runConfigNativeAdmin(t, "default") }

func runConfigNativeAdmin(t *testing.T, scenario string) {
	agentBinary, serverBinary := os.Getenv("FRP_CONFIG_E2E_AGENT"), os.Getenv("FRP_CONFIG_E2E_SERVER")
	if agentBinary == "" || serverBinary == "" {
		t.Skip("native Admin E2E requires FRP_CONFIG_E2E_AGENT and FRP_CONFIG_E2E_SERVER")
	}
	for _, binary := range []string{agentBinary, serverBinary} {
		info, e := os.Stat(binary)
		if e != nil || !info.Mode().IsRegular() || info.Mode().Perm()&0111 == 0 {
			t.Fatal("native E2E binary is unavailable or not executable")
		}
	}
	agentBinary, _ = filepath.Abs(agentBinary)
	serverBinary, _ = filepath.Abs(serverBinary)
	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Minute)
	defer cancel()
	root, err := filepath.EvalSymlinks(t.TempDir())
	if err != nil {
		t.Fatal("cannot resolve native E2E root")
	}
	if os.Chmod(root, 0700) != nil {
		t.Fatal("cannot secure native E2E root")
	}
	privateWrite := func(name, value string) string {
		path := filepath.Join(root, name)
		if os.WriteFile(path, []byte(value), 0600) != nil {
			t.Fatal("cannot write private native E2E file")
		}
		return path
	}
	managedRoot := filepath.Join(root, "managed")
	if os.Mkdir(managedRoot, 0700) != nil {
		t.Fatal("cannot create managed native E2E directory")
	}
	storePath := filepath.Join(managedRoot, "store.json")
	original := []byte("{\"proxies\":[],\"visitors\":[]}\n")
	if os.WriteFile(storePath, original, 0600) != nil {
		t.Fatal("cannot initialize private native Store")
	}
	s, agentToken, admin := testAdmin(t)
	cookie, session := login(t, s, admin)
	tokenPath := privateWrite("agent.token", agentToken+"\n")
	controlReservation, controlPort := configNativePort(t)
	defer controlReservation.Close()
	remoteReservation, remotePort := configNativePort(t)
	defer remoteReservation.Close()
	visitorReservation, visitorPort := configNativePort(t)
	defer visitorReservation.Close()
	local, localPort := configNativePort(t)
	var echoWG sync.WaitGroup
	echoAcceptDone := make(chan struct{})
	echoCtx, stopEcho := context.WithCancel(ctx)
	go func() { <-echoCtx.Done(); local.Close() }()
	go func() {
		defer close(echoAcceptDone)
		for {
			connection, e := local.Accept()
			if e != nil {
				return
			}
			echoWG.Add(1)
			go func() {
				defer echoWG.Done()
				defer connection.Close()
				_ = connection.SetDeadline(time.Now().Add(2 * time.Second))
				_, _ = io.Copy(connection, connection)
			}()
		}
	}()
	t.Cleanup(func() { stopEcho(); local.Close(); <-echoAcceptDone; echoWG.Wait() })
	frpToken, err := randomToken()
	if err != nil {
		t.Fatal("cannot generate disposable native credential")
	}
	common := fmt.Sprintf("auth.method = \"token\"\nauth.token = %q\nlog.to = \"console\"\nlog.level = \"warn\"\n", frpToken)
	serverConfig := privateWrite("server.toml", fmt.Sprintf("bindAddr = \"127.0.0.1\"\nproxyBindAddr = \"127.0.0.1\"\nbindPort = %d\n", controlPort)+common)
	agentConfig := privateWrite("agent.toml", fmt.Sprintf("serverAddr = \"127.0.0.1\"\nserverPort = %d\nclientID = \"1\"\nloginFailExit = false\ntransport.protocol = \"tcp\"\ntransport.wireProtocol = \"v2\"\ntransport.tls.enable = true\nstore.path = %q\n", controlPort, storePath)+common+fmt.Sprintf("\n[telemetry]\nenabled = true\nendpoint = %q\ntokenFile = %q\nserverID = \"native-admin-e2e\"\nintervalSeconds = 1\nallowInsecureLoopback = true\n\n[telemetry.configManagement]\nenabled = true\nroot = %q\n", "ws://"+s.Address()+"/agent/v1/ws", tokenPath, managedRoot))
	controlReservation.Close()
	server := startConfigNative(t, ctx, serverBinary, serverConfig, root, "server")
	configNativeWait(t, ctx, "FRP control listener", []*configNativeProcess{server}, func() bool { return configNativeOpen(controlPort) })
	client := startConfigNative(t, ctx, agentBinary, agentConfig, root, "agent")
	processes := []*configNativeProcess{server, client}
	var inventory struct {
		Code      string                  `json:"code"`
		ServiceID string                  `json:"service_id"`
		Inventory *shared.ConfigInventory `json:"inventory"`
	}
	configNativeWait(t, ctx, "authenticated native inventory", processes, func() bool {
		status, data := configNativeHTTP(t, ctx, s, cookie, "", "GET", configAdminPath, nil)
		return status == 200 && json.Unmarshal(data, &inventory) == nil && inventory.Inventory != nil && inventory.Inventory.State == "ready"
	})
	if len(inventory.Inventory.Objects) != 0 {
		t.Fatal("native E2E did not start with an empty inventory")
	}
	nativeSecret, err := randomToken()
	if err != nil {
		t.Fatal("cannot generate disposable tunnel secret")
	}
	reference, err := shared.NewConfigOperationID()
	if err != nil {
		t.Fatal("cannot generate opaque secret reference")
	}
	field := func(path string, value any) shared.ConfigFieldPatch {
		data, _ := json.Marshal(value)
		return shared.ConfigFieldPatch{Path: path, Value: data}
	}
	secret := []shared.ConfigSecretPatch{{Path: "secretKey", Mode: "reference", Reference: reference}}
	change := func(name, kind, typ string, fields []shared.ConfigFieldPatch, secrets []shared.ConfigSecretPatch) shared.ConfigChange {
		return shared.ConfigChange{Operation: "create", Kind: kind, Name: name, Type: typ, Fields: fields, Secrets: secrets}
	}
	changes := []shared.ConfigChange{
		change("e2e-tcp", "proxy", "tcp", []shared.ConfigFieldPatch{field("localIP", "127.0.0.1"), field("localPort", localPort), field("remotePort", remotePort)}, []shared.ConfigSecretPatch{}),
		change("e2e-private", "proxy", "stcp", []shared.ConfigFieldPatch{field("localIP", "127.0.0.1"), field("localPort", localPort)}, secret),
		change("e2e-visitor", "visitor", "stcp", []shared.ConfigFieldPatch{field("serverName", "e2e-private"), field("bindAddr", "127.0.0.1"), field("bindPort", visitorPort)}, secret),
	}
	key, _ := shared.NewConfigOperationID()
	input := configCreateRequest{ServiceID: inventory.ServiceID, BaseRevision: inventory.Inventory.Revision, IdempotencyKey: key, DeadlineAtMS: time.Now().Add(2 * time.Minute).UnixMilli(), Changes: changes, SecretValues: []shared.ConfigSecretInput{{Reference: reference, Value: nativeSecret}}}
	status, _ := configNativeHTTP(t, ctx, s, cookie, "invalid-csrf", "POST", configAdminPath+"/operations", input)
	if status != 403 {
		t.Fatal("native E2E mutation bypassed CSRF")
	}
	if operations, e := s.control.ListConfigOperations(ctx, "1", 100); e != nil || len(operations) != 0 {
		t.Fatal("CSRF rejection created an operation")
	}
	// The first pure validation failure exercises a real typed non-empty field
	// through HTTP decoding, the wire DTO and the native adapter.
	invalid := input
	invalid.IdempotencyKey, _ = shared.NewConfigOperationID()
	invalid.SecretValues = []shared.ConfigSecretInput{}
	invalid.Changes = []shared.ConfigChange{change("e2e-invalid", "proxy", "tcp", []shared.ConfigFieldPatch{field("localPort", "not-a-port")}, []shared.ConfigSecretPatch{})}
	status, data := configNativeHTTP(t, ctx, s, cookie, session.CSRF, "POST", configAdminPath+"/operations", invalid)
	if status != 201 {
		t.Fatal("native E2E validation request failed")
	}
	rejected := configNativeDecode(t, data)
	if rejected.Code != "invalid_field" || rejected.Operation.State != "rejected" {
		t.Fatal("native invalid field did not become a definite rejection")
	}
	remoteReservation.Close()
	visitorReservation.Close()
	status, data = configNativeHTTP(t, ctx, s, cookie, session.CSRF, "POST", configAdminPath+"/operations", input)
	if status != 201 {
		t.Fatal("native E2E prepare HTTP failed")
	}
	prepared := configNativeDecode(t, data)
	if prepared.Operation.State != "prepared" || prepared.Preview == nil || prepared.Agent == nil || len(prepared.Preview.Changes) != 3 {
		t.Fatal("native E2E prepare omitted reviewable preview")
	}
	if bytes.Contains(data, []byte(nativeSecret)) {
		t.Fatal("native secret escaped into preparation response")
	}
	before, e := os.ReadFile(storePath)
	if e != nil || !bytes.Equal(before, original) || configNativeOpen(remotePort) || configNativeOpen(visitorPort) {
		t.Fatal("prepare mutated native Store or forwarding")
	}
	// Fault injection discards exactly one actual successful apply response. It
	// does not manufacture state: recovery must query the native durable journal.
	var applied atomic.Int64
	var queried atomic.Int64
	var dropped atomic.Bool
	realCommand := s.ConfigCommand
	s.configCoordinator.mu.Lock()
	s.configCoordinator.command = func(callCtx context.Context, node string, command shared.ConfigCommand) (shared.ConfigResult, error) {
		result, err := realCommand(callCtx, node, command)
		// Local rate/backpressure refusal guarantees no command was queued.
		// Count actual successful exchanges, not those safe unsent retries.
		if command.Action == "apply" && err == nil {
			applied.Add(1)
		}
		if command.Action == "query" && err == nil {
			queried.Add(1)
		}
		if command.Action == "apply" && err == nil && result.Operation != nil && result.Operation.State == "confirmed" && dropped.CompareAndSwap(false, true) {
			return shared.ConfigResult{}, context.DeadlineExceeded
		}
		return result, err
	}
	s.configCoordinator.mu.Unlock()
	path := configAdminPath + "/operations/" + prepared.Operation.OperationID
	action := configActionRequest{ExpectedVersion: prepared.Operation.Version, ContextRevision: prepared.Agent.ContextRevision, CandidateDigest: prepared.Operation.CandidateDigest}
	status, data = configNativeHTTP(t, ctx, s, cookie, session.CSRF, "POST", path+"/apply", action)
	if status != 200 {
		t.Fatal("native E2E apply HTTP failed")
	}
	uncertain := configNativeDecode(t, data)
	if !dropped.Load() || uncertain.Operation.State != "outcome_unknown" {
		t.Fatal("lost native application response did not retain unknown outcome")
	}
	var confirmed configOperationResponse
	configNativeWait(t, ctx, "native journal query confirmation", processes, func() bool {
		status, data := configNativeHTTP(t, ctx, s, cookie, "", "GET", path, nil)
		if status != 200 {
			return false
		}
		confirmed = configNativeDecode(t, data)
		if confirmed.Operation.State != "confirmed" {
			time.Sleep(900 * time.Millisecond)
			return false
		}
		return true
	})
	if applied.Load() != 1 {
		t.Fatal("native recovery repeated a successfully delivered apply")
	}
	if queried.Load() < 1 {
		t.Fatal("native recovery did not query the durable journal")
	}
	if confirmed.Agent == nil || !confirmed.Agent.StorePersisted || !confirmed.Agent.RuntimeApplied || !confirmed.Agent.RuntimeLoaded || !confirmed.Agent.ResourcesReady || confirmed.Agent.BusinessChecked {
		t.Fatal("native recovery misreported runtime or business facts")
	}
	configNativeWait(t, ctx, "TCP business forwarding", processes, func() bool { return configNativeEcho(remotePort) })
	configNativeWait(t, ctx, "STCP Visitor business forwarding", processes, func() bool { return configNativeEcho(visitorPort) })
	if scenario == "restore" {
		runConfigNativeRestore(t, ctx, s, cookie, session.CSRF, root, agentBinary, agentConfig, server, client, inventory.ServiceID, remotePort, visitorPort)
		return
	}
	if scenario == "rollback_failure" {
		runConfigNativeRollbackFailure(t, ctx, s, cookie, session.CSRF, root, agentBinary, agentConfig, server, client, storePath, remotePort, visitorPort)
		return
	}
	// Successful business checks above are external evidence; the native Agent
	// still correctly leaves business_checked=false.
	action = configActionRequest{ExpectedVersion: confirmed.Operation.Version, ContextRevision: confirmed.Agent.ContextRevision, CandidateDigest: confirmed.Operation.CandidateDigest}
	status, data = configNativeHTTP(t, ctx, s, cookie, session.CSRF, "POST", path+"/rollback", action)
	if status != 200 {
		t.Fatal("native E2E rollback HTTP failed")
	}
	rolled := configNativeDecode(t, data)
	if rolled.Operation.State != "rolled_back" || rolled.Agent == nil || rolled.Agent.StorePersisted || rolled.Agent.RuntimeApplied || !rolled.Agent.RuntimeLoaded || !rolled.Agent.ResourcesReady {
		t.Fatal("native rollback did not preserve confirmed recovery facts")
	}
	after, e := os.ReadFile(storePath)
	if e != nil || !bytes.Equal(after, original) {
		t.Fatal("native rollback did not restore exact Store bytes")
	}
	configNativeWait(t, ctx, "removed forwarding resources", processes, func() bool { return !configNativeOpen(remotePort) && !configNativeOpen(visitorPort) })
	for _, audit := range rolled.Events {
		if audit.Actor == "" {
			t.Fatal("native E2E audit lacks authenticated actor")
		}
	}
	for _, filename := range []string{s.cfg.DatabaseFile, s.cfg.DatabaseFile + "-wal"} {
		data, e := os.ReadFile(filename)
		if e != nil && !errors.Is(e, os.ErrNotExist) {
			t.Fatal("cannot inspect private control database")
		}
		if bytes.Contains(data, []byte(nativeSecret)) {
			t.Fatal("native secret persisted in control database")
		}
	}
	if strings.Contains(configBody(t, rolled), nativeSecret) {
		t.Fatal("native secret escaped into operation audit")
	}
	t.Log("native Admin E2E passed: authenticated HTTP/WS, strict field validation, secret reference, preview, lost-reply query recovery, TCP/STCP Visitor, exact rollback")
}
