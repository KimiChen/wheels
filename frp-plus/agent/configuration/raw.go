package configuration

import (
	"bytes"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"io"
	"sort"
	"strings"
	"unicode"
	"unicode/utf8"

	v1 "github.com/fatedier/frp/pkg/config/v1"
)

type object struct {
	kind, name, typ, source string
	raw                     map[string]json.RawMessage
	proxy                   v1.ProxyConfigurer
	visitor                 v1.VisitorConfigurer
}

func (o object) key() string  { return o.kind + "/" + o.name }
func hash(data []byte) string { sum := sha256.Sum256(data); return hex.EncodeToString(sum[:]) }

// Reject duplicate object members recursively BEFORE any map decoder can erase
// them. Store parsing is intentionally stricter than the native Store loader.
func strictJSON(data []byte) error {
	if len(data) > MaxInputBytes || !utf8.Valid(data) {
		return failure("invalid_source")
	}
	d := json.NewDecoder(bytes.NewReader(data))
	d.UseNumber()
	var walk func(int) error
	walk = func(depth int) error {
		if depth > 32 {
			return failure("invalid_source")
		}
		t, err := d.Token()
		if err != nil {
			return failure("invalid_source")
		}
		delim, ok := t.(json.Delim)
		if !ok {
			return nil
		}
		switch delim {
		case '{':
			seen := map[string]bool{}
			for d.More() {
				k, e := d.Token()
				if e != nil {
					return failure("invalid_source")
				}
				name, ok := k.(string)
				if !ok || seen[name] {
					return failure("duplicate_key")
				}
				seen[name] = true
				if e = walk(depth + 1); e != nil {
					return e
				}
			}
		case '[':
			for d.More() {
				if err := walk(depth + 1); err != nil {
					return err
				}
			}
		default:
			return failure("invalid_source")
		}
		_, err = d.Token()
		if err != nil {
			return failure("invalid_source")
		}
		return nil
	}
	if err := walk(0); err != nil {
		return err
	}
	if _, err := d.Token(); err != io.EOF {
		return failure("invalid_source")
	}
	return nil
}

func parseObject(kind, origin string, data []byte) (object, error) {
	o := object{kind: kind, source: origin}
	if err := strictJSON(data); err != nil {
		return o, err
	}
	if json.Unmarshal(data, &o.raw) != nil || o.raw == nil {
		return o, failure("invalid_source")
	}
	var err error
	options := v1.DecodeOptions{DisallowUnknownFields: true}
	switch kind {
	case "proxy":
		o.proxy, err = v1.DecodeProxyConfigurerJSON(data, options)
		if err == nil {
			o.name = o.proxy.GetBaseConfig().Name
			o.typ = o.proxy.GetBaseConfig().Type
		}
	case "visitor":
		o.visitor, err = v1.DecodeVisitorConfigurerJSON(data, options)
		if err == nil {
			o.name = o.visitor.GetBaseConfig().Name
			o.typ = o.visitor.GetBaseConfig().Type
		}
	default:
		return o, failure("invalid_kind")
	}
	if err != nil {
		return o, failure("unsupported_source")
	}
	if !identifier(o.name) {
		return o, failure("invalid_name")
	}
	return o, nil
}

func identifier(value string) bool {
	if value == "" || len(value) > 256 || !utf8.ValidString(value) {
		return false
	}
	for _, r := range value {
		if !unicode.IsPrint(r) {
			return false
		}
	}
	return true
}

func parseStore(data []byte) ([]object, error) {
	if err := strictJSON(data); err != nil {
		return nil, err
	}
	var root map[string]json.RawMessage
	if json.Unmarshal(data, &root) != nil || root == nil {
		return nil, failure("invalid_source")
	}
	for key := range root {
		if key != "proxies" && key != "visitors" {
			return nil, failure("unsupported_source")
		}
	}
	out := []object{}
	seen := map[string]bool{}
	for _, pair := range [][2]string{{"proxies", "proxy"}, {"visitors", "visitor"}} {
		var entries []json.RawMessage
		if raw, ok := root[pair[0]]; ok {
			if json.Unmarshal(raw, &entries) != nil {
				return nil, failure("invalid_source")
			}
		}
		for _, raw := range entries {
			entry, err := parseObject(pair[1], "store", raw)
			if err != nil {
				return nil, err
			}
			if seen[entry.key()] {
				return nil, failure("duplicate_name")
			}
			seen[entry.key()] = true
			out = append(out, entry)
		}
	}
	if len(out) > MaxObjects {
		return nil, failure("limit_exceeded")
	}
	return out, nil
}

