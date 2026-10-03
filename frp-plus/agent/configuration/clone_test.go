package configuration

import (
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/fatedier/frp/extension/frpmonitor/agent/managed"
	"github.com/fatedier/frp/extension/frpmonitor/shared"
)

const cloneStore = `{"proxies":[{"name":"web","type":"http","localPort":8080,"customDomains":["example.invalid"],"httpPassword":"private-http-original","metadatas":{"private":"private-metadata-original"},"requestHeaders":{"set":{"Authorization":"private-header-original"}},"responseHeaders":{"set":{"X-Private":"private-response-original"}},"loadBalancer":{"group":"pool","groupKey":"private-group-original"},"healthCheck":{"type":"http","path":"/health","intervalSeconds":10,"httpHeaders":[{"name":"Authorization","value":"private-health-original"}]}}],"visitors":[]}`

func cloneNamed(t *testing.T, data []byte, kind, name string) object {
	t.Helper()
	objects, err := parseStore(data)
	if err != nil {
		t.Fatal(err)
	}
	for _, o := range objects {
		if o.kind == kind && o.name == name {
			return o
		}
	}
	t.Fatalf("candidate omitted %s/%s", kind, name)
	return object{}
}
func assertClonePrivate(t *testing.T, values ...any) {
	t.Helper()
	for _, value := range values {
		data, err := json.Marshal(value)
		if err != nil {
			t.Fatal(err)
		}
		for _, private := range []string{"private-http-original", "private-metadata-original", "private-header-original", "private-response-original", "private-group-original", "private-health-original", "private-vault-value", "private-visitor-original"} {
			if strings.Contains(string(data), private) {
				t.Fatal("private clone material escaped preview")
			}
		}
	}
}
func TestCloneAndRenamePreserveOriginalSecretsAndAdvancedFields(t *testing.T) {
	var renameDigest string
	for _, mode := range []string{"clone", "delete_first", "create_first"} {
		t.Run(mode, func(t *testing.T) {
			input := fixture(t, cloneStore, nil)
			original := cloneNamed(t, []byte(cloneStore), "proxy", "web")
			clone := Change{Operation: "create", Kind: "proxy", Name: "new-web", Type: "http", CloneFrom: "web", Fields: fields(map[string]any{"localPort": 9090})}
			remove := Change{Operation: "delete", Kind: "proxy", Name: "web"}
			changes := []Change{clone}
			if mode == "delete_first" {
				changes = []Change{remove, clone}
			}
			if mode == "create_first" {
				changes = append(changes, remove)
			}
			out, err := Prepare(input, Request{ExpectedRevision: ready(t, input).Revision, Changes: changes})
			if err != nil {
				t.Fatal(err)
			}
			copied := cloneNamed(t, out.StoreBytes, "proxy", "new-web")
			for _, key := range []string{"httpPassword", "metadatas", "requestHeaders", "responseHeaders", "loadBalancer", "healthCheck", "customDomains"} {
				if !sameJSON(original.raw[key], copied.raw[key]) {
					t.Fatalf("clone lost original %s", key)
				}
			}
			if string(copied.raw["localPort"]) != "9090" {
				t.Fatal("clone ignored explicit field edit")
			}
			objects, _ := parseStore(out.StoreBytes)
			if mode == "clone" {
				if len(objects) != 2 {
					t.Fatal("copy removed source")
				}
				source := cloneNamed(t, out.StoreBytes, "proxy", "web")
				if string(source.raw["localPort"]) != "8080" {
					t.Fatal("copy mutated source")
				}
			} else {
				if len(objects) != 1 {
					t.Fatal("rename retained source")
				}
				if renameDigest != "" && renameDigest != out.CandidateSHA256 {
					t.Fatal("rename depends on delete/create ordering")
				}
				renameDigest = out.CandidateSHA256
			}
			after, _ := os.ReadFile(input.StoreFile)
			if string(after) != cloneStore {
				t.Fatal("prepare wrote live Store")
			}
			wire := previewView(out, managed.Digest(managed.StoreSnapshot{Exists: true, Bytes: out.StoreBytes}))
			if err := wire.Validate(); err != nil {
				t.Fatal("clone preview cannot use wire contract", err)
			}
			assertClonePrivate(t, ready(t, input), out, wire)
		})
	}
}
func TestCloneUsesInitialSourceEvenWhenSourceChangesEarlier(t *testing.T) {
	var digest string
	for _, cloneFirst := range []bool{false, true} {
		input := fixture(t, cloneStore, nil)
		update := Change{Operation: "update", Kind: "proxy", Name: "web", Fields: fields(map[string]any{"localPort": 7070}), Secrets: map[string]SecretAction{"httpPassword": {Mode: "reference", Reference: secretID}}}
		clone := Change{Operation: "create", Kind: "proxy", Name: "copy", Type: "http", CloneFrom: "web"}
		changes := []Change{update, clone}
		if cloneFirst {
			changes = []Change{clone, update}
		}
		out, err := Prepare(input, Request{ExpectedRevision: ready(t, input).Revision, Changes: changes})
		if err != nil {
			t.Fatal(err)
		}
		source, copied := cloneNamed(t, out.StoreBytes, "proxy", "web"), cloneNamed(t, out.StoreBytes, "proxy", "copy")
		if string(source.raw["httpPassword"]) != `"private-vault-value"` || string(source.raw["localPort"]) != "7070" {
			t.Fatal("source edit did not apply")
		}
		if string(copied.raw["httpPassword"]) != `"private-http-original"` || string(copied.raw["localPort"]) != "8080" {
			t.Fatal("clone read modified rather than initial source")
		}
		if digest != "" && digest != out.CandidateSHA256 {
			t.Fatal("clone source edit ordering changed transaction")
		}
		digest = out.CandidateSHA256
	}
}
func TestCloneSecretKeepClearReferenceAndVisitor(t *testing.T) {
	for _, mode := range []string{"keep", "clear", "reference"} {
		input := fixture(t, cloneStore, nil)
		action := SecretAction{Mode: mode}
		if mode == "reference" {
			action.Reference = secretID
		}
		out := prepare(t, input, Change{Operation: "create", Kind: "proxy", Name: "copy", Type: "http", CloneFrom: "web", Secrets: map[string]SecretAction{"httpPassword": action}})
		copied := cloneNamed(t, out.StoreBytes, "proxy", "copy")
		value, exists := copied.raw["httpPassword"]
		switch mode {
		case "keep":
			if string(value) != `"private-http-original"` {
				t.Fatal("keep changed inherited secret")
			}
		case "clear":
			if exists {
				t.Fatal("clear retained inherited secret")
			}
		case "reference":
			if string(value) != `"private-vault-value"` {
				t.Fatal("reference did not replace inherited secret")
			}
		}
		assertClonePrivate(t, out, previewView(out, managed.Digest(managed.StoreSnapshot{Exists: true, Bytes: out.StoreBytes})))
	}
	input := fixture(t, `{"proxies":[],"visitors":[{"name":"visit","type":"stcp","serverName":"peer","bindPort":9000,"secretKey":"private-visitor-original"}]}`, nil)
	out := prepare(t, input, Change{Operation: "create", Kind: "visitor", Name: "visit-copy", Type: "stcp", CloneFrom: "visit", Fields: fields(map[string]any{"bindPort": 9001})})
	visitor := cloneNamed(t, out.StoreBytes, "visitor", "visit-copy")
	if string(visitor.raw["secretKey"]) != `"private-visitor-original"` {
		t.Fatal("visitor lost inherited secret")
	}
	assertClonePrivate(t, out)
}
func TestCloneRejectsForeignOwnershipTypePluginAndNonInitialSource(t *testing.T) {
	input := fixture(t, cloneStore, nil)
	for _, tc := range []struct {
		change Change
		want   string
	}{
		{Change{Operation: "update", Kind: "proxy", Name: "web", CloneFrom: "other"}, "invalid_change"},
		{Change{Operation: "delete", Kind: "proxy", Name: "web", CloneFrom: "other"}, "invalid_change"},
		{Change{Operation: "create", Kind: "proxy", Name: "web", Type: "http", CloneFrom: "web"}, "invalid_change"},
		{Change{Operation: "create", Kind: "proxy", Name: "copy", Type: "tcp", CloneFrom: "web"}, "invalid_type"},
		{Change{Operation: "create", Kind: "visitor", Name: "copy", Type: "stcp", CloneFrom: "web"}, "not_found"},
		{Change{Operation: "create", Kind: "proxy", Name: "copy", Type: "http", CloneFrom: "missing"}, "not_found"},
		{Change{Operation: "create", Kind: "proxy", Name: "copy", Type: "http", CloneFrom: "bad\nname"}, "invalid_change"},
	} {
		_, err := Prepare(input, Request{ExpectedRevision: ready(t, input).Revision, Changes: []Change{tc.change}})
		expectCode(t, err, tc.want)
	}
	_, err := Prepare(input, Request{ExpectedRevision: ready(t, input).Revision, Changes: []Change{{Operation: "create", Kind: "proxy", Name: "copy", Type: "http", CloneFrom: "web"}, {Operation: "create", Kind: "proxy", Name: "copy2", Type: "http", CloneFrom: "copy"}}})
	expectCode(t, err, "not_found")
	plugin := fixture(t, `{"proxies":[{"name":"plugin","type":"tcp","plugin":{"type":"socks5","password":"private-plugin-value"}}]}`, nil)
	_, err = Prepare(plugin, Request{ExpectedRevision: ready(t, plugin).Revision, Changes: []Change{{Operation: "create", Kind: "proxy", Name: "copy", Type: "tcp", CloneFrom: "plugin"}}})
	expectCode(t, err, "advanced_read_only")
	dir, err := filepath.EvalSymlinks(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	include := filepath.Join(dir, "include.json")
	if err := os.WriteFile(include, []byte(`{"proxies":[{"name":"included","type":"tcp","localPort":80,"enabled":false}]}`), 0600); err != nil {
		t.Fatal(err)
	}
	foreign := fixture(t, "", map[string]any{"includes": []string{include}, "proxies": []any{map[string]any{"name": "inline", "type": "tcp", "localPort": 80, "enabled": false}}})
	for _, source := range []string{"inline", "included"} {
		_, err := Prepare(foreign, Request{ExpectedRevision: ready(t, foreign).Revision, Changes: []Change{{Operation: "create", Kind: "proxy", Name: "copy", Type: "tcp", CloneFrom: source}}})
		expectCode(t, err, "ownership_conflict")
	}
}
func TestProviderCloneMappingPreservesSecretsWithoutReturningThem(t *testing.T) {
	if runtime.GOOS != "linux" && runtime.GOOS != "darwin" {
		t.Skip("managed Store requires supported private filesystem")
	}
	input := fixture(t, cloneStore, nil)
	engine, err := managed.Open(managed.Options{Root: input.WorkingDir, StorePath: input.StoreFile})
	if err != nil {
		t.Fatal(err)
	}
	defer engine.Close()
	var mu sync.Mutex
	runtimeDigest := managed.Digest(managed.StoreSnapshot{Exists: true, Bytes: []byte(cloneStore)})
	if err := engine.AttachRuntime(managed.Runtime{
		Check: func(context.Context, managed.Operation, string) error { return nil },
		Apply: func(_ context.Context, snapshot managed.StoreSnapshot) error {
			values, err := parseStore(snapshot.Bytes)
			if err != nil {
				return err
			}
			mu.Lock()
			input.StoreMemory = native(values)
			runtimeDigest = managed.Digest(snapshot)
			mu.Unlock()
			return nil
		},
		Verify: func(_ context.Context, snapshot managed.StoreSnapshot) (managed.Verification, error) {
			mu.Lock()
			defer mu.Unlock()
			loaded := runtimeDigest == managed.Digest(snapshot)
			return managed.Verification{RuntimeLoaded: loaded, ResourcesReady: loaded}, nil
		},
	}); err != nil {
		t.Fatal(err)
	}
	provider, err := NewProvider(engine, func() (Input, error) { mu.Lock(); defer mu.Unlock(); return input, nil })
	if err != nil {
		t.Fatal(err)
	}
	now := time.Now().UTC()
	requestID, _ := shared.NewConfigOperationID()
	opID, _ := shared.NewConfigOperationID()
	idem, _ := shared.NewConfigOperationID()
	command := shared.ConfigCommand{Meta: shared.Meta{Schema: 1, SessionID: "clone-test", Sequence: 2, CollectedAt: now.Format(time.RFC3339Nano)}, RequestID: requestID, ServiceID: engine.ServiceID(), Action: "prepare", OperationID: opID, BaseRevision: ready(t, input).Revision, IdempotencyKey: idem, DeadlineAtMS: now.Add(10 * time.Second).UnixMilli(), OperationDeadlineAtMS: now.Add(time.Minute).UnixMilli(), Changes: []shared.ConfigChange{{Operation: "create", Kind: "proxy", Name: "copy", Type: "http", CloneFrom: "web", Fields: []shared.ConfigFieldPatch{{Path: "localPort", Value: json.RawMessage("9090")}}, Secrets: []shared.ConfigSecretPatch{}}}}
	result := provider.HandleConfig(context.Background(), command)
	if result.Code != "ok" || result.Preview == nil || result.Operation == nil || result.Validate() != nil {
		t.Fatalf("clone was not mapped into prepare: %+v", result)
	}
	assertClonePrivate(t, result)
	command.Action = "apply"
	command.IdempotencyKey = ""
	command.OperationDeadlineAtMS = 0
	command.Changes = []shared.ConfigChange{}
	command.ContextRevision = result.Operation.ContextRevision
	command.CandidateDigest = result.Operation.CandidateDigest
	result = provider.HandleConfig(context.Background(), command)
	if result.Code != "ok" || result.Operation == nil || result.Operation.State != "confirmed" {
		t.Fatal("cloned candidate did not apply")
	}
	current, _ := os.ReadFile(input.StoreFile)
	copied := cloneNamed(t, current, "proxy", "copy")
	if string(copied.raw["httpPassword"]) != `"private-http-original"` {
		t.Fatal("bridge lost copied credential")
	}
	assertClonePrivate(t, result)
}
