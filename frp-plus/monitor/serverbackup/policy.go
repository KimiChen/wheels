package serverbackup

import (
	"bytes"
	"encoding/json"
	"io"
	"path/filepath"
	"reflect"
	"regexp"
	"sort"
	"strings"
	"unicode/utf8"
)

func stringLower(s string) string { return strings.ToLower(s) }

var grantID = regexp.MustCompile(`^[a-z][a-z0-9_-]{0,31}$`)

func absolute(p string) bool {
	return filepath.IsAbs(p) && filepath.Clean(p) == p && p != "/" && len(p) <= 4096 && !strings.ContainsAny(p, "\x00\r\n")
}
func within(p, r string) bool { return p == r || strings.HasPrefix(p, r+string(filepath.Separator)) }
func resolve(p, cwd string) string {
	if filepath.IsAbs(p) {
		return filepath.Clean(p)
	}
	return filepath.Join(cwd, p)
}

// canonical rejects aliases, unknown keys, duplicate keys and excessive nesting
// before encoding/json can apply its case-insensitive struct matching.
func canonical(data []byte, target any) error {
	if len(data) > MaxFileBytes || !utf8.Valid(data) {
		return failure("invalid_json")
	}
	d := json.NewDecoder(bytes.NewReader(data))
	d.UseNumber()
	var walk func(reflect.Type, int) error
	walk = func(t reflect.Type, depth int) error {
		if depth > 64 {
			return failure("invalid_json")
		}
		for t.Kind() == reflect.Pointer {
			t = t.Elem()
		}
		tok, e := d.Token()
		if e != nil {
			return failure("invalid_json")
		}
		switch tok {
		case json.Delim('{'):
			if t.Kind() != reflect.Struct && t.Kind() != reflect.Map {
				return failure("invalid_json")
			}
			fields := map[string]reflect.Type{}
			var collect func(reflect.Type)
			collect = func(x reflect.Type) {
				for i := 0; i < x.NumField(); i++ {
					f := x.Field(i)
					if f.PkgPath != "" {
						continue
					}
					tag := strings.Split(f.Tag.Get("json"), ",")[0]
					if tag == "-" {
						continue
					}
					if f.Anonymous && tag == "" {
						collect(f.Type)
						continue
					}
					if tag == "" {
						tag = f.Name
					}
					fields[tag] = f.Type
				}
			}
			if t.Kind() == reflect.Struct {
				collect(t)
			}
			seen := map[string]bool{}
			for d.More() {
				key, e := d.Token()
				k, ok := key.(string)
				if e != nil || !ok || seen[strings.ToLower(k)] {
					return failure("invalid_json")
				}
				seen[strings.ToLower(k)] = true
				child, ok := fields[k]
				if t.Kind() == reflect.Map {
					child = t.Elem()
					ok = true
				}
				if !ok {
					return failure("invalid_json")
				}
				if e = walk(child, depth+1); e != nil {
					return e
				}
			}
			if end, e := d.Token(); e != nil || end != json.Delim('}') {
				return failure("invalid_json")
			}
		case json.Delim('['):
			if t.Kind() != reflect.Slice && t.Kind() != reflect.Array {
				return failure("invalid_json")
			}
			for d.More() {
				if e := walk(t.Elem(), depth+1); e != nil {
					return e
				}
			}
			if end, e := d.Token(); e != nil || end != json.Delim(']') {
				return failure("invalid_json")
			}
		default:
			if tok == nil {
				return failure("invalid_json")
			}
		}
		return nil
	}
	if e := walk(reflect.TypeOf(target).Elem(), 0); e != nil {
		return e
	}
	if _, e := d.Token(); e != io.EOF {
		return failure("invalid_json")
	}
	d = json.NewDecoder(bytes.NewReader(data))
	d.DisallowUnknownFields()
	if d.Decode(target) != nil {
		return failure("invalid_json")
	}
	return nil
}
func DecodePolicy(data []byte) (Policy, error) {
	var p Policy
	if canonical(data, &p) != nil {
		return p, failure("policy_invalid")
	}
	return normalize(p)
}
func normalize(p Policy) (Policy, error) {
	bad := failure("policy_invalid")
	if p.Version != 1 || !absolute(p.ConfigFile) || !absolute(p.WorkingDir) || len(p.Roots) < 1 || len(p.Roots) > 32 || p.Files == nil || len(p.Files) > 128 {
		return p, bad
	}
	p.Roots = append([]PathGrant{}, p.Roots...)
	p.Files = append([]PathGrant{}, p.Files...)
	sort.Slice(p.Roots, func(i, j int) bool { return p.Roots[i].ID < p.Roots[j].ID })
	sort.Slice(p.Files, func(i, j int) bool { return p.Files[i].ID < p.Files[j].ID })
	ids := map[string]bool{}
	all := append(append([]PathGrant{}, p.Roots...), p.Files...)
	for i, v := range all {
		if !grantID.MatchString(v.ID) || ids[v.ID] || !absolute(v.Path) {
			return p, bad
		}
		ids[v.ID] = true
		for _, old := range all[:i] {
			if within(v.Path, old.Path) || within(old.Path, v.Path) {
				return p, bad
			}
		}
	}
	if !p.authorized(p.ConfigFile, false) || !p.authorized(p.WorkingDir, true) {
		return p, bad
	}
	return p, nil
}
func (p Policy) authorized(path string, directory bool) bool {
	if !absolute(path) {
		return false
	}
	for _, g := range p.Roots {
		if within(path, g.Path) {
			return true
		}
	}
	if !directory {
		for _, g := range p.Files {
			if path == g.Path {
				return true
			}
		}
	}
	return false
}
func policyBytes(p Policy) []byte { b, _ := json.Marshal(p); return b }
func mapping(source, target Policy) (func(string, bool) (string, error), error) {
	if len(source.Roots) != len(target.Roots) || len(source.Files) != len(target.Files) {
		return nil, failure("mapping_invalid")
	}
	for _, pair := range [][2][]PathGrant{{source.Roots, target.Roots}, {source.Files, target.Files}} {
		for i, g := range pair[0] {
			if g.ID != pair[1][i].ID {
				return nil, failure("mapping_invalid")
			}
		}
	}
	for _, s := range append(append([]PathGrant{}, source.Roots...), source.Files...) {
		for _, t := range append(append([]PathGrant{}, target.Roots...), target.Files...) {
			if s.Path != t.Path && (within(s.Path, t.Path) || within(t.Path, s.Path)) {
				return nil, failure("mapping_overlap")
			}
		}
	}
	fn := func(path string, dir bool) (string, error) {
		for i, g := range source.Roots {
			if within(path, g.Path) {
				v := target.Roots[i].Path + strings.TrimPrefix(path, g.Path)
				if target.authorized(v, dir) {
					return v, nil
				}
			}
		}
		if !dir {
			for i, g := range source.Files {
				if path == g.Path {
					return target.Files[i].Path, nil
				}
			}
		}
		return "", failure("path_unauthorized")
	}
	c, e := fn(source.ConfigFile, false)
	w, we := fn(source.WorkingDir, true)
	if e != nil || we != nil || c != target.ConfigFile || w != target.WorkingDir {
		return nil, failure("mapping_invalid")
	}
	return fn, nil
}
