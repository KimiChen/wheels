package monitor

import (
	"bufio"
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/fatedier/frp/extension/frpmonitor/monitor/control"
)

func groupRequest(t *testing.T, s *Service, cookie *http.Cookie, csrf, method, path string, input any, status int) *http.Response {
	t.Helper()
	data, err := json.Marshal(input)
	if err != nil {
		t.Fatal(err)
	}
	response := adminRequest(t, s, method, "/api/admin/v1/groups"+path, string(data), cookie, csrf, nil)
	expectStatus(t, response, status)
	return response
}

func createTestGroup(t *testing.T, s *Service, cookie *http.Cookie, csrf, name string, ids []string) control.NodeGroup {
	t.Helper()
	response := groupRequest(t, s, cookie, csrf, http.MethodPost, "", map[string]any{"name": name, "node_ids": ids}, 201)
	defer response.Body.Close()
	var group control.NodeGroup
	if err := json.NewDecoder(response.Body).Decode(&group); err != nil || !validNodeID(group.ID) || group.ConfigRevision != 1 {
		t.Fatalf("invalid group creation: %+v, %v", group, err)
	}
	return group
}

func TestGroupsManagementAndPublicProjection(t *testing.T) {
	s, token, admin := testAdmin(t)
	cookie, session := login(t, s, admin)
	s.configMu.Lock()
	privateConfig := control.DefaultNodeConfig("hidden node")
	privateConfig.IsPublic = false
	hidden, err := s.control.CreateNode(context.Background(), privateConfig, tokenHash(strings.Repeat("z", 43)), nil)
	if err == nil {
		err = s.refreshNodes(context.Background())
	}
	s.configMu.Unlock()
	if err != nil {
		t.Fatal(err)
	}
	shared := createTestGroup(t, s, cookie, session.CSRF, "亚洲", []string{"1", hidden.ID})
	second := createTestGroup(t, s, cookie, session.CSRF, "Web", []string{"1"})
	createTestGroup(t, s, cookie, session.CSRF, "hidden-only-group", []string{hidden.ID})
	createTestGroup(t, s, cookie, session.CSRF, "empty-private-group", []string{})
	checkPublic := func(data []byte) {
		t.Helper()
		for _, private := range []string{"hidden-only-group", "empty-private-group", "hidden node", "node_ids", "config_revision"} {
			if strings.Contains(string(data), private) {
				t.Fatalf("public group projection exposed %q", private)
			}
		}
		var snapshot PublicSnapshot
		if err := json.Unmarshal(data, &snapshot); err != nil || len(snapshot.Nodes) != 1 || len(snapshot.Nodes[0].Groups) != 2 {
			t.Fatalf("public groups: %s, %v", data, err)
		}
		if snapshot.Nodes[0].Groups[0].ID != shared.ID || snapshot.Nodes[0].Groups[1].ID != second.ID {
			t.Fatal("multi-group membership lost")
		}
	}
	response := adminRequest(t, s, "GET", "/api/public/v1/nodes", "", nil, "", nil)
	data, _ := io.ReadAll(response.Body)
	response.Body.Close()
	checkPublic(data)
	stream := adminRequest(t, s, "GET", "/events/public", "", nil, "", nil)
	reader := bufio.NewReader(stream.Body)
	reader.ReadString('\n')
	line, err := reader.ReadString('\n')
	stream.Body.Close()
	if err != nil || !strings.HasPrefix(line, "data: ") {
		t.Fatal("SSE snapshot missing", err)
	}
	checkPublic([]byte(strings.TrimSpace(strings.TrimPrefix(line, "data: "))))
	private := s.adminSnapshot()
	if private.GroupsState != "ready" || len(private.Groups) != 4 || len(private.Nodes) != 2 {
		t.Fatal("administrator cannot see complete group inventory")
	}
	if refreshTokenID(s, token) != "1" {
		t.Fatal("group edits changed agent credentials")
	}
	// Deleting a group detaches its members; node identity remains unchanged.
	pending := pendingConfigurationRead(t, s)
	groupRequest(t, s, cookie, session.CSRF, http.MethodDelete, "/"+second.ID, map[string]any{"config_revision": second.ConfigRevision}, 204).Body.Close()
	s.applyConfiguration(pending)
	if got := s.public.Load().Nodes; len(got) != 1 || len(got[0].Groups) != 1 || refreshTokenID(s, token) != "1" {
		t.Fatal("group deletion removed a node or retained stale membership")
	}
}

