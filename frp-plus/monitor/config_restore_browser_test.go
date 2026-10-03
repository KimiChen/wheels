package monitor

import (
	"bytes"
	"context"
	"encoding/json"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"testing"
)

// Optional browser takeover inside the real checkpoint/native restore fixture.
// The session is disposable and reaches the helper through stdin only.
func runConfigRestoreBrowser(t *testing.T, ctx context.Context, s *Service, cookie *http.Cookie, root string) {
	t.Helper()
	helper, node := os.Getenv("FRP_CONFIG_RESTORE_BROWSER_HELPER"), os.Getenv("FRP_CONFIG_E2E_NODE")
	for _, path := range []string{helper, node} {
		info, err := os.Stat(path)
		if !filepath.IsAbs(path) || err != nil || !info.Mode().IsRegular() {
			t.Fatal("restore browser inputs must be absolute regular files")
		}
	}
	payload, err := json.Marshal(map[string]any{"url": "http://" + s.Address(), "cookie": map[string]string{"name": cookie.Name, "value": cookie.Value}})
	if err != nil {
		t.Fatal("cannot encode restore browser session")
	}
	command := exec.CommandContext(ctx, node, helper)
	command.Dir = root
	command.Stdin = bytes.NewReader(payload)
	command.Env = []string{"PATH=" + os.Getenv("PATH"), "HOME=" + root, "TMPDIR=" + root, "LANG=C", "LC_ALL=C", "PLAYWRIGHT_MODULE=" + os.Getenv("PLAYWRIGHT_MODULE"), "BROWSER_EXECUTABLE=" + os.Getenv("BROWSER_EXECUTABLE")}
	output, runErr := command.CombinedOutput()
	var report struct {
		OK    bool   `json:"ok"`
		Stage string `json:"stage"`
	}
	if len(output) > 1024 || json.Unmarshal(output, &report) != nil || !regexp.MustCompile(`^[a-z_]{1,64}$`).MatchString(report.Stage) {
		t.Fatal("restore browser did not return a bounded safe report")
	}
	if runErr != nil || !report.OK {
		t.Fatalf("restore browser failed at safe stage %q", report.Stage)
	}
	t.Log("actual restore browser takeover passed: explicit consent, closed write gate, one acknowledgement and offline-confirm instructions")
}
