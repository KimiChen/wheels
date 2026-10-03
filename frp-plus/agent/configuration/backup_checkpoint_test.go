//go:build linux || darwin

package configuration

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"testing"

	"github.com/fatedier/frp/extension/frpmonitor/agent/backupmanifest"
	"github.com/fatedier/frp/extension/frpmonitor/agent/managed"
	nativeconfig "github.com/fatedier/frp/pkg/config"
)

func checkpointFixtureV2(t *testing.T) (string, managed.Options, []managed.StoreVariant) {
	t.Helper()
	file, options := maintenanceFixture(t)
	parent := filepath.Dir(file)
	for _, dir := range []string{"includes", "tls", "plugin"} {
		if err := os.Mkdir(filepath.Join(parent, dir), 0700); err != nil {
			t.Fatal(err)
		}
	}
	for _, name := range []string{"tls/ca.crt", "tls/client.crt", "tls/client.key", "plugin/site.crt", "plugin/site.key", "plugin/historical.crt", "plugin/historical.key"} {
		backupWrite(t, filepath.Join(parent, name), []byte("synthetic-private-"+name))
	}
	data, err := os.ReadFile(file)
	if err != nil {
		t.Fatal(err)
	}
	// Main scalar fields must precede the explicit TLS table.
	data = append([]byte("includes = ['includes/*.toml']\n"), data...)
	data = append(data, []byte("\n[transport.tls]\ncertFile = 'tls/client.crt'\nkeyFile = 'tls/client.key'\ntrustedCaFile = 'tls/ca.crt'\n")...)
	backupWrite(t, file, data)
	backupWrite(t, filepath.Join(parent, "includes", "site.toml"), []byte("[[proxies]]\nname='site'\ntype='tcp'\nremotePort=18001\n[proxies.plugin]\ntype='https2http'\nlocalAddr='127.0.0.1:18002'\ncrtPath='plugin/site.crt'\nkeyPath='plugin/site.key'\n"))
	variants := []managed.StoreVariant{{Path: "managed/store.json", Snapshot: managed.StoreSnapshot{}}}
	return file, options, variants
}
func TestCheckpointV2SealedContextMatchesNativeIncludes(t *testing.T) {
	file, options, variants := checkpointFixtureV2(t)
	t.Chdir(filepath.Dir(file))
	ctx := context.Background()
	captured, err := CheckpointCaptureV2(file)(ctx, variants)
	if err != nil {
		t.Fatal(err)
	}
	loaded, err := nativeconfig.LoadClientConfigResult(file, true)
	if err != nil {
		t.Fatal(err)
	}
	revision, err := InspectContext(Input{ConfigFile: file, WorkingDir: filepath.Dir(file), StoreFile: options.StorePath, StartupCommon: loaded.Common, ReloadCommon: loaded.Common, FileMemory: Objects{Proxies: loaded.Proxies, Visitors: loaded.Visitors}, TemplateEnv: map[string]string{}})
	if err != nil || revision != captured.Revision {
		t.Fatal("native context mismatch", err)
	}
	m := managed.CheckpointManifestV2{CheckpointManifest: managed.CheckpointManifest{Root: options.Root, ConfigFile: file, WorkingDir: filepath.Dir(file), ContextRevision: revision}, GraphSHA256: backupmanifest.Digest(captured.GraphBytes)}
	if err = os.RemoveAll(filepath.Dir(file)); err != nil {
		t.Fatal(err)
	}
	if err = ValidateCheckpointContextV2(ctx, m, captured.Files, variants, captured.GraphBytes); err != nil {
		t.Fatal("sealed validation read disk", err)
	}
}
func TestCheckpointV2HistoricalCandidateCertificateClosure(t *testing.T) {
	file, options, variants := checkpointFixtureV2(t)
	ctx := context.Background()
	candidate := []byte(`{"proxies":[{"name":"history","type":"tcp","plugin":{"type":"https2http","crtPath":"plugin/historical.crt","keyPath":"plugin/historical.key"}}],"visitors":[]}`)
	variants = append(variants, managed.StoreVariant{Path: "managed/operations/test.old", Snapshot: managed.StoreSnapshot{}}, managed.StoreVariant{Path: "managed/operations/test.new", Snapshot: managed.StoreSnapshot{Exists: true, Bytes: candidate}})
	captured, err := CheckpointCaptureV2(file)(ctx, variants)
	if err != nil {
		t.Fatal(err)
	}
	found := 0
	for _, source := range captured.Files {
		if filepath.Base(source.Path) == "historical.crt" || filepath.Base(source.Path) == "historical.key" {
			found++
		}
	}
	if found != 2 {
		t.Fatal("candidate-only files omitted")
	}
	m := managed.CheckpointManifestV2{CheckpointManifest: managed.CheckpointManifest{Root: options.Root, ConfigFile: file, WorkingDir: filepath.Dir(file), ContextRevision: captured.Revision}, GraphSHA256: backupmanifest.Digest(captured.GraphBytes)}
	for i, source := range captured.Files {
		if filepath.Base(source.Path) == "historical.crt" {
			captured.Files = append(captured.Files[:i], captured.Files[i+1:]...)
			break
		}
	}
	if err = ValidateCheckpointContextV2(ctx, m, captured.Files, variants, captured.GraphBytes); err == nil {
		t.Fatal("self-consistent omitted historical dependency")
	}
}
func TestCheckpointV2ExportInstallIncludesAndPrivateFiles(t *testing.T) {
	file, options, _ := checkpointFixtureV2(t)
	ctx := context.Background()
	capture := CheckpointCaptureV2(file)
	parent, err := filepath.EvalSymlinks(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	if err = os.Chmod(parent, 0700); err != nil {
		t.Fatal(err)
	}
	out := filepath.Join(parent, "checkpoint")
	summary, err := managed.ExportCheckpointV2(ctx, options, capture, out)
	if err != nil {
		t.Fatal("export", err)
	}
	if _, err = managed.ValidateCheckpointV2(ctx, out, summary.ManifestDigest, ValidateCheckpointContextV2); err != nil {
		t.Fatal("validate", err)
	}
	key := filepath.Join(filepath.Dir(file), "plugin", "site.key")
	before, _ := os.Stat(key)
	expected, _ := os.ReadFile(key)
	backupWrite(t, key, []byte("broken-key"))
	if err = os.Remove(filepath.Join(filepath.Dir(file), "tls", "ca.crt")); err != nil {
		t.Fatal(err)
	}
	installed, err := managed.InstallCheckpointV2(ctx, out, options.Root, summary.ManifestDigest, capture, ValidateCheckpointContextV2)
	if err != nil {
		t.Fatal("install", err)
	}
	if installed.State != "pending" {
		t.Fatal("wrong state")
	}
	after, _ := os.Stat(key)
	actual, _ := os.ReadFile(key)
	if string(actual) != string(expected) || !before.ModTime().Equal(after.ModTime()) {
		t.Fatal("certificate bytes/mtime not restored")
	}
	if _, err = managed.ConfirmRestorationV2(ctx, options, installed.Epoch, summary.ManifestDigest, capture); err == nil {
		t.Fatal("confirmation skipped Admin")
	}
	// A new matching include is an unknown source, never silently deleted.
	extra := filepath.Join(filepath.Dir(file), "includes", "foreign.toml")
	backupWrite(t, extra, []byte("[[proxies]]\nname='foreign'\ntype='tcp'\nremotePort=18101\n"))
	if _, err = managed.InstallCheckpointV2(ctx, out, options.Root, summary.ManifestDigest, capture, ValidateCheckpointContextV2); err == nil {
		t.Fatal(fmt.Sprintf("pending retry accepted changed includes: %s", installed.State))
	}
}