func TestGroupRoutesRequireAuthorizationAndRejectInvalidWrites(t *testing.T) {
	s, _, admin := testAdmin(t)
	groupRequest(t, s, nil, "", http.MethodGet, "", nil, 401).Body.Close()
	cookie, session := login(t, s, admin)
	groupRequest(t, s, cookie, "", http.MethodPost, "", map[string]any{"name": "one", "node_ids": []string{"1"}}, 403).Body.Close()
	group := createTestGroup(t, s, cookie, session.CSRF, "one", []string{"1"})
	groupRequest(t, s, cookie, session.CSRF, http.MethodPatch, "/"+group.ID, map[string]any{"name": "renamed", "node_ids": []string{}, "config_revision": 1}, 200).Body.Close()
	groupRequest(t, s, cookie, session.CSRF, http.MethodDelete, "/"+group.ID, map[string]any{"config_revision": 1}, 409).Body.Close()
	for _, body := range []string{`{"name":"bad","node_ids":null}`, `{"name":"bad"}`, `{"name":"bad","node_ids":["01"]}`, `{"name":"bad","node_ids":["1","1"]}`, `{"name":"bad","node_ids":["999"]}`, `{"name":"bad","name":"duplicate","node_ids":[]}`, `{"name":"bad","node_ids":[],"unexpected":true}`} {
		expectStatus(t, adminRequest(t, s, http.MethodPost, "/api/admin/v1/groups", body, cookie, session.CSRF, nil), 400)
	}
	for _, path := range []string{"", "/" + group.ID} {
		response := groupRequest(t, s, cookie, "", http.MethodHead, path, nil, 405)
		if response.Header.Get("Allow") == "" {
			t.Fatal("group method response missing Allow")
		}
		response.Body.Close()
	}
	groupRequest(t, s, cookie, session.CSRF, http.MethodDelete, "/01", map[string]any{"config_revision": 2}, 404).Body.Close()
}

func TestGroupNodeDeletionAndStaleRefresh(t *testing.T) {
	s, _, admin := testAdmin(t)
	cookie, session := login(t, s, admin)
	group := createTestGroup(t, s, cookie, session.CSRF, "members", []string{"1"})
	pending := pendingConfigurationRead(t, s)
	groupRequest(t, s, cookie, session.CSRF, http.MethodPatch, "/"+group.ID, map[string]any{"name": "new name", "node_ids": []string{"1"}, "config_revision": 1}, 200).Body.Close()
	s.applyConfiguration(pending)
	if s.groups.Load().Groups[0].Name != "new name" {
		t.Fatal("stale background read restored old group configuration")
	}
	expectStatus(t, adminRequest(t, s, http.MethodDelete, "/api/admin/v1/nodes/1", "", cookie, session.CSRF, nil), 204)
	groups := s.groups.Load().Groups
	if len(groups) != 1 || len(groups[0].NodeIDs) != 0 || groups[0].ConfigRevision != 3 {
		t.Fatal("node deletion did not publish cleaned group membership", groups)
	}
}

func TestGroupNetworkIOAndCanceledMutation(t *testing.T) {
	s, token, _ := testAdmin(t)
	request := httptest.NewRequest(http.MethodPost, "/api/admin/v1/groups", nil)
	request.Header.Set("Content-Type", "application/json")
	request.Body = io.NopCloser(configUnlockedBody{Reader: strings.NewReader(`{"name":"one","node_ids":["1"]}`), t: t, s: s})
	response := &configUnlockedRecorder{ResponseRecorder: httptest.NewRecorder(), t: t, s: s}
	s.handleAdminGroups(response, request, "")
	if response.Code != 201 {
		t.Fatal("group creation failed", response.Code)
	}
	pending := pendingConfigurationRead(t, s)
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	request = httptest.NewRequest(http.MethodPost, "/api/admin/v1/groups", strings.NewReader(`{"name":"cancelled","node_ids":[]}`)).WithContext(ctx)
	request.Header.Set("Content-Type", "application/json")
	response = &configUnlockedRecorder{ResponseRecorder: httptest.NewRecorder(), t: t, s: s}
	s.handleAdminGroups(response, request, "")
	s.applyConfiguration(pending)
	if response.Code != 503 || !s.groups.Load().Failed || refreshTokenID(s, token) != "1" || len(s.public.Load().Nodes[0].Groups) != 0 {
		t.Fatal("uncertain group write retained old membership or revoked agent credentials")
	}
	s.refreshConfiguration()
	if s.groups.Load().Failed || len(s.groups.Load().Groups) != 1 {
		t.Fatal("group refresh did not recover committed state")
	}
}
