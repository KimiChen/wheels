package monitor

import (
	"bytes"
	"encoding/json"
	"errors"
	"io"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"time"
	"unicode/utf8"
)

// Strict decoding also rejects duplicate and case-aliased keys, unlike encoding/json alone.
func strictJSON(data []byte, target any) error {
	if !utf8.Valid(data) {
		return errors.New("invalid JSON")
	}
	d := json.NewDecoder(bytes.NewReader(data))
	d.UseNumber()
	var walk func(reflect.Type) error
	walk = func(t reflect.Type) error {
		tok, err := d.Token()
		if err != nil {
			return err
		}
		if t.Kind() == reflect.Pointer {
			if tok == nil {
				return nil
			}
			t = t.Elem()
		}
		if tok == nil {
			return errors.New("null field")
		}
		switch t.Kind() {
		case reflect.Struct:
			if tok != json.Delim('{') {
				return errors.New("expected object")
			}
			fields := map[string]reflect.Type{}
			for i := 0; i < t.NumField(); i++ {
				f := t.Field(i)
				key := strings.Split(f.Tag.Get("json"), ",")[0]
				if key != "-" {
					if key == "" {
						key = f.Name
					}
					fields[key] = f.Type
				}
			}
			seen := map[string]bool{}
			for d.More() {
				key, err := d.Token()
				if err != nil {
					return err
				}
				name, ok := key.(string)
				ft, known := fields[name]
				if !ok || !known || seen[name] {
					return errors.New("unknown or duplicate field")
				}
				seen[name] = true
				if err := walk(ft); err != nil {
					return err
				}
			}
			_, err = d.Token()
			return err
		case reflect.Slice:
			if tok != json.Delim('[') {
				return errors.New("expected array")
			}
			for d.More() {
				if err := walk(t.Elem()); err != nil {
					return err
				}
			}
			_, err = d.Token()
			return err
		default:
			if _, ok := tok.(json.Delim); ok {
				return errors.New("unexpected structure")
			}
			return nil
		}
	}
	typ := reflect.TypeOf(target)
	if typ == nil || typ.Kind() != reflect.Pointer {
		return errors.New("invalid decode target")
	}
	if err := walk(typ.Elem()); err != nil {
		return err
	}
	if _, err := d.Token(); err != io.EOF {
		return errors.New("trailing JSON")
	}
	d = json.NewDecoder(bytes.NewReader(data))
	d.DisallowUnknownFields()
	return d.Decode(target)
}

func readPrivate(path string) ([]byte, error) {
	info, err := os.Lstat(path)
	if err != nil || !info.Mode().IsRegular() || info.Mode().Perm() != 0600 || info.Size() > 1024*1024 {
		return nil, errors.New("private file unavailable")
	}
	f, err := os.Open(path)
	if err != nil {
		return nil, errors.New("private file unavailable")
	}
	defer f.Close()
	actual, err := f.Stat()
	if err != nil || !os.SameFile(info, actual) || actual.Mode().Perm() != 0600 {
		return nil, errors.New("private file changed")
	}
	data, err := io.ReadAll(io.LimitReader(f, 1024*1024+1))
	if err != nil || len(data) > 1024*1024 {
		return nil, errors.New("private file too large")
	}
	return data, nil
}

// Write a complete 0600 replacement on the same filesystem, then fsync its parent.
// Existing files must remain regular private files; symlink replacement is refused.
func writePrivate(path string, value any) error {
	data, err := json.MarshalIndent(value, "", "  ")
	if err != nil {
		return err
	}
	data = append(data, '\n')
	if len(data) > 1024*1024 {
		return errors.New("configuration too large")
	}
	info, err := os.Lstat(path)
	if err != nil || !info.Mode().IsRegular() || info.Mode().Perm() != 0600 {
		return errors.New("private file unavailable")
	}
	parent := filepath.Dir(path)
	f, err := os.CreateTemp(parent, ".monitor-config-*")
	if err != nil {
		return err
	}
	name := f.Name()
	defer os.Remove(name)
	if err = f.Chmod(0600); err == nil {
		_, err = f.Write(data)
	}
	if err == nil {
		err = f.Sync()
	}
	closeErr := f.Close()
	if err == nil {
		err = closeErr
	}
	if err != nil {
		return err
	}
	current, err := os.Lstat(path)
	if err != nil || !os.SameFile(info, current) {
		return errors.New("configuration changed concurrently")
	}
	if err = os.Rename(name, path); err != nil {
		return err
	}
	dir, err := os.Open(parent)
	if err != nil {
		return err
	}
	defer dir.Close()
	return dir.Sync()
}

// Caller holds configMu. An unchanged credential preserves the latest snapshot;
// token rotation clears it and closes every socket, including pending hellos.
func (s *Service) applyCredentials(creds []credential) {
	s.mu.Lock()
	next := make(map[string]*node, len(creds))
	grants := make(map[string]string, len(creds))
	for _, c := range creds {
		grants[c.AgentID] = c.TokenSHA256
		n := s.nodes[c.AgentID]
		if n == nil || n.credential.TokenSHA256 != c.TokenSHA256 {
			n = &node{}
		}
		n.credential = c
		next[c.AgentID] = n
	}
	for conn, grant := range s.connections {
		if grants[grant.AgentID] != grant.TokenSHA256 {
			_ = conn.Close()
		}
	}
	s.nodes = next
	s.credentials = creds
	s.mu.Unlock()
	s.credentialError.Store(false)
	// Remove stale cached public rows immediately when authorization is revoked.
	s.publishSnapshot(time.Now())
}
func (s *Service) reloadCredentials() {
	creds, err := readCredentials(s.cfg.CredentialsFile)
	if err != nil {
		s.credentialError.Store(true)
		return
	}
	s.applyCredentials(creds)
}
