package monitor

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net"
	"net/http"
	"os"
	"path/filepath"
	"testing"
	"time"
)

func TestConfigNativeUnmanagedAPIsEndToEnd(t *testing.T) {
	agent, server := os.Getenv("FRP_CONFIG_E2E_AGENT"), os.Getenv("FRP_CONFIG_E2E_SERVER")
	if agent == "" || server == "" {
		t.Skip("native unmanaged API E2E requires actual Agent and Server binaries")
	}
	ctx, cancel := context.WithTimeout(context.Background(), time.Minute)
	defer cancel()
	root, err := filepath.EvalSymlinks(t.TempDir())
	if err != nil || os.Chmod(root, 0700) != nil {
		t.Fatal("cannot secure native unmanaged fixture")
	}
	write := func(name, content string) string {
		path := filepath.Join(root, name)
		if os.WriteFile(path, []byte(content), 0600) != nil {
			t.Fatal("cannot write private native unmanaged configuration")
		}
		return path
	}
	ports := make([]int, 5)
	reservations := make([]net.Listener, len(ports))
	for i := range ports {
		reservation, port := configNativePort(t)
		defer reservation.Close()
		ports[i] = port
		reservations[i] = reservation
	}
	for _, reservation := range reservations {
		reservation.Close()
	}
	local, localPort := configNativePort(t)
	done := make(chan struct{})
	go func() {
		defer close(done)
		for {
			connection, err := local.Accept()
			if err != nil {
				return
			}
			_ = connection.SetDeadline(time.Now().Add(time.Second))
			_, _ = io.Copy(connection, connection)
			connection.Close()
		}
	}()
	t.Cleanup(func() { local.Close(); <-done })
	password, _ := randomToken()
	store := write("store.json", "{\"proxies\":[],\"visitors\":[]}")
	serverConfig := write("server.toml", fmt.Sprintf("bindAddr = \"127.0.0.1\"\nproxyBindAddr = \"127.0.0.1\"\nbindPort = %d\nlog.level = \"warn\"\n", ports[0]))
	base := fmt.Sprintf("serverAddr = \"127.0.0.1\"\nserverPort = %d\nstore.path = %q\nlog.level = \"warn\"\nwebServer.addr = \"127.0.0.1\"\nwebServer.port = %d\nwebServer.user = \"native-test\"\nwebServer.password = %q\n", ports[0], store, ports[1], password)
	fileConfig := func(port int) string {
		return base + fmt.Sprintf("[[proxies]]\nname = \"native-file\"\ntype = \"tcp\"\nlocalIP = \"127.0.0.1\"\nlocalPort = %d\nremotePort = %d\n", localPort, port)
	}
	config := write("agent.toml", fileConfig(ports[2]))
	frps := startConfigNative(t, ctx, server, serverConfig, root, "native-server")
	configNativeWait(t, ctx, "native server", []*configNativeProcess{frps}, func() bool { return configNativeOpen(ports[0]) })
	frpc := startConfigNative(t, ctx, agent, config, root, "native-agent")
	processes := []*configNativeProcess{frps, frpc}
	configNativeWait(t, ctx, "native file forwarding and Dashboard", processes, func() bool { return configNativeEcho(ports[2]) && configNativeOpen(ports[1]) })
	call := func(method, path string, body []byte, authenticated bool) (int, []byte) {
		t.Helper()
		req, err := http.NewRequestWithContext(ctx, method, fmt.Sprintf("http://127.0.0.1:%d%s", ports[1], path), bytes.NewReader(body))
		if err != nil {
			t.Fatal("cannot create native request")
		}
		if authenticated {
			req.SetBasicAuth("native-test", password)
		}
		req.Header.Set("Content-Type", "application/json")
		response, err := http.DefaultClient.Do(req)
		if err != nil {
			t.Fatal("native API request failed")
		}
		defer response.Body.Close()
		data, err := io.ReadAll(io.LimitReader(response.Body, 1<<20))
		if err != nil {
			t.Fatal("native API response failed")
		}
		return response.StatusCode, data
	}
	if status, _ := call("GET", "/api/store/proxies", nil, false); status != 401 {
		t.Fatal("native Store API bypassed Dashboard authentication")
	}
	payload, _ := json.Marshal(map[string]any{"name": "native-store", "type": "tcp", "tcp": map[string]any{"localIP": "127.0.0.1", "localPort": localPort, "remotePort": ports[3]}})
	if status, _ := call("POST", "/api/store/proxies", payload, true); status != 200 {
		t.Fatal("unmanaged native Store create failed")
	}
	configNativeWait(t, ctx, "native Store plus file forwarding", processes, func() bool { return configNativeEcho(ports[2]) && configNativeEcho(ports[3]) })
	if status, _ := call("GET", "/api/store/proxies/native-store", nil, true); status != 200 {
		t.Fatal("unmanaged native Store read failed")
	}
	if status, _ := call("PUT", "/api/config", []byte(fileConfig(ports[4])), true); status != 200 {
		t.Fatal("unmanaged native file write failed")
	}
	if !configNativeEcho(ports[2]) || configNativeOpen(ports[4]) {
		t.Fatal("native file write unexpectedly reloaded runtime")
	}
	if status, _ := call("GET", "/api/reload", nil, true); status != 200 {
		t.Fatal("unmanaged native reload failed")
	}
	configNativeWait(t, ctx, "native file reload preserves Store", processes, func() bool {
		return !configNativeOpen(ports[2]) && configNativeEcho(ports[4]) && configNativeEcho(ports[3])
	})
	if status, _ := call("DELETE", "/api/store/proxies/native-store", nil, true); status != 200 {
		t.Fatal("unmanaged native Store delete failed")
	}
	configNativeWait(t, ctx, "native Store withdrawal preserves file", processes, func() bool { return !configNativeOpen(ports[3]) && configNativeEcho(ports[4]) })
	if _, err := os.Stat(filepath.Join(root, "managed")); !os.IsNotExist(err) {
		t.Fatal("unmanaged startup created a management root")
	}
	t.Log("unmanaged native API regression passed: Dashboard auth, Store create/read/delete, file write without reload, explicit reload and independent file/Store forwarding")
}
