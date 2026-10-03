package monitor

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"io"
	"os"
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
		// Only typed configuration patch values use RawMessage. Keep objects and
		// nested containers outside the HTTP boundary; the field validator checks
		// primitive type, array contents and limits after decoding.
		if t == reflect.TypeOf(json.RawMessage{}) {
			if delim, ok := tok.(json.Delim); ok {
				if delim != json.Delim('[') {
					return errors.New("unexpected patch structure")
				}
				for d.More() {
					item, e := d.Token()
					if e != nil {
						return e
					}
					if _, nested := item.(json.Delim); nested {
						return errors.New("nested patch structure")
					}
				}
				_, err = d.Token()
				return err
			}
			return nil
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
			var addFields func(reflect.Type)
			addFields = func(typ reflect.Type) {
				for i := 0; i < typ.NumField(); i++ {
					f := typ.Field(i)
					if f.Anonymous && f.Type.Kind() == reflect.Struct {
						addFields(f.Type)
						continue
					}
					key := strings.Split(f.Tag.Get("json"), ",")[0]
					if key != "-" {
						if key == "" {
							key = f.Name
						}
						fields[key] = f.Type
					}
				}
			}
			addFields(t)
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

// Caller holds configMu. An unchanged credential preserves the latest snapshot;
// token rotation clears it and closes every socket, including pending hellos.
func (s *Service) applyCredentials(creds []credential) {
	s.mu.Lock()
	next := make(map[string]*node, len(creds))
	for _, c := range creds {
		n := s.nodes[c.AgentID]
		if n == nil || n.credential.TokenSHA256 != c.TokenSHA256 {
			n = &node{}
		}
		n.credential = c
		next[c.AgentID] = n
	}
	for conn, grant := range s.connections {
		if n := next[grant.AgentID]; n == nil || n.credential.TokenSHA256 != grant.TokenSHA256 {
			_ = conn.Close()
		}
	}
	s.nodes = next
	s.mu.Unlock()
	s.credentialError.Store(false)
	s.invalidateAdminSnapshot()
	// Remove stale cached public rows immediately when authorization is revoked.
	s.publishSnapshot(time.Now())
}
func (s *Service) reloadCredentials() {
	ctx, cancel := context.WithTimeout(s.ctx, 2*time.Second)
	defer cancel()
	if s.refreshNodes(ctx) != nil {
		s.credentialError.Store(true)
		s.invalidateAdminSnapshot()
	}
}
