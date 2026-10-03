//go:build linux || darwin

package managed

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func secretTestEngine(t *testing.T) (*Engine, Options) {
	t.Helper()
	parent, err := filepath.EvalSymlinks(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	root := filepath.Join(parent, "managed")
	options := Options{Root: root, StorePath: filepath.Join(root, "store.json")}
	e, err := Open(options)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = e.Close() })
	return e, options
}

func TestLocalSecretsImmutableAndPrivate(t *testing.T) {
	e, options := secretTestEngine(t)
	ctx := context.Background()
	reference := "12345678-1234-4234-8234-123456789abc"
	value := "synthetic-secret-not-for-audit"
	if err := e.PutSecret(ctx, reference, value); err != nil {
		t.Fatal(err)
	}
	if err := e.PutSecret(ctx, reference, value); err != nil {
		t.Fatal("retry", err)
	}
	if err := e.PutSecret(ctx, reference, "replacement"); !errors.Is(err, ErrConflict) {
		t.Fatal("reference was mutable", err)
	}
	resolved, err := e.ResolveSecrets(ctx, []string{reference, reference})
	if err != nil || len(resolved) != 1 || resolved[reference] != value {
		t.Fatal("resolution failed", err)
	}
	for path, mode := range map[string]os.FileMode{filepath.Join(options.Root, "secrets"): 0700, filepath.Join(options.Root, "secrets", reference+".secret"): 0600} {
		info, err := os.Stat(path)
		if err != nil || info.Mode().Perm() != mode {
			t.Fatal("unexpected secret permissions", err)
		}
	}
	if err := e.Close(); err != nil {
		t.Fatal(err)
	}
	reopened, err := Open(options)
	if err != nil {
		t.Fatal(err)
	}
	defer reopened.Close()
	resolved, err = reopened.ResolveSecrets(ctx, []string{reference})
	if err != nil || resolved[reference] != value {
		t.Fatal("secret did not survive reopen", err)
	}
}

func TestLocalSecretsRejectPathsAndSymlinks(t *testing.T) {
	e, options := secretTestEngine(t)
	ctx := context.Background()
	reference := "12345678-1234-4234-8234-123456789abc"
	for _, id := range []string{"../outside", reference + "/other", "not-a-reference"} {
		if e.PutSecret(ctx, id, "secret") == nil {
			t.Fatal("accepted secret path")
		}
		if _, err := e.ResolveSecrets(ctx, []string{id}); err == nil {
			t.Fatal("accepted resolution path")
		}
	}
	for _, value := range []string{"", "x\x00y", "x\ny", strings.Repeat("x", MaxSecretBytes+1), string([]byte{0xff})} {
		if e.PutSecret(ctx, reference, value) == nil {
			t.Fatal("accepted invalid secret value")
		}
	}
	if _, err := e.ResolveSecrets(ctx, []string{reference}); !errors.Is(err, ErrNotFound) {
		t.Fatal("missing reference", err)
	}
	outside := filepath.Join(filepath.Dir(options.Root), "outside")
	if err := os.WriteFile(outside, []byte("private-other-file"), 0600); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(outside, filepath.Join(options.Root, "secrets", reference+".secret")); err != nil {
		t.Fatal(err)
	}
	if e.PutSecret(ctx, reference, "secret") == nil {
		t.Fatal("followed symlink during write")
	}
	if _, err := e.ResolveSecrets(ctx, []string{reference}); err == nil {
		t.Fatal("followed symlink during read")
	}
	data, err := os.ReadFile(outside)
	if err != nil || string(data) != "private-other-file" {
		t.Fatal("outside file changed")
	}
	cancelled, cancel := context.WithCancel(ctx)
	cancel()
	if e.PutSecret(cancelled, reference, "secret") == nil {
		t.Fatal("accepted cancelled request")
	}
}
