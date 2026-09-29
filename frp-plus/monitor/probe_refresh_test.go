package monitor

import (
	"context"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/fatedier/frp/extension/frpmonitor/monitor/control"
	"github.com/fatedier/frp/extension/frpmonitor/shared"
)

func testConfigurationRefresh(t *testing.T) *Service {
	t.Helper()
	dir, err := filepath.EvalSymlinks(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	if err := os.Chmod(dir, 0700); err != nil {
		t.Fatal(err)
	}
	db, err := control.Open(control.Config{Path: filepath.Join(dir, "control.sqlite"), Location: time.UTC})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		if err := db.Close(); err != nil {
			t.Error(err)
		}
	})
	if _, err := db.CreateNode(context.Background(), control.DefaultNodeConfig("before"), tokenHash(strings.Repeat("a", 43)), nil); err != nil {
		t.Fatal(err)
	}
	// No listener or background refresh: the two phases can be interleaved
	// deterministically with real committed database mutations.
	s := &Service{ctx: context.Background(), cfg: shared.MonitorConfig{ReportIntervalSeconds: 1}, location: time.UTC, control: db, nodes: map[string]*node{}}
	s.tasks.Store(&probeBook{Nodes: map[string][]configuredProbe{}})
	s.refreshConfiguration()
	if s.configs.Load() == nil || s.tasks.Load().Version != 1 || s.credentialError.Load() || s.taskError.Load() {
		t.Fatal("initial refresh failed")
	}
	return s
}

func pendingConfigurationRead(t *testing.T, s *Service) configurationRead {
	t.Helper()
	read := configurationRead{configs: s.configs.Load(), tasks: s.tasks.Load(), groups: s.groups.Load()}
	ctx, cancel := context.WithTimeout(context.Background(), time.Second)
	defer cancel()
	read.nodes, read.nodeErr = s.control.Nodes(ctx)
	read.taskData, read.taskErr = s.control.ReadProbes(ctx)
	read.groupData, read.groupErr = s.control.Groups(ctx)
	if read.nodeErr != nil || read.taskErr != nil {
		t.Fatalf("read pending configuration: %v, %v", read.nodeErr, read.taskErr)
	}
	return read
}

func refreshTokenID(s *Service, token string) string {
	r := httptest.NewRequest("GET", "http://example.invalid/agent/v1/ws", nil)
	r.Header.Set("Authorization", "Bearer "+token)
	return s.authenticateCredential(r).AgentID
}

func TestConfigurationRefreshCannotRestoreRevokedCredentials(t *testing.T) {
	for _, mutation := range []string{"delete", "rotate", "invalidate"} {
		t.Run(mutation, func(t *testing.T) {
			s := testConfigurationRefresh(t)
			pending := pendingConfigurationRead(t, s)
			oldToken, newToken := strings.Repeat("a", 43), strings.Repeat("b", 43)
			if refreshTokenID(s, oldToken) != "1" {
				t.Fatal("old token did not initially authenticate")
			}
			s.configMu.Lock()
			var err error
			switch mutation {
			case "delete":
				err = s.control.DeleteNode(context.Background(), "1")
			case "rotate":
				err = s.control.RotateToken(context.Background(), "1", tokenHash(newToken))
			case "invalidate":
				s.controlWriteError(context.DeadlineExceeded)
			}
			if mutation != "invalidate" && err == nil {
				s.refreshCommitted(context.Background())
			}
			s.configMu.Unlock()
			if err != nil {
				t.Fatal(err)
			}
			current := s.configs.Load()
			s.applyConfiguration(pending)
			if s.configs.Load() != current || refreshTokenID(s, oldToken) != "" {
				t.Fatal("pre-mutation database read restored old authorization")
			}
			if mutation == "rotate" {
				if refreshTokenID(s, newToken) != "1" {
					t.Fatal("stale refresh replaced the new token")
				}
			} else if len(s.public.Load().Nodes) != 0 {
				t.Fatal("stale refresh restored a removed public row")
			}
			if mutation == "invalidate" && !s.credentialError.Load() {
				t.Fatal("stale refresh cleared uncertain-commit invalidation")
			}
		})
	}
}

const updatedProbeConfig = `{"version":2,"nodes":[{"agent_id":"1","tasks":[{"id":"tcp","name":"new target","target":"example.invalid:443","interval":5}]}]}`

func TestConfigurationRefreshCannotOverwriteNewProbeBook(t *testing.T) {
	s := testConfigurationRefresh(t)
	pending := pendingConfigurationRead(t, s)
	s.configMu.Lock()
	err := s.control.WriteProbes(context.Background(), []byte(updatedProbeConfig))
	if err == nil {
		s.reloadTasks()
	}
	s.configMu.Unlock()
	if err != nil {
		t.Fatal(err)
	}
	currentTasks, currentConfigs := s.tasks.Load(), s.configs.Load()
	if currentTasks.Version != 2 || s.taskError.Load() {
		t.Fatal("probe update failed")
	}
	s.applyConfiguration(pending)
	if s.tasks.Load() != currentTasks || s.configs.Load() != currentConfigs || s.taskError.Load() {
		t.Fatal("stale refresh overwrote or degraded the new probe configuration")
	}
}

func TestConfigurationRefreshAppliesCurrentDatabaseRead(t *testing.T) {
	s := testConfigurationRefresh(t)
	n, err := s.control.Get(context.Background(), "1")
	if err != nil {
		t.Fatal(err)
	}
	n.Name = "after"
	if _, err := s.control.UpdateNode(context.Background(), "1", n.NodeConfig, nil, false, n.ConfigRevision); err != nil {
		t.Fatal(err)
	}
	if err := s.control.WriteProbes(context.Background(), []byte(updatedProbeConfig)); err != nil {
		t.Fatal(err)
	}
	s.credentialError.Store(true)
	s.taskError.Store(true)
	s.refreshConfiguration()
	if s.credentialError.Load() || s.taskError.Load() || s.tasks.Load().Version != 2 {
		t.Fatal("current refresh did not recover healthy configuration")
	}
	if nodes := s.public.Load().Nodes; len(nodes) != 1 || nodes[0].Name != "after" || refreshTokenID(s, strings.Repeat("a", 43)) != "1" {
		t.Fatal("current node configuration was not published")
	}
}
