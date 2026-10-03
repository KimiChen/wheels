package managed

import (
	"bytes"
	"context"
	"strings"
	"unicode/utf8"
)

const (
	MaxSecretBytes  = 1024
	MaxLocalSecrets = 1024
)

// PutSecret stores an immutable opaque reference in the private local vault.
// A repeated request is idempotent only when the bytes match. The caller must
// authorize the write before invoking this method and must never log value.
// Native FRP still stores resolved values in its private Store/snapshots; this
// reference layer does not claim to encrypt the native configuration.
func (e *Engine) PutSecret(ctx context.Context, reference, value string) error {
	if !serviceIdentity.MatchString(reference) || !validSecretValue(value) {
		return ErrInvalid
	}
	if err := ctx.Err(); err != nil {
		return err
	}
	e.mu.Lock()
	defer e.mu.Unlock()
	if e.closing {
		return ErrClosed
	}
	if e.restoreBlocked() {
		return ErrRecovery
	}
	dir, err := e.root.subdir("secrets")
	if err != nil {
		return err
	}
	defer dir.close()
	name := reference + ".secret"
	old, err := dir.read(name, MaxSecretBytes)
	if err != nil {
		return err
	}
	if old.Exists {
		if bytes.Equal(old.Bytes, []byte(value)) {
			return nil
		}
		return ErrConflict
	}
	names, err := dir.names()
	if err != nil {
		return err
	}
	if len(names) >= MaxLocalSecrets {
		return ErrStorage
	}
	if err := ctx.Err(); err != nil {
		return err
	}
	return dir.replace(name, StoreSnapshot{Exists: true, Bytes: []byte(value)}, Digest(old), MaxSecretBytes, nil)
}

// ResolveSecrets is a process-local adapter API, never a management GET. It
// reads only the requested opaque references and never accepts a file path.
func (e *Engine) ResolveSecrets(ctx context.Context, references []string) (map[string]string, error) {
	if len(references) > MaxLocalSecrets {
		return nil, ErrInvalid
	}
	for _, reference := range references {
		if !serviceIdentity.MatchString(reference) {
			return nil, ErrInvalid
		}
	}
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	e.mu.Lock()
	defer e.mu.Unlock()
	if e.closing {
		return nil, ErrClosed
	}
	result := make(map[string]string, len(references))
	if len(references) == 0 {
		return result, nil
	}
	dir, err := e.root.subdir("secrets")
	if err != nil {
		return nil, err
	}
	defer dir.close()
	for _, reference := range references {
		if err := ctx.Err(); err != nil {
			return nil, err
		}
		file, err := dir.read(reference+".secret", MaxSecretBytes)
		if err != nil {
			return nil, err
		}
		if !file.Exists {
			return nil, ErrNotFound
		}
		value := string(file.Bytes)
		if len(value) == 0 || !utf8.ValidString(value) || strings.ContainsAny(value, "\x00\r\n") {
			return nil, ErrRecovery
		}
		result[reference] = value
	}
	return result, nil
}

func validSecretValue(value string) bool {
	return len(value) > 0 && len(value) <= MaxSecretBytes && utf8.ValidString(value) && !strings.ContainsAny(value, "\x00\r\n")
}
