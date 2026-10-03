package monitor

import (
	"bytes"
	"context"
	"encoding/json"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"testing"
	"time"

	"github.com/fatedier/frp/extension/frpmonitor/monitor/control"
)

// Real Handler, authentication, SQLite and audit API; only the external GitHub
// provider is a fixture. Lifecycle records are seeded, not native execution.
func TestAuditBrowserEndToEnd(t *testing.T) {
	helper, node := os.Getenv("FRP_AUDIT_BROWSER_HELPER"), os.Getenv("FRP_CONFIG_E2E_NODE")
	if helper == "" || node == "" {
		t.Skip("audit browser requires FRP_AUDIT_BROWSER_HELPER and FRP_CONFIG_E2E_NODE")
	}
	for _, path := range []string{helper, node} {
		info, err := os.Stat(path)
		if !filepath.IsAbs(path) || err != nil || !info.Mode().IsRegular() {
			t.Fatal("audit browser inputs must be absolute regular files")
		}
	}
	ctx, cancel := context.WithTimeout(context.Background(), time.Minute)
	defer cancel()
	s, _, admin := testAdmin(t)
	cookie, _ := login(t, s, admin)
	for i := 0; i < 53; i++ {
		input := control.AuditInput{Kind: "preview", ActorKind: "github", Actor: "operator", NodeID: "1", ServiceID: "primary", Code: "preview_available", State: "prepared", Changes: []control.ConfigOperationChange{{Kind: "proxy", Name: "audit-sample", Action: "update", Fields: []string{"secretKey", "transport.useCompression"}}}}
		if i == 52 {
			input.Kind, input.ActorKind, input.Actor, input.Observer, input.Code, input.State = "external_drift", "unknown", "", "operator", "source_drift", ""
		}
		if _, err := s.control.RecordAudit(ctx, input); err != nil {
			t.Fatal("cannot seed private audit browser records")
		}
	}
	root := t.TempDir()
	payload, err := json.Marshal(map[string]any{"url": "http://" + s.Address(), "cookie": map[string]string{"name": cookie.Name, "value": cookie.Value}, "expected_count": 53})
	if err != nil {
		t.Fatal("cannot encode private audit browser input")
	}
	command := exec.CommandContext(ctx, node, helper)
	command.Dir, command.Stdin = root, bytes.NewReader(payload)
	command.Env = []string{"PATH=" + os.Getenv("PATH"), "HOME=" + root, "TMPDIR=" + root, "LANG=C", "LC_ALL=C", "PLAYWRIGHT_MODULE=" + os.Getenv("PLAYWRIGHT_MODULE"), "BROWSER_EXECUTABLE=" + os.Getenv("BROWSER_EXECUTABLE")}
	output, runErr := command.CombinedOutput()
	var report struct {
		OK    bool   `json:"ok"`
		Stage string `json:"stage"`
	}
	if len(output) > 1024 || json.Unmarshal(output, &report) != nil || !regexp.MustCompile(`^[a-z_]{1,64}$`).MatchString(report.Stage) {
		t.Fatal("audit browser did not return a bounded safe report")
	}
	if runErr != nil || !report.OK {
		t.Fatalf("audit browser failed at safe stage %q", report.Stage)
	}
	page, err := s.control.ListAudit(ctx, control.AuditFilter{Kind: "export"}, "", 50)
	if err != nil || len(page.Items) != 2 {
		t.Fatal("explicit audit export did not retain its requested and prepared records")
	}
	t.Log("actual audit UI/API/SQLite passed: stable pagination, unknown actor, legacy Service ID, filters, explicit bounded export and session clearing")
}
