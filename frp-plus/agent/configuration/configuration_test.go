package configuration

import (
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"

	nativeconfig "github.com/fatedier/frp/pkg/config"
	"github.com/fatedier/frp/pkg/config/source"
)

const secretID = "12345678-1234-4234-8234-123456789abc"

func fixture(t *testing.T, store string, main map[string]any) Input {
	t.Helper()
	dir, err := filepath.EvalSymlinks(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	if err = os.Chmod(dir, 0700); err != nil {
		t.Fatal(err)
	}
	if main == nil {
		main = map[string]any{}
	}
	main["serverAddr"] = "127.0.0.1"
	main["serverPort"] = 7000
	main["store"] = map[string]any{"path": filepath.Join(dir, "store.json")}
	file := filepath.Join(dir, "frpc.json")
	data, _ := json.Marshal(main)
	if err = os.WriteFile(file, data, 0600); err != nil {
		t.Fatal(err)
	}
	storeFile := filepath.Join(dir, "store.json")
	if store == "" {
		store = `{"proxies":[],"visitors":[]}`
	}
	if err = os.WriteFile(storeFile, []byte(store), 0600); err != nil {
		t.Fatal(err)
	}
	loaded, err := nativeconfig.LoadClientConfigResult(file, true)
	if err != nil {
		t.Fatal("native fixture file decode failed")
	}
	s, err := source.NewStoreSource(source.StoreSourceConfig{Path: storeFile})
	if err != nil {
		t.Fatal("native fixture Store decode failed")
	}
	ps, err := s.GetAllProxies()
	if err != nil {
		t.Fatal(err)
	}
	vs, err := s.GetAllVisitors()
	if err != nil {
		t.Fatal(err)
	}
	return Input{ConfigFile: file, StoreFile: storeFile, WorkingDir: dir, StartupCommon: loaded.Common, ReloadCommon: loaded.Common,
		FileMemory: Objects{Proxies: loaded.Proxies, Visitors: loaded.Visitors}, StoreMemory: Objects{Proxies: ps, Visitors: vs},
		TemplateEnv: map[string]string{}, HTTPProxy: os.Getenv("http_proxy"), LocalSecrets: map[string]string{secretID: "private-vault-value"}}
}

func fields(values map[string]any) map[string]json.RawMessage {
	out := map[string]json.RawMessage{}
	for key, value := range values {
		out[key], _ = json.Marshal(value)
	}
	return out
}
func ready(t *testing.T, input Input) *Inspection {
	t.Helper()
	result, err := Inspect(input)
	if err != nil {
		t.Fatal(err)
	}
	if result.State != "ready" {
		t.Fatalf("unexpected read-only codes: %#v", result.Issues)
	}
	return result
}
func prepare(t *testing.T, input Input, change Change) *Prepared {
	t.Helper()
	result, err := Prepare(input, Request{ExpectedRevision: ready(t, input).Revision, Changes: []Change{change}})
	if err != nil {
		t.Fatal(err)
	}
	return result
}
func expectCode(t *testing.T, err error, want string) {
	t.Helper()
	if err == nil || code(err) != want {
		t.Fatalf("expected code %s, got %v", want, err)
	}
}
func hasIssue(issues []Issue, want string) bool {
	for _, i := range issues {
		if i.Code == want {
			return true
		}
	}
	return false
}

func TestAllNativeTypesPrepareWithoutMutation(t *testing.T) {
	cases := []struct {
		kind, typ string
		fields    map[string]any
		badPath   string
		badValue  any
	}{
		{"proxy", "tcp", map[string]any{"localPort": 8080, "remotePort": 0}, "localPort", 0},
		{"proxy", "udp", map[string]any{"localPort": 8080, "remotePort": 8081}, "remotePort", -1},
		{"proxy", "http", map[string]any{"localPort": 8080, "customDomains": []string{"example.invalid"}}, "customDomains", []string{}},
		{"proxy", "https", map[string]any{"localPort": 8443, "subdomain": "app"}, "subdomain", ""},
		{"proxy", "tcpmux", map[string]any{"localPort": 8080, "customDomains": []string{"example.invalid"}, "multiplexer": "httpconnect"}, "multiplexer", "invalid"},
		{"proxy", "stcp", map[string]any{"localPort": 8080}, "localPort", 0},
		{"proxy", "sudp", map[string]any{"localPort": 8080}, "localPort", 0},
		{"proxy", "xtcp", map[string]any{"localPort": 8080}, "localPort", 0},
		{"visitor", "stcp", map[string]any{"serverName": "peer", "bindPort": 8080}, "serverName", ""},
		{"visitor", "sudp", map[string]any{"serverName": "peer", "bindPort": 8080}, "bindPort", -1},
		{"visitor", "xtcp", map[string]any{"serverName": "peer", "bindPort": 8080, "protocol": "quic"}, "protocol", "invalid"},
	}
	for _, tc := range cases {
		t.Run(tc.kind+"_"+tc.typ, func(t *testing.T) {
			input := fixture(t, "", nil)
			before, _ := os.ReadFile(input.StoreFile)
			memory, _ := json.Marshal(input)
			change := Change{Operation: "create", Kind: tc.kind, Name: "entry", Type: tc.typ, Fields: fields(tc.fields), Secrets: map[string]SecretAction{}}
			for _, path := range secretFields(tc.kind, tc.typ) {
				change.Secrets[path] = SecretAction{Mode: "reference", Reference: secretID}
			}
			out := prepare(t, input, change)
			if len(out.Store.Proxies)+len(out.Store.Visitors) != 1 || len(out.Effective.Proxies)+len(out.Effective.Visitors) != 1 {
				t.Fatal("candidate missing native object")
			}
			if _, err := parseStore(out.StoreBytes); err != nil {
				t.Fatal(err)
			}
			after, _ := os.ReadFile(input.StoreFile)
			current, _ := json.Marshal(input)
			if string(before) != string(after) || string(memory) != string(current) {
				t.Fatal("Prepare mutated input or disk")
			}
			wire, _ := json.Marshal(out)
			if strings.Contains(string(wire), "private-vault-value") || strings.Contains(string(wire), input.WorkingDir) {
				t.Fatal("private transaction material escaped preview")
			}
			bad := change
			bad.Fields = fields(tc.fields)
			bad.Fields[tc.badPath], _ = json.Marshal(tc.badValue)
			if _, err := Prepare(input, Request{ExpectedRevision: ready(t, input).Revision, Changes: []Change{bad}}); err == nil {
				t.Fatal("invalid native candidate was accepted")
			}
		})
	}
}

func TestPreservesOpaqueKnownFieldsAndSecrets(t *testing.T) {
	store := `{"proxies":[{"name":"web","type":"http","localPort":8080,"customDomains":["example.invalid"],"httpPassword":"private-http-value","metadatas":{"private":"private-metadata-value"},"requestHeaders":{"set":{"Authorization":"private-header-value"}},"responseHeaders":{"set":{"X-Private":"private-response-value"}},"loadBalancer":{"group":"pool","groupKey":"private-group-value"},"healthCheck":{"type":"http","path":"/health","intervalSeconds":10,"httpHeaders":[{"name":"Authorization","value":"private-health-value"}]}}]}`
	input := fixture(t, store, nil)
	out := prepare(t, input, Change{Operation: "update", Kind: "proxy", Name: "web", Fields: fields(map[string]any{"localPort": 9090})})
	original, _ := parseStore([]byte(store))
	next, err := parseStore(out.StoreBytes)
	if err != nil {
		t.Fatal(err)
	}
	for _, key := range []string{"httpPassword", "metadatas", "requestHeaders", "responseHeaders", "loadBalancer", "healthCheck"} {
		if !sameJSON(json.RawMessage(original[0].raw[key]), json.RawMessage(next[0].raw[key])) {
			t.Fatal("unedited advanced field was lost")
		}
	}
	for _, payload := range []any{ready(t, input), out} {
		data, _ := json.Marshal(payload)
		for _, value := range []string{"private-http-value", "private-metadata-value", "private-header-value", "private-response-value", "private-group-value", "private-health-value"} {
			if strings.Contains(string(data), value) {
				t.Fatal("secret or opaque field escaped preview")
			}
		}
	}
	for _, path := range []string{"requestHeaders", "plugin", "metadatas", "httpPassword", "secretKey", "name", "type"} {
		_, err = Prepare(input, Request{ExpectedRevision: ready(t, input).Revision, Changes: []Change{{Operation: "update", Kind: "proxy", Name: "web", Fields: fields(map[string]any{path: "cannot-write"})}}})
		expectCode(t, err, "field_read_only")
	}
	cleared := prepare(t, input, Change{Operation: "update", Kind: "proxy", Name: "web", Secrets: map[string]SecretAction{"httpPassword": {Mode: "clear"}, "loadBalancer.groupKey": {Mode: "keep"}}})
	objects, _ := parseStore(cleared.StoreBytes)
	if _, exists := objects[0].raw["httpPassword"]; exists {
		t.Fatal("explicit secret clear was ignored")
	}
	if group, _ := getPath(objects[0].raw, "loadBalancer.groupKey"); !strings.Contains(string(group), "private-group-value") {
		t.Fatal("secret keep lost original")
	}
	_, err = Prepare(input, Request{ExpectedRevision: ready(t, input).Revision, Changes: []Change{{Operation: "update", Kind: "proxy", Name: "web", Secrets: map[string]SecretAction{"httpPassword": {Mode: "reference", Reference: "/private/path"}}}}})
	expectCode(t, err, "invalid_secret_reference")
}

func TestStoreAmbiguityCannotBeManaged(t *testing.T) {
	for _, raw := range []string{
		`{"proxies":[],"proxies":[]}`,
		`{"proxies":[{"name":"x","type":"tcp","localPort":80,"transport":{"useEncryption":true,"useEncryption":false}}]}`,
		`{"proxies":[{"name":"x","type":"tcp","localPort":80},{"name":"x","type":"tcp","localPort":81}]}`,
		`{"proxies":[],"hidden":"private-secret"}`,
		`{"proxies":[{"name":"x","type":"tcp","localPort":80,"hidden":"private-secret"}]}`,
		`{"proxies":[{"name":"x","type":"tcp","localPort":80,"plugin":{"type":"socks5","hidden":"private-secret"}}]}`,
	} {
		t.Run(hash([]byte(raw))[:8], func(t *testing.T) {
			input := fixture(t, "", nil)
			before := ready(t, input)
			if err := os.WriteFile(input.StoreFile, []byte(raw), 0600); err != nil {
				t.Fatal(err)
			}
			after, err := Inspect(input)
			if err != nil {
				t.Fatal(err)
			}
			if after.State != "read_only" || after.Revision == before.Revision {
				t.Fatal("ambiguous Store was accepted")
			}
			encoded, _ := json.Marshal(after)
			if strings.Contains(string(encoded), "private-secret") {
				t.Fatal("raw unknown member leaked")
			}
			_, err = Prepare(input, Request{ExpectedRevision: after.Revision, Changes: []Change{{Operation: "create", Kind: "proxy", Name: "new", Type: "tcp", Fields: fields(map[string]any{"localPort": 80})}}})
			if err == nil {
				t.Fatal("ambiguous Store remained writable")
			}
		})
	}
}

func TestDisabledFileAndIncludeOwnershipIsComplete(t *testing.T) {
	dir, _ := filepath.EvalSymlinks(t.TempDir())
	include := filepath.Join(dir, "extra.json")
	if err := os.WriteFile(include, []byte(`{"proxies":[{"name":"included","type":"tcp","localPort":80,"enabled":false}],"visitors":[{"name":"visit","type":"stcp","serverName":"peer","bindPort":9000,"enabled":false}]}`), 0600); err != nil {
		t.Fatal(err)
	}
	input := fixture(t, "", map[string]any{"includes": []string{include}, "proxies": []any{map[string]any{"name": "inline", "type": "tcp", "localPort": 81, "enabled": false}}})
	inspection := ready(t, input)
	if len(inspection.Objects) != 3 {
		t.Fatal("disabled file/include objects missing")
	}
	for _, o := range inspection.Objects {
		if o.Active || o.Writable {
			t.Fatal("disabled file object writable or active")
		}
	}
	for _, name := range []string{"inline", "included"} {
		_, err := Prepare(input, Request{ExpectedRevision: inspection.Revision, Changes: []Change{{Operation: "create", Kind: "proxy", Name: name, Type: "tcp", Fields: fields(map[string]any{"localPort": 90})}}})
		expectCode(t, err, "ownership_conflict")
	}
}

func TestAllCASInputsAndIndependentRecoveryContext(t *testing.T) {
	input := fixture(t, `{"proxies":[{"name":"x","type":"tcp","localPort":80}]}`, nil)
	before := ready(t, input)
	context, err := InspectContext(input)
	if err != nil || context != before.ContextRevision {
		t.Fatal("context-only inspection differs")
	}
	mutations := []struct {
		name   string
		change func(*Input)
	}{
		{"memory", func(in *Input) {
			in.StoreMemory.Proxies[0] = in.StoreMemory.Proxies[0].Clone()
			in.StoreMemory.Proxies[0].GetBaseConfig().LocalPort = 81
		}},
		{"raw_store", func(in *Input) {
			data, _ := os.ReadFile(in.StoreFile)
			_ = os.WriteFile(in.StoreFile, append(data, ' '), 0600)
		}},
		{"http_proxy", func(in *Input) { in.HTTPProxy = "http://proxy.invalid:80" }},
		{"startup_common", func(in *Input) { in.StartupCommon, _ = commonCopy(in.StartupCommon); in.StartupCommon.ServerPort++ }},
		{"reload_start", func(in *Input) {
			in.ReloadCommon, _ = commonCopy(in.ReloadCommon)
			in.ReloadCommon.Start = []string{"other"}
		}},
		{"unsafe_features", func(in *Input) { in.UnsafeFeatures = []string{"TokenSourceExec"} }},
		{"runtime", func(in *Input) { in.VirtualNetReady = true }},
	}
	for _, tc := range mutations {
		t.Run(tc.name, func(t *testing.T) {
			in := fixture(t, `{"proxies":[{"name":"x","type":"tcp","localPort":80}]}`, nil)
			base := ready(t, in)
			tc.change(&in)
			next, err := Inspect(in)
			if err != nil {
				t.Fatal(err)
			}
			if next.Revision == base.Revision {
				t.Fatal("CAS omitted changed input")
			}
			_, err = Prepare(in, Request{ExpectedRevision: base.Revision, Changes: []Change{{Operation: "disable", Kind: "proxy", Name: "x"}}})
			expectCode(t, err, "revision_conflict")
			if tc.name == "memory" || tc.name == "raw_store" || tc.name == "runtime" {
				if next.ContextRevision != base.ContextRevision {
					t.Fatal("Store changed recovery context")
				}
			} else if next.ContextRevision == base.ContextRevision {
				t.Fatal("external context omitted changed input")
			}
		})
	}
	if err = os.WriteFile(input.StoreFile, []byte("corrupt store"), 0600); err != nil {
		t.Fatal(err)
	}
	input.StoreMemory = Objects{}
	if got, err := InspectContext(input); err != nil || got != context {
		t.Fatal("startup context parsed corrupted Store")
	}
}

func TestStartAndActualVirtualNetAndDisabledValidation(t *testing.T) {
	input := fixture(t, "", map[string]any{"start": []string{"different"}})
	out := prepare(t, input, Change{Operation: "create", Kind: "proxy", Name: "x", Type: "tcp", Fields: fields(map[string]any{"localPort": 80})})
	if len(out.Effective.Proxies) != 0 || out.Changes[0].After.Active || !hasIssue(out.Warnings, "start_filtered") {
		t.Fatal("start filter not reflected in preview")
	}
	broken := fixture(t, `{"proxies":[{"name":"disabled","type":"http","localPort":80,"enabled":false}]}`, nil)
	_, err := Prepare(broken, Request{ExpectedRevision: ready(t, broken).Revision, Changes: []Change{{Operation: "create", Kind: "proxy", Name: "good", Type: "tcp", Fields: fields(map[string]any{"localPort": 80})}}})
	expectCode(t, err, "validation_failed")
	vnet := fixture(t, "", map[string]any{"featureGates": map[string]bool{"VirtualNet": true}, "virtualNet": map[string]any{"address": "100.64.0.1/24"}})
	change := Change{Operation: "create", Kind: "proxy", Name: "x", Type: "tcp", Fields: fields(map[string]any{"localPort": 80})}
	_, err = Prepare(vnet, Request{ExpectedRevision: ready(t, vnet).Revision, Changes: []Change{change}})
	expectCode(t, err, "runtime_capability_required")
	vnet.VirtualNetReady = true
	_ = prepare(t, vnet, change)
}

func TestDependenciesAreReadOnlyWhenNotCompletelyObservable(t *testing.T) {
	marker := filepath.Join(t.TempDir(), "must-not-execute")
	input := fixture(t, "", map[string]any{"auth": map[string]any{"tokenSource": map[string]any{"type": "exec", "exec": map[string]any{"command": "touch", "args": []string{marker}}}}})
	inspection, err := Inspect(input)
	if err != nil {
		t.Fatal(err)
	}
	if inspection.State != "read_only" || !hasIssue(inspection.Issues, "unsupported_dependency") {
		t.Fatal("dynamic source claimed complete CAS")
	}
	if _, err = os.Stat(marker); !os.IsNotExist(err) {
		t.Fatal("inspection executed a value source")
	}
	plugin := fixture(t, `{"proxies":[{"name":"plugin","type":"tcp","plugin":{"type":"static_file","localPath":"."}}]}`, nil)
	inspection, err = Inspect(plugin)
	if err != nil {
		t.Fatal(err)
	}
	if !hasIssue(inspection.Issues, "unsupported_dependency") {
		t.Fatal("plugin directory dependency remained writable")
	}
	plain := fixture(t, `{"proxies":[{"name":"plugin","type":"tcp","plugin":{"type":"socks5","username":"private-user","password":"private-plugin-value"}}]}`, nil)
	inspection = ready(t, plain)
	if inspection.Objects[0].Writable {
		t.Fatal("advanced plugin object remained writable")
	}
	wire, _ := json.Marshal(inspection)
	if strings.Contains(string(wire), "private-plugin-value") || strings.Contains(string(wire), "private-user") {
		t.Fatal("plugin credentials leaked")
	}
}

func TestFileDependencyBytesAndIncludeMembershipChangeCAS(t *testing.T) {
	dir, _ := filepath.EvalSymlinks(t.TempDir())
	token := filepath.Join(dir, "token")
	if err := os.WriteFile(token, []byte("private-token-a"), 0600); err != nil {
		t.Fatal(err)
	}
	input := fixture(t, "", map[string]any{"includes": []string{filepath.Join(dir, "*.json")}, "auth": map[string]any{"tokenSource": map[string]any{"type": "file", "file": map[string]string{"path": token}}}})
	base := ready(t, input)
	if err := os.WriteFile(token, []byte("private-token-b"), 0600); err != nil {
		t.Fatal(err)
	}
	next := ready(t, input)
	if base.ContextRevision == next.ContextRevision {
		t.Fatal("token file bytes omitted from context")
	}
	base = next
	if err := os.WriteFile(filepath.Join(dir, "new.json"), []byte(`{"proxies":[]}`), 0600); err != nil {
		t.Fatal(err)
	}
	next = ready(t, input)
	if base.ContextRevision == next.ContextRevision {
		t.Fatal("include set omitted from context")
	}
	preview, _ := json.Marshal(next)
	if strings.Contains(string(preview), token) || strings.Contains(string(preview), "private-token-") {
		t.Fatal("dependency path or bytes leaked")
	}
}

func TestTemplateEnvironmentTracksOnlyNamedReferences(t *testing.T) {
	for _, expr := range []string{"127.0.0.1", "{{ .Envs.ADDR }}", `{{ index .Envs "ADDR" }}`, "{{ $.Envs.ADDR }}", `{{ index $.Envs "ADDR" }}`, `{{define "host"}}127.0.0.1{{end}}{{template "host"}}`} {
		t.Run(hash([]byte(expr))[:8], func(t *testing.T) {
			input := fixture(t, "", nil)
			data, err := os.ReadFile(input.ConfigFile)
			if err != nil {
				t.Fatal(err)
			}
			data = []byte(strings.Replace(string(data), `"127.0.0.1"`, `"`+expr+`"`, 1))
			if err = os.WriteFile(input.ConfigFile, data, 0600); err != nil {
				t.Fatal(err)
			}
			input.TemplateEnv = map[string]string{"ADDR": "127.0.0.1", "INVOCATION_ID": "old-session"}
			base := ready(t, input)
			input.TemplateEnv["INVOCATION_ID"] = "new-session"
			unrelated := ready(t, input)
			if unrelated.ContextRevision != base.ContextRevision || unrelated.Revision != base.Revision {
				t.Fatal("unused environment changed CAS")
			}
			if !strings.Contains(expr, "ADDR") {
				return
			}
			input.TemplateEnv["ADDR"] = "127.0.0.2"
			changed, err := Inspect(input)
			if err != nil || changed.ContextRevision == base.ContextRevision {
				t.Fatal("referenced environment omitted from CAS")
			}
			_, err = Prepare(input, Request{ExpectedRevision: base.Revision, Changes: []Change{{Operation: "create", Kind: "proxy", Name: "x", Type: "tcp", Fields: fields(map[string]any{"localPort": 80})}}})
			expectCode(t, err, "revision_conflict")
			delete(input.TemplateEnv, "ADDR")
			_, err = InspectContext(input)
			expectCode(t, err, "unsupported_dependency")
		})
	}
}

func TestDynamicTemplateEnvironmentIsReadOnly(t *testing.T) {
	for _, expr := range []string{`{{ $key := "ADDR" }}{{ index .Envs $key }}`, `{{range .Envs}}{{.}}{{end}}`, `{{printf "%v" .Envs}}`, `{{printf "%v" .}}`, `{{with .Envs}}{{.ADDR}}{{end}}`} {
		t.Run(hash([]byte(expr))[:8], func(t *testing.T) {
			input := fixture(t, "", nil)
			data, err := os.ReadFile(input.ConfigFile)
			if err != nil {
				t.Fatal(err)
			}
			data = []byte(strings.Replace(string(data), `"127.0.0.1"`, `"`+expr+`"`, 1))
			if err = os.WriteFile(input.ConfigFile, data, 0600); err != nil {
				t.Fatal(err)
			}
			input.TemplateEnv = map[string]string{"ADDR": "127.0.0.1"}
			out, err := Inspect(input)
			if err != nil || out.State != "read_only" || !hasIssue(out.Issues, "unsupported_dependency") {
				t.Fatal("dynamic environment dependency was accepted")
			}
			_, err = InspectContext(input)
			expectCode(t, err, "unsupported_dependency")
		})
	}
}

func TestCandidateStoreDoesNotChangeRecoveryContext(t *testing.T) {
	for _, absent := range []bool{false, true} {
		t.Run(map[bool]string{false: "existing", true: "missing"}[absent], func(t *testing.T) {
			input := fixture(t, "", nil)
			if absent {
				if err := os.Remove(input.StoreFile); err != nil {
					t.Fatal(err)
				}
			}
			before, _ := os.ReadFile(input.StoreFile)
			base := ready(t, input)
			out := prepare(t, input, Change{Operation: "create", Kind: "proxy", Name: "x", Type: "tcp", Fields: fields(map[string]any{"localPort": 80})})
			if out.OriginalStoreExists == absent || string(out.OriginalStoreBytes) != string(before) {
				t.Fatal("original Store CAS material differs from snapshot")
			}
			if err := os.WriteFile(input.StoreFile, out.StoreBytes, 0600); err != nil {
				t.Fatal(err)
			}
			input.StoreMemory = out.Store
			next := ready(t, input)
			if base.Revision == next.Revision || base.ContextRevision != next.ContextRevision {
				t.Fatal("Store application changed recovery context")
			}
			context, err := InspectContext(input)
			if err != nil || context != base.ContextRevision {
				t.Fatal("startup context differs after Store application")
			}
		})
	}
}
