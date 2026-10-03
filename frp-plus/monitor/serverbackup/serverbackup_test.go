package serverbackup

import (
	"bytes"
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func testWrite(t *testing.T, path string, data []byte) {
	t.Helper()
	if os.MkdirAll(filepath.Dir(path), 0700) != nil || os.WriteFile(path, data, 0600) != nil {
		t.Fatal("fixture write failed")
	}
}
func fixture(t *testing.T, format string, templated bool) (Policy, Policy, map[string]string, map[string]string) {
	t.Helper()
	root, e := filepath.EvalSymlinks(t.TempDir())
	if e != nil {
		t.Fatal(e)
	}
	source, target := filepath.Join(root, "source"), filepath.Join(root, "target")
	os.MkdirAll(source, 0700)
	p := Policy{1, filepath.Join(source, "server."+format), source, []PathGrant{{"install", source}}, []PathGrant{{"ca", filepath.Join(root, "external-source", "ca.pem")}}}
	q := Policy{1, filepath.Join(target, "server."+format), target, []PathGrant{{"install", target}}, []PathGrant{{"ca", filepath.Join(root, "external-target", "ca.pem")}}}
	env := map[string]string{"ROOT": source, "CA": p.Files[0].Path, "TOKEN": "sensitive-template-fixture-value"}
	next := map[string]string{"ROOT": target, "CA": q.Files[0].Path, "TOKEN": env["TOKEN"]}
	prefix, ca, token := source, p.Files[0].Path, env["TOKEN"]
	if templated {
		prefix = "{{ .Envs.ROOT }}"
		ca = "{{ .Envs.CA }}"
		token = "{{ .Envs.TOKEN }}"
	}
	raw := map[string]any{"bindAddr": "127.0.0.1", "bindPort": 7000, "subDomainHost": "domain.invalid", "auth": map[string]any{"token": token}, "transport": map[string]any{"tls": map[string]any{"certFile": prefix + "/tls/cert", "keyFile": prefix + "/tls/key", "trustedCaFile": ca}}, "webServer": map[string]any{"tls": map[string]any{"certFile": prefix + "/tls/cert", "keyFile": prefix + "/tls/key"}}, "custom404Page": prefix + "/404.html", "sshTunnelGateway": map[string]any{"bindPort": 7022, "autoGenPrivateKeyPath": prefix + "/ssh/key", "authorizedKeysFile": prefix + "/ssh/authorized"}, "monitor": map[string]any{"enabled": true, "bindAddr": "127.0.0.1", "bindPort": 7401, "serverID": "persistent-server", "databaseFile": prefix + "/control.sqlite", "historyDataPath": prefix + "/history", "certFile": prefix + "/tls/cert", "keyFile": prefix + "/tls/key", "githubClientID": "test-client", "githubClientSecretFile": prefix + "/github.secret", "githubCallbackURL": "https://admin.invalid/api/admin/v1/auth/github/callback", "githubAdminUsers": []any{"test-admin"}}, "log": map[string]any{"to": prefix + "/log/server.log"}}
	// Normalize numeric types to the encoder's JSON domain.
	b, _ := json.Marshal(raw)
	d := json.NewDecoder(bytes.NewReader(b))
	d.UseNumber()
	d.Decode(&raw)
	encoded, e := relocationEncode(p.ConfigFile, raw)
	if e != nil {
		t.Fatal(e)
	}
	testWrite(t, p.ConfigFile, encoded)
	for _, name := range []string{"tls/cert", "tls/key", "404.html", "ssh/key", "ssh/authorized", "github.secret"} {
		testWrite(t, filepath.Join(source, name), []byte("private fixture "+name))
	}
	testWrite(t, p.Files[0].Path, []byte("private external CA"))
	return p, q, env, next
}
func TestCaptureTransformSealedAllFormats(t *testing.T) {
	for _, format := range []string{"toml", "json", "yaml"} {
		for _, templates := range []bool{false, true} {
			t.Run(format+map[bool]string{false: "-literal", true: "-template"}[templates], func(t *testing.T) {
				p, q, env, next := fixture(t, format, templates)
				ctx := context.Background()
				s, e := Capture(ctx, p, env, nil)
				if e != nil {
					t.Fatal(e)
				}
				again, e := Capture(ctx, p, env, nil)
				if e != nil || again.Summary() != s.Summary() {
					t.Fatal("capture is not deterministic")
				}
				if len(s.Manifest.Files) != 8 || s.Manifest.DatabaseFile != filepath.Join(p.WorkingDir, "control.sqlite") || !s.Manifest.HistoryEnabled {
					t.Fatal("closure incomplete")
				}
				encoded, _ := json.Marshal(s.Manifest)
				if bytes.Contains(encoded, []byte(env["TOKEN"])) {
					t.Fatal("environment value leaked to manifest")
				}
				output := filepath.Join(filepath.Dir(p.WorkingDir), "snapshot")
				summary, e := WriteSnapshot(output, s)
				if e != nil {
					t.Fatal(e)
				}
				sealed, e := ReadSnapshot(ctx, output, summary.ManifestDigest, p, env)
				if e != nil {
					t.Fatal(e)
				}
				os.RemoveAll(p.WorkingDir)
				os.RemoveAll(filepath.Dir(p.Files[0].Path))
				result, e := Transform(ctx, sealed, q, env, next, nil)
				if e != nil {
					t.Fatal(e)
				}
				if _, e = os.Stat(q.WorkingDir); !os.IsNotExist(e) {
					t.Fatal("pure transformation read or wrote target")
				}
				if result.Manifest.DatabaseFile != filepath.Join(q.WorkingDir, "control.sqlite") || result.Policy.Files[0].Path != q.Files[0].Path {
					t.Fatal("paths not mapped")
				}
				if templates && !bytes.Equal(result.payload[q.ConfigFile].data, s.payload[p.ConfigFile].data) {
					t.Fatal("template bytes changed unnecessarily")
				}
				if templates {
					for _, m := range result.payload {
						if bytes.Contains(m.data, []byte(env["TOKEN"])) {
							t.Fatal("template value materialized")
						}
					}
				}
				targetOutput := filepath.Join(filepath.Dir(p.WorkingDir), "target-snapshot")
				sum, e := WriteSnapshot(targetOutput, result)
				if e != nil {
					t.Fatal(e)
				}
				if _, e = ReadSnapshot(ctx, targetOutput, sum.ManifestDigest, q, next); e != nil {
					t.Fatal(e)
				}
				if _, e = ReadSnapshot(ctx, targetOutput, sum.ManifestDigest, q, map[string]string{"ROOT": q.WorkingDir, "CA": q.Files[0].Path, "TOKEN": "changed"}); templates && e == nil {
					t.Fatal("changed environment accepted")
				}
			})
		}
	}
}
func TestRelocationRejectsSemanticChanges(t *testing.T) {
	p, q, env, next := fixture(t, "toml", true)
	s, e := Capture(context.Background(), p, env, nil)
	if e != nil {
		t.Fatal(e)
	}
	next["TOKEN"] = "different-secret"
	if _, e = Transform(context.Background(), s, q, env, next, nil); e == nil || !strings.Contains(e.Error(), "semantics_changed") {
		t.Fatal("secret semantics changed")
	}
	next["TOKEN"] = env["TOKEN"]
	raw := s.payload[p.ConfigFile]
	raw.data = append(raw.data, []byte("\n# {{ index .Envs .Envs.TOKEN }}\n")...)
	testWrite(t, p.ConfigFile, raw.data)
	if _, e = Capture(context.Background(), p, env, nil); e == nil {
		t.Fatal("dynamic template accepted")
	}
}
func TestStrictFormatsAndUnsupportedDependencies(t *testing.T) {
	cases := []struct{ name, format, extra string }{{"yaml-disguised", "toml", ".ignored: hidden\nmonitor:\n  enabled: true\n"}, {"case-alias", "json", `{"Monitor":{"enabled":true},"monitor":{"enabled":true}}`}, {"duplicate", "json", `{"bindPort":1,"bindPort":2}`}, {"yaml-alias", "yaml", "monitor: &x {enabled: true}\nauth: *x\n"}}
	for _, tt := range cases {
		t.Run(tt.name, func(t *testing.T) {
			p, _, env, _ := fixture(t, tt.format, false)
			testWrite(t, p.ConfigFile, []byte(tt.extra))
			if _, e := Capture(context.Background(), p, env, nil); e == nil {
				t.Fatal("invalid source accepted")
			}
		})
	}
	for _, what := range []string{"assets", "exec", "missing-ssh", "symlink", "hardlink", "outside-root", "source-env"} {
		t.Run(what, func(t *testing.T) {
			p, _, env, _ := fixture(t, "json", false)
			data, _ := os.ReadFile(p.ConfigFile)
			var raw map[string]any
			json.Unmarshal(data, &raw)
			switch what {
			case "assets":
				raw["webServer"].(map[string]any)["assetsDir"] = "assets"
			case "exec":
				raw["auth"] = map[string]any{"tokenSource": map[string]any{"type": "exec", "exec": map[string]any{"command": "true"}}}
			case "missing-ssh":
				os.Remove(filepath.Join(p.WorkingDir, "ssh/key"))
			case "symlink":
				os.Remove(filepath.Join(p.WorkingDir, "github.secret"))
				os.Symlink(p.Files[0].Path, filepath.Join(p.WorkingDir, "github.secret"))
			case "hardlink":
				os.Remove(filepath.Join(p.WorkingDir, "github.secret"))
				os.Link(p.Files[0].Path, filepath.Join(p.WorkingDir, "github.secret"))
			case "outside-root":
				raw["custom404Page"] = "/outside-authorization.html"
			}
			data, _ = json.Marshal(raw)
			testWrite(t, p.ConfigFile, data)
			forbidden := []string{}
			if what == "source-env" {
				forbidden = append(forbidden, filepath.Join(p.WorkingDir, "github.secret"))
			}
			if _, e := Capture(context.Background(), p, env, forbidden); e == nil {
				t.Fatal("unsafe closure accepted")
			}
		})
	}
}
func TestPoliciesAndSealedClosureFailClosed(t *testing.T) {
	p, q, env, next := fixture(t, "json", true)
	ctx := context.Background()
	s, e := Capture(ctx, p, env, nil)
	if e != nil {
		t.Fatal(e)
	}
	root := filepath.Dir(p.WorkingDir)
	out := filepath.Join(root, "sealed")
	sum, e := WriteSnapshot(out, s)
	if e != nil {
		t.Fatal(e)
	}
	// A self-consistent manifest cannot omit a required certificate.
	missing := s.Manifest
	missing.Files = append([]File{}, missing.Files[:len(missing.Files)-1]...)
	raw, _ := json.Marshal(missing)
	testWrite(t, filepath.Join(out, "CONTEXT.json"), raw)
	if _, e = ReadSnapshot(ctx, out, hash(raw), p, env); e == nil {
		t.Fatal("incomplete sealed closure accepted")
	}
	raw, _ = json.Marshal(s.Manifest)
	testWrite(t, filepath.Join(out, "CONTEXT.json"), raw)
	testWrite(t, filepath.Join(out, "extra"), []byte("extra"))
	if _, e = ReadSnapshot(ctx, out, sum.ManifestDigest, p, env); e == nil {
		t.Fatal("extra sealed file accepted")
	}
	os.Remove(filepath.Join(out, "extra"))
	bad := q
	bad.Files = []PathGrant{{"ca", filepath.Join(q.WorkingDir, "tls/ca")}}
	if _, e = Transform(ctx, s, bad, env, next, nil); e == nil {
		t.Fatal("root/exact overlap accepted")
	}
	bad = q
	bad.Roots = []PathGrant{{"install", p.WorkingDir + "/nested"}}
	bad.WorkingDir = bad.Roots[0].Path
	bad.ConfigFile = filepath.Join(bad.WorkingDir, "server.json")
	if _, e = Transform(ctx, s, bad, env, next, nil); e == nil {
		t.Fatal("source/target overlap accepted")
	}
	for _, raw := range []string{`{"version":1,"Version":1}`, `{"version":1,"config_file":"/x","working_dir":"/x","roots":[],"files":null}`} {
		if _, e = DecodePolicy([]byte(raw)); e == nil {
			t.Fatal("invalid policy accepted")
		}
	}
}
func TestTokenFileAndCancelledCapture(t *testing.T) {
	p, _, env, _ := fixture(t, "json", false)
	raw, _ := os.ReadFile(p.ConfigFile)
	var value map[string]any
	json.Unmarshal(raw, &value)
	value["auth"] = map[string]any{"tokenSource": map[string]any{"type": "file", "file": map[string]any{"path": "frp.token"}}}
	raw, _ = json.Marshal(value)
	testWrite(t, p.ConfigFile, raw)
	testWrite(t, filepath.Join(p.WorkingDir, "frp.token"), []byte("literal private token"))
	s, e := Capture(context.Background(), p, env, nil)
	if e != nil || s.payload[filepath.Join(p.WorkingDir, "frp.token")].data == nil {
		t.Fatal("file token dependency missing")
	}
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	if _, e = Capture(ctx, p, env, nil); e == nil {
		t.Fatal("cancel ignored")
	}
}

func TestStaticLiteralIndexAndBusinessIdentity(t *testing.T) {
	p, q, env, next := fixture(t, "toml", true)
	raw, _ := os.ReadFile(p.ConfigFile)
	raw = bytes.ReplaceAll(raw, []byte("{{ .Envs.TOKEN }}"), []byte(`{{ index .Envs "TOKEN" }}`))
	testWrite(t, p.ConfigFile, raw)
	s, e := Capture(context.Background(), p, env, nil)
	if e != nil {
		t.Fatal(e)
	}
	if _, e = Transform(context.Background(), s, q, env, next, nil); e != nil {
		t.Fatal(e)
	}
	// The same ROOT variable in a path and a domain is not a path-only change.
	raw = bytes.ReplaceAll(raw, []byte("domain.invalid"), []byte("{{ .Envs.ROOT }}"))
	testWrite(t, p.ConfigFile, raw)
	s, e = Capture(context.Background(), p, env, nil)
	if e != nil {
		t.Fatal(e)
	}
	if _, e = Transform(context.Background(), s, q, env, next, nil); e == nil {
		t.Fatal("root variable changed domain identity")
	}
}
func TestReservedProofsAndEnvironmentInputs(t *testing.T) {
	for _, suffix := range []string{".restore-gate.json", ".restore-completed-012345.json", "-wal", "-shm", "-journal"} {
		t.Run(suffix, func(t *testing.T) {
			p, _, env, _ := fixture(t, "json", false)
			raw, _ := os.ReadFile(p.ConfigFile)
			var v map[string]any
			json.Unmarshal(raw, &v)
			v["custom404Page"] = filepath.Join(p.WorkingDir, "control.sqlite") + suffix
			raw, _ = json.Marshal(v)
			testWrite(t, p.ConfigFile, raw)
			if _, e := Capture(context.Background(), p, env, nil); e == nil {
				t.Fatal("restore/database sidecar captured as ordinary dependency")
			}
		})
	}
	p, q, env, next := fixture(t, "json", true)
	ctx := context.Background()
	root := filepath.Dir(p.WorkingDir)
	policyFile := filepath.Join(root, "source-policy.json")
	envFile := filepath.Join(root, "source-env.json")
	targetPolicy := filepath.Join(root, "target-policy.json")
	targetEnv := filepath.Join(root, "target-env.json")
	testWrite(t, policyFile, policyBytes(p))
	testWrite(t, targetPolicy, policyBytes(q))
	raw, _ := json.Marshal(env)
	testWrite(t, envFile, raw)
	raw, _ = json.Marshal(next)
	testWrite(t, targetEnv, raw)
	capture := CLIRequest{Action: "capture", ConfigFile: p.ConfigFile, PolicyFile: policyFile, EnvFile: envFile, Output: filepath.Join(root, "capture"), Offline: true}
	summary, e := RunCLI(ctx, capture)
	if e != nil {
		t.Fatal(e)
	}
	transform := CLIRequest{Action: "transform", PolicyFile: targetPolicy, EnvFile: targetEnv, SourcePolicyFile: policyFile, SourceEnvFile: envFile, Checkpoint: capture.Output, ManifestDigest: summary.ManifestDigest, Output: filepath.Join(root, "transformed"), Offline: true}
	if _, e = RunCLI(ctx, transform); e != nil {
		t.Fatal(e)
	}
	// Explicit environment must be private, and may not live in a published root.
	os.Chmod(envFile, 0644)
	capture.Output = filepath.Join(root, "bad-permissions")
	if _, e = RunCLI(ctx, capture); e == nil {
		t.Fatal("public environment accepted")
	}
	os.Chmod(envFile, 0600)
	raw, _ = json.Marshal(next)
	transform.EnvFile = filepath.Join(q.WorkingDir, "env.json")
	testWrite(t, transform.EnvFile, raw)
	transform.Output = filepath.Join(root, "bad-target-env")
	if _, e = RunCLI(ctx, transform); e == nil {
		t.Fatal("environment could be overwritten during restore")
	}
}

func TestExternalDataPathsRemainMetadataOnly(t *testing.T) {
	p, q, env, next := fixture(t, "json", true)
	root := filepath.Dir(p.WorkingDir)
	p.Roots = append(p.Roots, PathGrant{"history", filepath.Join(root, "source-tsdb")})
	q.Roots = append(q.Roots, PathGrant{"history", filepath.Join(root, "target-tsdb")})
	p.Files = append(p.Files, PathGrant{"database", filepath.Join(root, "source-db", "database.sqlite")})
	q.Files = append(q.Files, PathGrant{"database", filepath.Join(root, "target-db", "database.sqlite")})
	env["DB"] = p.Files[1].Path
	next["DB"] = q.Files[1].Path
	env["HISTORY"] = p.Roots[1].Path
	next["HISTORY"] = q.Roots[1].Path
	data, _ := os.ReadFile(p.ConfigFile)
	var raw map[string]any
	json.Unmarshal(data, &raw)
	monitor := raw["monitor"].(map[string]any)
	monitor["databaseFile"] = "{{ .Envs.DB }}"
	monitor["historyDataPath"] = "{{ .Envs.HISTORY }}"
	data, _ = json.Marshal(raw)
	testWrite(t, p.ConfigFile, data)
	// Neither path exists. Go must not touch/open either storage engine.
	s, e := Capture(context.Background(), p, env, nil)
	if e != nil {
		t.Fatal(e)
	}
	target, e := Transform(context.Background(), s, q, env, next, nil)
	if e != nil {
		t.Fatal(e)
	}
	if s.Manifest.DatabaseFile != env["DB"] || target.Manifest.DatabaseFile != next["DB"] || target.Manifest.HistoryPath != next["HISTORY"] {
		t.Fatal("external data metadata was not mapped")
	}
	for _, f := range target.Manifest.Files {
		if f.Path == next["DB"] || within(f.Path, next["HISTORY"]) {
			t.Fatal("database/TSDB included as ordinary material")
		}
	}
	t.Setenv("TOKEN", env["TOKEN"])
	delete(env, "TOKEN")
	if _, e = Capture(context.Background(), p, env, nil); e == nil {
		t.Fatal("ambient environment used")
	}
}

func TestInvalidUTF8AndSealedDirectoryMode(t *testing.T) {
	if _, e := DecodePolicy([]byte{'{', '"', 'v', 'e', 'r', 's', 'i', 'o', 'n', '"', ':', '1', ',', '"', 0xff, '"', ':', '1', '}'}); e == nil {
		t.Fatal("non-UTF8 policy accepted")
	}
	p, _, env, _ := fixture(t, "toml", false)
	snapshot, e := Capture(context.Background(), p, env, nil)
	if e != nil {
		t.Fatal(e)
	}
	out := filepath.Join(filepath.Dir(p.WorkingDir), "snapshot-mode")
	sum, e := WriteSnapshot(out, snapshot)
	if e != nil {
		t.Fatal(e)
	}
	os.Chmod(filepath.Join(out, "context"), 0755)
	if _, e = ReadSnapshot(context.Background(), out, sum.ManifestDigest, p, env); e == nil {
		t.Fatal("non-private sealed directory accepted")
	}
}
