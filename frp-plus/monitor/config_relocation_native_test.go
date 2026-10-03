package monitor

import (
	"bytes"
	"context"
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/tls"
	"crypto/x509"
	"crypto/x509/pkix"
	"encoding/json"
	"encoding/pem"
	"fmt"
	"io"
	"math/big"
	"net"
	"net/http"
	"net/http/httptest"
	"os"
	"os/exec"
	"path/filepath"
	"sort"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/fatedier/frp/extension/frpmonitor/agent/configuration"
	"github.com/fatedier/frp/extension/frpmonitor/agent/managed"
	"github.com/fatedier/frp/extension/frpmonitor/shared"
)

// Opt-in: this fixture uses only isolated loopback listeners, disposable TLS
// credentials, a test GitHub identity provider and private t.TempDir files.
func TestBackupRelocationNativeEndToEnd(t *testing.T) {
	agent, server := os.Getenv("FRP_BACKUP_RELOCATION_E2E_AGENT"), os.Getenv("FRP_BACKUP_RELOCATION_E2E_SERVER")
	if agent == "" || server == "" {
		t.Skip("requires FRP_BACKUP_RELOCATION_E2E_AGENT and FRP_BACKUP_RELOCATION_E2E_SERVER")
	}
	for _, wire := range []string{"v1", "v2"} {
		t.Run(wire, func(t *testing.T) { runBackupRelocationNative(t, agent, server, wire) })
	}
}
func relocationPrivate(t *testing.T, path string, data []byte) {
	t.Helper()
	if os.MkdirAll(filepath.Dir(path), 0700) != nil || os.WriteFile(path, data, 0600) != nil {
		t.Fatal("cannot write private relocation fixture")
	}
}
func relocationJSON(t *testing.T, path string, value any) {
	t.Helper()
	data, err := json.Marshal(value)
	if err != nil {
		t.Fatal("cannot encode relocation fixture")
	}
	relocationPrivate(t, path, data)
}
func relocationNativeEnv(root string, env map[string]string) []string {
	out := []string{"PATH=" + os.Getenv("PATH"), "HOME=" + root, "TMPDIR=" + root, "LANG=C", "LC_ALL=C"}
	names := []string{}
	for name := range env {
		names = append(names, name)
	}
	sort.Strings(names)
	for _, name := range names {
		out = append(out, name+"="+env[name])
	}
	return out
}
func relocationProcess(t *testing.T, ctx context.Context, binary, config, cwd, logPath string, env map[string]string) *configNativeProcess {
	t.Helper()
	log, err := os.OpenFile(logPath, os.O_CREATE|os.O_EXCL|os.O_WRONLY, 0600)
	if err != nil {
		t.Fatal("cannot open private relocation log")
	}
	cmd := exec.CommandContext(ctx, binary, "-c", config)
	cmd.Dir = cwd
	cmd.Env = relocationNativeEnv(cwd, env)
	cmd.Stdout = log
	cmd.Stderr = log
	if cmd.Start() != nil {
		log.Close()
		t.Fatal("cannot start relocation native process")
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
		_ = p.cmd.Process.Signal(os.Interrupt)
		select {
		case <-p.done:
		case <-time.After(3 * time.Second):
			_ = p.cmd.Process.Kill()
			<-p.done
		}
	})
	return p
}
func relocationMaintenance(t *testing.T, ctx context.Context, binary, cwd, logPath string, result any, args ...string) {
	t.Helper()
	cmd := exec.CommandContext(ctx, binary, append([]string{"managed-maintenance", "--offline"}, args...)...)
	cmd.Dir = cwd
	cmd.Env = relocationNativeEnv(cwd, nil)
	var output bytes.Buffer
	cmd.Stdout = &output
	log, err := os.OpenFile(logPath, os.O_CREATE|os.O_EXCL|os.O_WRONLY, 0600)
	if err != nil {
		t.Fatal("cannot open private helper log")
	}
	defer log.Close()
	cmd.Stderr = log
	if cmd.Run() != nil {
		reason := "maintenance_unavailable"
		_ = log.Sync()
		data, _ := os.ReadFile(logPath)
		for _, code := range []string{"maintenance_locked", "maintenance_unsafe_path", "maintenance_invalid_request", "maintenance_conflict", "maintenance_recovery_required", "maintenance_unsupported_dependency"} {
			if bytes.Contains(data, []byte(code)) {
				reason = code
				break
			}
		}
		t.Fatalf("native relocation maintenance failed at %s (%s)", args[0], reason)
	}
	if output.Len() > 1<<20 || json.Unmarshal(output.Bytes(), result) != nil {
		t.Fatal("native relocation helper output invalid")
	}
}
func relocationTLS(t *testing.T) ([]byte, []byte, []byte) {
	t.Helper()
	key, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		t.Fatal("TLS fixture key")
	}
	ca := &x509.Certificate{SerialNumber: big.NewInt(1), Subject: pkix.Name{CommonName: "frp-plus-test-ca"}, NotBefore: time.Now().Add(-time.Hour), NotAfter: time.Now().Add(24 * time.Hour), IsCA: true, BasicConstraintsValid: true, KeyUsage: x509.KeyUsageCertSign | x509.KeyUsageDigitalSignature}
	caDER, err := x509.CreateCertificate(rand.Reader, ca, ca, &key.PublicKey, key)
	if err != nil {
		t.Fatal("TLS fixture CA")
	}
	leafKey, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		t.Fatal("TLS fixture leaf key")
	}
	leaf := &x509.Certificate{SerialNumber: big.NewInt(2), Subject: pkix.Name{CommonName: "localhost"}, DNSNames: []string{"localhost"}, IPAddresses: []net.IP{net.ParseIP("127.0.0.1")}, NotBefore: ca.NotBefore, NotAfter: ca.NotAfter, KeyUsage: x509.KeyUsageDigitalSignature, ExtKeyUsage: []x509.ExtKeyUsage{x509.ExtKeyUsageServerAuth, x509.ExtKeyUsageClientAuth}}
	cert, err := x509.CreateCertificate(rand.Reader, leaf, ca, &leafKey.PublicKey, key)
	if err != nil {
		t.Fatal("TLS fixture leaf")
	}
	private, err := x509.MarshalPKCS8PrivateKey(leafKey)
	if err != nil {
		t.Fatal("TLS fixture private encoding")
	}
	return pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: caDER}), pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: cert}), pem.EncodeToMemory(&pem.Block{Type: "PRIVATE KEY", Bytes: private})
}
func relocationWaitQuery(t *testing.T, ctx context.Context, s *Service, processes []*configNativeProcess, command shared.ConfigCommand) shared.ConfigResult {
	t.Helper()
	var result shared.ConfigResult
	configNativeWait(t, ctx, "relocated journal facts", processes, func() bool {
		r, err := s.ConfigCommand(ctx, "1", command)
		if err != nil {
			return false
		}
		result = r
		return r.Operation != nil
	})
	return result
}
func runBackupRelocationNative(t *testing.T, agentBinary, serverBinary, wire string) {
	ctx, cancel := context.WithTimeout(context.Background(), 4*time.Minute)
	defer cancel()
	root, err := filepath.EvalSymlinks(t.TempDir())
	if err != nil || os.Chmod(root, 0700) != nil {
		t.Fatal("cannot secure relocation fixture")
	}
	agentBinary, _ = filepath.Abs(agentBinary)
	serverBinary, _ = filepath.Abs(serverBinary)
	source, target := filepath.Join(root, "source"), filepath.Join(root, "target")
	externalSource, externalTarget := filepath.Join(root, "external-source"), filepath.Join(root, "external-target")
	for _, dir := range []string{source, target, externalSource, externalTarget, filepath.Join(source, "managed"), filepath.Join(target, "managed"), filepath.Join(root, "server"), filepath.Join(root, "logs")} {
		if os.MkdirAll(dir, 0700) != nil {
			t.Fatal("cannot create private fixture directory")
		}
	}
	s, agentToken, admin := testAdmin(t)
	cookie, session := login(t, s, admin)
	var rollbackDispatches atomic.Int64
	realCommand := s.ConfigCommand
	s.configCoordinator.mu.Lock()
	s.configCoordinator.command = func(callCtx context.Context, node string, command shared.ConfigCommand) (shared.ConfigResult, error) {
		if command.Action == "rollback" {
			rollbackDispatches.Add(1)
		}
		return realCommand(callCtx, node, command)
	}
	s.configCoordinator.mu.Unlock()
	controlReservation, controlPort := configNativePort(t)
	defer controlReservation.Close()
	fileReservation, filePort := configNativePort(t)
	defer fileReservation.Close()
	pluginReservation, pluginPort := configNativePort(t)
	defer pluginReservation.Close()
	storedReservation, storedPort := configNativePort(t)
	defer storedReservation.Close()
	echo, echoPort := configNativePort(t)
	go func() {
		for {
			conn, e := echo.Accept()
			if e != nil {
				return
			}
			go func() {
				defer conn.Close()
				_ = conn.SetDeadline(time.Now().Add(time.Second))
				_, _ = io.Copy(conn, conn)
			}()
		}
	}()
	t.Cleanup(func() { echo.Close() })
	origin := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_, _ = io.WriteString(w, "frp-plus-relocation-business\n")
	}))
	defer origin.Close()
	ca, cert, key := relocationTLS(t)
	caSource, caTarget := filepath.Join(externalSource, "ca.pem"), filepath.Join(externalTarget, "ca.pem")
	relocationPrivate(t, caSource, ca)
	for _, dir := range []string{filepath.Join(root, "server"), filepath.Join(source, "tls")} {
		relocationPrivate(t, filepath.Join(dir, "leaf.crt"), cert)
		relocationPrivate(t, filepath.Join(dir, "leaf.key"), key)
	}
	relocationPrivate(t, filepath.Join(root, "server/ca.pem"), ca)
	token, err := randomToken()
	if err != nil {
		t.Fatal("disposable FRP token unavailable")
	}
	relocationPrivate(t, filepath.Join(root, "server/frp.token"), []byte(token))
	relocationPrivate(t, filepath.Join(source, "agent.token"), []byte(agentToken))
	relocationPrivate(t, filepath.Join(source, "managed/store.json"), []byte("{\"proxies\":[],\"visitors\":[]}\n"))
	relocationPrivate(t, filepath.Join(source, "installation.json"), []byte(`{"format":2,"roles":["agent"],"managed":{"version":1,"root":"managed","store":"store.json"}}`))
	serverConfig := filepath.Join(root, "server/server.toml")
	relocationPrivate(t, serverConfig, []byte(fmt.Sprintf("bindAddr='127.0.0.1'\nproxyBindAddr='127.0.0.1'\nbindPort=%d\nauth.method='token'\nauth.tokenSource.type='file'\nauth.tokenSource.file.path=%q\ntransport.tls.force=true\ntransport.tls.certFile=%q\ntransport.tls.keyFile=%q\ntransport.tls.trustedCaFile=%q\nlog.to='console'\nlog.level='info'\n", controlPort, filepath.Join(root, "server/frp.token"), filepath.Join(root, "server/leaf.crt"), filepath.Join(root, "server/leaf.key"), filepath.Join(root, "server/ca.pem"))))
	configRaw := []byte(fmt.Sprintf("serverAddr='127.0.0.1'\nserverPort=%d\nclientID='1'\nloginFailExit=false\nincludes=['{{ .Envs.ROOT }}/includes/*.toml']\nstore.path='{{ .Envs.ROOT }}/managed/store.json'\nauth.method='token'\nauth.token='{{ index .Envs \"FRP_TOKEN\" }}'\ntransport.protocol='tcp'\ntransport.wireProtocol=%q\ntransport.tls.enable=true\ntransport.tls.serverName='localhost'\ntransport.tls.certFile='{{ .Envs.ROOT }}/tls/leaf.crt'\ntransport.tls.keyFile='{{ .Envs.ROOT }}/tls/leaf.key'\ntransport.tls.trustedCaFile='{{ .Envs.CA }}'\nlog.to='console'\nlog.level='info'\n[telemetry]\nenabled=true\nendpoint=%q\ntokenFile='{{ .Envs.ROOT }}/agent.token'\nserverID='relocation-native-test'\nintervalSeconds=1\nallowInsecureLoopback=true\n[telemetry.configManagement]\nenabled=true\nroot='{{ .Envs.ROOT }}/managed'\n", controlPort, wire, "ws://"+s.Address()+"/agent/v1/ws"))
	includeRaw := []byte(fmt.Sprintf("[[proxies]]\nname='file-tcp'\ntype='tcp'\nlocalIP='127.0.0.1'\nlocalPort=%d\nremotePort=%d\n[[proxies]]\nname='file-plugin'\ntype='tcp'\nremotePort=%d\n[proxies.plugin]\ntype='https2http'\nlocalAddr=%q\ncrtPath='{{ .Envs.ROOT }}/tls/leaf.crt'\nkeyPath='{{ .Envs.ROOT }}/tls/leaf.key'\n", echoPort, filePort, pluginPort, strings.TrimPrefix(origin.URL, "http://")))
	relocationPrivate(t, filepath.Join(source, "agent.toml"), configRaw)
	relocationPrivate(t, filepath.Join(source, "includes/proxies.toml"), includeRaw)
	sourceEnv := map[string]string{"ROOT": source, "CA": caSource, "FRP_TOKEN": token}
	targetEnv := map[string]string{"ROOT": target, "CA": caTarget, "FRP_TOKEN": token}
	policyFor := func(dir, caFile string) configuration.BackupPolicy {
		return configuration.BackupPolicy{Version: 1, ConfigFile: filepath.Join(dir, "agent.toml"), WorkingDir: dir, StoreFile: filepath.Join(dir, "managed/store.json"), Roots: []configuration.BackupPolicyPath{{ID: "installation", Path: dir}}, Files: []configuration.BackupPolicyPath{{ID: "ca", Path: caFile}}}
	}
	sourcePolicyFile, targetPolicyFile := filepath.Join(root, "source-policy.json"), filepath.Join(root, "target-policy.json")
	sourceEnvFile, targetEnvFile := filepath.Join(root, "source-env.json"), filepath.Join(root, "target-env.json")
	relocationJSON(t, sourcePolicyFile, policyFor(source, caSource))
	relocationJSON(t, targetPolicyFile, policyFor(target, caTarget))
	relocationJSON(t, sourceEnvFile, sourceEnv)
	relocationJSON(t, targetEnvFile, targetEnv)
	roots := x509.NewCertPool()
	roots.AppendCertsFromPEM(ca)
	transport := &http.Transport{TLSClientConfig: &tls.Config{RootCAs: roots, ServerName: "localhost", MinVersion: tls.VersionTLS12}}
	httpClient := &http.Client{Transport: transport, Timeout: time.Second}
	defer transport.CloseIdleConnections()
	lastPluginDiagnostic := ""
	pluginWorks := func() bool {
		request, _ := http.NewRequestWithContext(ctx, "GET", fmt.Sprintf("https://127.0.0.1:%d/", pluginPort), nil)
		request.Host = "localhost"
		response, err := httpClient.Do(request)
		if err != nil {
			if lastPluginDiagnostic != "connection_error" {
				t.Log("plugin probe", "connection_error")
				lastPluginDiagnostic = "connection_error"
			}
			return false
		}
		defer response.Body.Close()
		body, err := io.ReadAll(io.LimitReader(response.Body, 1024))
		diagnostic := fmt.Sprintf("status=%d bytes=%d", response.StatusCode, len(body))
		if diagnostic != lastPluginDiagnostic {
			t.Log("plugin probe", diagnostic)
			lastPluginDiagnostic = diagnostic
		}
		return err == nil && response.StatusCode == 200 && string(body) == "frp-plus-relocation-business\n"
	}
	controlReservation.Close()
	fileReservation.Close()
	pluginReservation.Close()
	server := relocationProcess(t, ctx, serverBinary, serverConfig, filepath.Join(root, "server"), filepath.Join(root, "logs/server.log"), nil)
	configNativeWait(t, ctx, "mutual TLS server", []*configNativeProcess{server}, func() bool { return configNativeOpen(controlPort) })
	client := relocationProcess(t, ctx, agentBinary, filepath.Join(source, "agent.toml"), source, filepath.Join(root, "logs/source.log"), sourceEnv)
	processes := []*configNativeProcess{server, client}
	var inventory struct {
		Code      string                  `json:"code"`
		ServiceID string                  `json:"service_id"`
		Inventory *shared.ConfigInventory `json:"inventory"`
	}
	waitInventory := func(want string) {
		lastDiagnostic := ""
		configNativeWait(t, ctx, "native managed inventory", processes, func() bool {
			status, data := configNativeHTTP(t, ctx, s, cookie, "", "GET", configAdminPath, nil)
			decoded := json.Unmarshal(data, &inventory) == nil
			if decoded {
				state := "none"
				if inventory.Inventory != nil {
					state = inventory.Inventory.State
				}
				diagnostic := fmt.Sprintf("%d/%s/%s", status, inventory.Code, state)
				if diagnostic != lastDiagnostic && (inventory.Code == "ok" || shared.ValidConfigResultCode(inventory.Code)) {
					t.Log("native inventory state", diagnostic)
					lastDiagnostic = diagnostic
				}
			}
			return status == 200 && decoded && inventory.Inventory != nil && inventory.Inventory.State == "ready" && (want == "" || inventory.ServiceID == want)
		})
	}
	waitInventory("")
	t.Log("source inventory ready")
	configNativeWait(t, ctx, "source TLS/token/include TCP forwarding", processes, func() bool { return configNativeEcho(filePort) })
	t.Log("source include TCP verified")
	configNativeWait(t, ctx, "source HTTPS plugin forwarding", processes, pluginWorks)
	field := func(path string, value any) shared.ConfigFieldPatch {
		data, _ := json.Marshal(value)
		return shared.ConfigFieldPatch{Path: path, Value: data}
	}
	idem, _ := shared.NewConfigOperationID()
	input := configCreateRequest{ServiceID: inventory.ServiceID, BaseRevision: inventory.Inventory.Revision, IdempotencyKey: idem, DeadlineAtMS: time.Now().Add(2 * time.Minute).UnixMilli(), Changes: []shared.ConfigChange{{Operation: "create", Kind: "proxy", Name: "stored-tcp", Type: "tcp", Fields: []shared.ConfigFieldPatch{field("localIP", "127.0.0.1"), field("localPort", echoPort), field("remotePort", storedPort)}, Secrets: []shared.ConfigSecretPatch{}}}, SecretValues: []shared.ConfigSecretInput{}}
	storedReservation.Close()
	status, data := configNativeHTTP(t, ctx, s, cookie, session.CSRF, "POST", configAdminPath+"/operations", input)
	if status != 201 {
		t.Fatal("native relocation prepare failed")
	}
	prepared := configNativeDecode(t, data)
	if prepared.Operation.State != "prepared" || prepared.Agent == nil {
		t.Fatal("native relocation did not prepare")
	}
	opPath := configAdminPath + "/operations/" + prepared.Operation.OperationID
	action := configActionRequest{ExpectedVersion: prepared.Operation.Version, ContextRevision: prepared.Agent.ContextRevision, CandidateDigest: prepared.Operation.CandidateDigest}
	status, data = configNativeHTTP(t, ctx, s, cookie, session.CSRF, "POST", opPath+"/apply", action)
	if status != 200 {
		t.Fatal("native relocation apply failed")
	}
	var original configOperationResponse
	configNativeWait(t, ctx, "completed source operation", processes, func() bool {
		status, data := configNativeHTTP(t, ctx, s, cookie, "", "GET", opPath, nil)
		if status != 200 {
			return false
		}
		original = configNativeDecode(t, data)
		return original.Operation.State == "confirmed" && original.Agent != nil
	})
	configNativeWait(t, ctx, "stored TCP forwarding", processes, func() bool { return configNativeEcho(storedPort) })
	t.Log("source completed operation and business verified")
	oldService := inventory.ServiceID
	for cycle := 1; cycle <= 2; cycle++ {
		t.Log("restore cycle", cycle)
		activeDir, activePolicy, activeEnvFile := source, sourcePolicyFile, sourceEnvFile
		if cycle == 2 {
			activeDir, activePolicy, activeEnvFile = target, targetPolicyFile, targetEnvFile
		}
		stopRestoreNative(t, client, false)
		checkpoint := filepath.Join(root, fmt.Sprintf("checkpoint-%d", cycle))
		var exported managed.CheckpointSummary
		relocationMaintenance(t, ctx, agentBinary, root, filepath.Join(root, fmt.Sprintf("logs/checkpoint-%d.log", cycle)), &exported, "checkpoint", "-c", filepath.Join(activeDir, "agent.toml"), "--policy", activePolicy, "--env-file", activeEnvFile, "--output", checkpoint)
		if exported.Code != "ok" {
			t.Fatal("native v3 checkpoint failed")
		}
		raw, err := os.ReadFile(filepath.Join(checkpoint, "CHECKPOINT.json"))
		if err != nil {
			t.Fatal("checkpoint envelope absent")
		}
		var envelope managed.CheckpointManifestV2
		if json.Unmarshal(raw, &envelope) != nil || envelope.Version != 3 {
			t.Fatal("native checkpoint did not use v3")
		}
		if err := filepath.Walk(checkpoint, func(path string, info os.FileInfo, walkErr error) error {
			if walkErr != nil {
				return walkErr
			}
			if info.IsDir() {
				return nil
			}
			if !info.Mode().IsRegular() {
				return fmt.Errorf("unexpected checkpoint file type")
			}
			payload, readErr := os.ReadFile(path)
			if readErr != nil {
				return readErr
			}
			if bytes.Contains(payload, []byte(token)) {
				return fmt.Errorf("template value archived")
			}
			return nil
		}); err != nil {
			t.Fatal("checkpoint archived an environment secret or invalid payload")
		}
		if cycle == 1 {
			if os.RemoveAll(source) != nil || os.RemoveAll(externalSource) != nil {
				t.Fatal("cannot remove source fixture")
			}
		} else {
			for _, path := range []string{filepath.Join(target, "agent.toml"), filepath.Join(target, "agent.token"), filepath.Join(target, "includes"), filepath.Join(target, "tls"), caTarget} {
				if os.RemoveAll(path) != nil {
					t.Fatal("cannot remove same-path context fixture")
				}
			}
		}
		var installed managed.RestoreSummary
		relocationMaintenance(t, ctx, agentBinary, root, filepath.Join(root, fmt.Sprintf("logs/install-%d.log", cycle)), &installed, "restore-install", "--root", filepath.Join(target, "managed"), "--checkpoint", checkpoint, "--manifest-digest", exported.ManifestDigest, "--policy", targetPolicyFile, "--source-policy", activePolicy, "--env-file", targetEnvFile, "--source-env-file", activeEnvFile)
		if installed.Code != "ok" || installed.State != "pending" || installed.ServiceID == oldService {
			t.Fatal("v3 restore identity/gate invalid")
		}
		for name, want := range map[string][]byte{"agent.toml": configRaw, "includes/proxies.toml": includeRaw} {
			got, err := os.ReadFile(filepath.Join(target, name))
			if err != nil || !bytes.Equal(got, want) {
				t.Fatal("restored source template bytes changed")
			}
		}
		for _, file := range []string{filepath.Join(target, "agent.toml"), filepath.Join(target, "includes/proxies.toml")} {
			got, _ := os.ReadFile(file)
			if bytes.Contains(got, []byte(token)) {
				t.Fatal("template secret materialized during restore")
			}
		}
		client = relocationProcess(t, ctx, agentBinary, filepath.Join(target, "agent.toml"), target, filepath.Join(root, fmt.Sprintf("logs/restored-%d.log", cycle)), targetEnv)
		processes = []*configNativeProcess{server, client}
		var restore configRestoreResponse
		configNativeWait(t, ctx, "v3 restored verification", processes, func() bool {
			status, data := configNativeHTTP(t, ctx, s, cookie, "", "GET", configAdminPath+"/restore", nil)
			return status == 200 && json.Unmarshal(data, &restore) == nil && restore.Restore != nil && restore.Restore.State == "verified"
		})
		if restore.ActiveOperation != nil || restore.Restore.BackupServiceID != oldService {
			t.Fatal("relocated completed journal became active or lineage changed")
		}
		configNativeWait(t, ctx, "restored TLS/token/includes/plugin forwarding", processes, func() bool { return configNativeEcho(filePort) && configNativeEcho(storedPort) && pluginWorks() })
		ack := configRestoreRequest{ServiceID: restore.ServiceID, Epoch: restore.Restore.Epoch, ManifestDigest: restore.Restore.ManifestDigest, ContextRevision: restore.Restore.ContextRevision, StoreDigest: restore.Restore.StoreDigest}
		configNativeWait(t, ctx, "authenticated relocation takeover", processes, func() bool {
			status, data := configNativeHTTP(t, ctx, s, cookie, session.CSRF, "POST", configAdminPath+"/restore/acknowledge", ack)
			return status == 200 && json.Unmarshal(data, &restore) == nil && restore.Receipt != nil && restore.Receipt.State == "acknowledged"
		})
		stopRestoreNative(t, client, false)
		var confirmed managed.RestoreSummary
		relocationMaintenance(t, ctx, agentBinary, root, filepath.Join(root, fmt.Sprintf("logs/confirm-%d.log", cycle)), &confirmed, "restore-confirm", "-c", filepath.Join(target, "agent.toml"), "--epoch", installed.Epoch, "--manifest-digest", exported.ManifestDigest, "--policy", targetPolicyFile, "--env-file", targetEnvFile)
		if confirmed.State != "confirmed" {
			t.Fatal("v3 offline confirmation failed")
		}
		client = relocationProcess(t, ctx, agentBinary, filepath.Join(target, "agent.toml"), target, filepath.Join(root, fmt.Sprintf("logs/confirmed-%d.log", cycle)), targetEnv)
		processes = []*configNativeProcess{server, client}
		waitInventory(installed.ServiceID)
		configNativeWait(t, ctx, "completed restore receipt", processes, func() bool {
			status, data := configNativeHTTP(t, ctx, s, cookie, "", "GET", configAdminPath+"/restore", nil)
			return status == 200 && json.Unmarshal(data, &restore) == nil && restore.Restore != nil && restore.Restore.State == "confirmed"
		})
		var historical configOperationResponse
		lastHistory := ""
		configNativeWait(t, ctx, "historical material projection through receipt lineage", processes, func() bool {
			status, data := configNativeHTTP(t, ctx, s, cookie, "", "GET", opPath, nil)
			if status != 200 {
				return false
			}
			historical = configNativeDecode(t, data)
			materials := "none"
			if historical.Agent != nil {
				materials = historical.Agent.MaterialsState
			}
			diagnostic := fmt.Sprintf("%d/%s/%s", status, historical.Code, materials)
			if diagnostic != lastHistory {
				t.Log("historical projection", diagnostic)
				lastHistory = diagnostic
			}
			return historical.Agent != nil && historical.Agent.MaterialsState == "context_changed"
		})
		if historical.Operation.ServiceID != original.Operation.ServiceID || historical.Operation.State != "confirmed" {
			t.Fatal("historical projection rewrote operation identity or terminal facts")
		}
		priorDispatches := rollbackDispatches.Load()
		historicalAction := configActionRequest{ExpectedVersion: historical.Operation.Version, ContextRevision: historical.Agent.ContextRevision, CandidateDigest: historical.Operation.CandidateDigest}
		configNativeWait(t, ctx, "local historical rollback rejection", processes, func() bool {
			status, data := configNativeHTTP(t, ctx, s, cookie, session.CSRF, "POST", opPath+"/rollback", historicalAction)
			var response struct {
				Code string `json:"code"`
			}
			return status == 200 && json.Unmarshal(data, &response) == nil && response.Code == "context_changed"
		})
		if rollbackDispatches.Load() != priorDispatches {
			t.Fatal("historical rollback dispatched a mutation")
		}
		query := shared.ConfigCommand{Action: "query", ServiceID: installed.ServiceID, OperationID: original.Operation.OperationID, BaseRevision: original.Operation.BaseRevision, CandidateDigest: original.Operation.CandidateDigest}
		result := relocationWaitQuery(t, ctx, s, processes, query)
		if result.Code != "ok" || result.Operation.State != "confirmed" || result.Operation.MaterialsState != "context_changed" {
			t.Fatal("historical journal lost context-changed proof")
		}
		before, _ := os.ReadFile(filepath.Join(target, "managed/store.json"))
		query.Action = "rollback"
		query.ContextRevision = original.Agent.ContextRevision
		rejected := relocationWaitQuery(t, ctx, s, processes, query)
		if rejected.Code != "context_changed" || rejected.Operation.State != "confirmed" {
			t.Fatal("native historical rollback was not rejected")
		}
		after, _ := os.ReadFile(filepath.Join(target, "managed/store.json"))
		if !bytes.Equal(before, after) {
			t.Fatal("historical rollback changed Store")
		}
		configNativeWait(t, ctx, "confirmed relocated forwarding", processes, func() bool { return configNativeEcho(filePort) && configNativeEcho(storedPort) && pluginWorks() })
		oldService = installed.ServiceID
	}
	t.Log("v3 relocation passed: TLS/token auth, preserved templates/includes, real HTTPS plugin and TCP, deleted source, authenticated two-step restore, second same-path recovery and historical rollback gate")
}