// DecodeStore decodes an existing private Store snapshot without applying
// defaults or executing dependencies. A missing Store is represented by the
// caller as empty Objects; an existing empty/invalid file is an error.
func DecodeStore(data []byte) (Objects, error) {
	values, err := parseStore(data)
	if err != nil {
		return Objects{}, err
	}
	return native(values), nil
}

func fromNative(values Objects, origin string) ([]object, error) {
	out := []object{}
	seen := map[string]bool{}
	add := func(kind string, value any) error {
		data, err := json.Marshal(value)
		if err != nil {
			return failure("invalid_memory")
		}
		o, err := parseObject(kind, origin, data)
		if err != nil {
			return err
		}
		if seen[o.key()] {
			return failure("duplicate_name")
		}
		seen[o.key()] = true
		out = append(out, o)
		return nil
	}
	for _, p := range values.Proxies {
		if err := add("proxy", p); err != nil {
			return nil, err
		}
	}
	for _, v := range values.Visitors {
		if err := add("visitor", v); err != nil {
			return nil, err
		}
	}
	if len(out) > MaxObjects {
		return nil, failure("limit_exceeded")
	}
	return out, nil
}

func native(values []object) Objects {
	out := Objects{Proxies: []v1.ProxyConfigurer{}, Visitors: []v1.VisitorConfigurer{}}
	for _, o := range values {
		if o.proxy != nil {
			out.Proxies = append(out.Proxies, o.proxy.Clone())
		} else {
			out.Visitors = append(out.Visitors, o.visitor.Clone())
		}
	}
	return out
}

func fingerprint(values []object) (string, error) {
	items := map[string]json.RawMessage{}
	for _, o := range values {
		var value any = o.proxy
		if o.visitor != nil {
			value = o.visitor
		}
		data, err := json.Marshal(value)
		if err != nil {
			return "", failure("invalid_memory")
		}
		items[o.key()] = data
	}
	data, err := json.Marshal(items)
	if err != nil {
		return "", failure("invalid_memory")
	}
	return hash(data), nil
}

func encodeStore(values []object) ([]byte, error) {
	root := map[string][]json.RawMessage{"proxies": {}, "visitors": {}}
	ordered := append([]object(nil), values...)
	sort.Slice(ordered, func(i, j int) bool { return ordered[i].key() < ordered[j].key() })
	for _, o := range ordered {
		data, err := json.Marshal(o.raw)
		if err != nil {
			return nil, failure("invalid_source")
		}
		key := "proxies"
		if o.kind == "visitor" {
			key = "visitors"
		}
		root[key] = append(root[key], data)
	}
	data, err := json.MarshalIndent(root, "", "  ")
	if err != nil {
		return nil, failure("invalid_source")
	}
	return append(data, '\n'), nil
}

func getPath(raw map[string]json.RawMessage, path string) (json.RawMessage, bool) {
	parts := strings.Split(path, ".")
	current := raw
	for i, part := range parts {
		v, ok := current[part]
		if !ok {
			return nil, false
		}
		if i == len(parts)-1 {
			return append(json.RawMessage(nil), v...), true
		}
		var child map[string]json.RawMessage
		if json.Unmarshal(v, &child) != nil || child == nil {
			return nil, false
		}
		current = child
	}
	return nil, false
}

func setPath(raw map[string]json.RawMessage, path string, value json.RawMessage) error {
	parts := strings.SplitN(path, ".", 2)
	if len(parts) == 1 {
		if bytes.Equal(bytes.TrimSpace(value), []byte("null")) {
			delete(raw, path)
		} else {
			raw[path] = append(json.RawMessage(nil), value...)
		}
		return nil
	}
	child := map[string]json.RawMessage{}
	if data, ok := raw[parts[0]]; ok && !bytes.Equal(bytes.TrimSpace(data), []byte("null")) {
		if json.Unmarshal(data, &child) != nil {
			return failure("invalid_source")
		}
	}
	if child == nil {
		child = map[string]json.RawMessage{}
	}
	if err := setPath(child, parts[1], value); err != nil {
		return err
	}
	data, _ := json.Marshal(child)
	raw[parts[0]] = data
	return nil
}
