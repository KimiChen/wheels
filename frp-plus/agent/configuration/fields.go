package configuration

import (
	"bytes"
	"encoding/json"
	"sort"
	"strings"
)

func fieldSpecs(kind, typ string) map[string]string {
	fields := map[string]string{"enabled": "bool", "transport.useEncryption": "bool", "transport.useCompression": "bool"}
	add := func(spec string) {
		for _, key := range strings.Fields(spec) {
			fields[key] = "string"
		}
	}
	if kind == "proxy" {
		add("localIP transport.bandwidthLimit transport.bandwidthLimitMode transport.proxyProtocolVersion")
		fields["localPort"] = "port"
		switch typ {
		case "tcp", "udp":
			fields["remotePort"] = "remote_port"
		case "http", "https", "tcpmux":
			fields["customDomains"] = "strings"
			add("subdomain")
			if typ == "http" {
				fields["locations"] = "strings"
				add("httpUser hostHeaderRewrite routeByHTTPUser")
			}
			if typ == "tcpmux" {
				add("multiplexer httpUser routeByHTTPUser")
			}
		case "stcp", "sudp", "xtcp":
			fields["allowUsers"] = "strings"
		default:
			return nil
		}
	} else if kind == "visitor" {
		if typ != "stcp" && typ != "sudp" && typ != "xtcp" {
			return nil
		}
		add("serverUser serverName bindAddr")
		fields["bindPort"] = "bind_port"
		if typ == "xtcp" {
			add("protocol fallbackTo")
			fields["keepTunnelOpen"] = "bool"
			for _, key := range []string{"maxRetriesAnHour", "minRetryInterval", "fallbackTimeoutMs"} {
				fields[key] = "nonnegative"
			}
		}
	} else {
		return nil
	}
	return fields
}

// AllowedFields is a copy of the exact editable field surface for a native type.
func AllowedFields(kind, typ string) []string {
	out := []string{}
	for key := range fieldSpecs(kind, typ) {
		out = append(out, key)
	}
	sort.Strings(out)
	return out
}
func secretFields(kind, typ string) []string {
	if kind == "visitor" && (typ == "stcp" || typ == "sudp" || typ == "xtcp") {
		return []string{"secretKey"}
	}
	if kind == "proxy" {
		switch typ {
		case "stcp", "sudp", "xtcp":
			return []string{"secretKey", "loadBalancer.groupKey"}
		case "http", "tcpmux":
			return []string{"httpPassword", "loadBalancer.groupKey"}
		default:
			return []string{"loadBalancer.groupKey"}
		}
	}
	return nil
}

func validateField(spec string, raw json.RawMessage) error {
	if len(raw) > 16*1024 || strictJSON(raw) != nil {
		return failure("invalid_field")
	}
	if bytes.Equal(bytes.TrimSpace(raw), []byte("null")) {
		return nil
	}
	switch spec {
	case "bool":
		var v bool
		if json.Unmarshal(raw, &v) != nil {
			return failure("invalid_field")
		}
	case "string":
		var v string
		if json.Unmarshal(raw, &v) != nil || len(v) > 1024 {
			return failure("invalid_field")
		}
	case "strings":
		var v []string
		if json.Unmarshal(raw, &v) != nil || len(v) > 32 {
			return failure("invalid_field")
		}
		for _, s := range v {
			if s == "" || len(s) > 1024 {
				return failure("invalid_field")
			}
		}
	case "port", "remote_port", "bind_port", "nonnegative":
		var v int
		if json.Unmarshal(raw, &v) != nil {
			return failure("invalid_field")
		}
		min, max := 0, 65535
		if spec == "port" {
			min = 1
		}
		if spec == "bind_port" {
			min = -1
		}
		if spec == "nonnegative" {
			max = 2147483647
		}
		if v < min || v > max || (spec == "bind_port" && v == 0) {
			return failure("invalid_field")
		}
	default:
		return failure("field_read_only")
	}
	return nil
}

func hasPlugin(o object) bool {
	var typ string
	raw, ok := getPath(o.raw, "plugin.type")
	return ok && json.Unmarshal(raw, &typ) == nil && typ != ""
}
func objectEnabled(o object) bool {
	if o.proxy != nil {
		p := o.proxy.GetBaseConfig().Enabled
		return p == nil || *p
	}
	v := o.visitor.GetBaseConfig().Enabled
	return v == nil || *v
}
func inStart(name string, start []string) bool {
	if len(start) == 0 {
		return true
	}
	for _, item := range start {
		if item == name {
			return true
		}
	}
	return false
}

func viewObject(o object, start []string, conflict bool) ObjectView {
	view := ObjectView{Kind: o.kind, Name: o.name, Type: o.typ, Source: o.source, Writable: o.source == "store" && !conflict && !hasPlugin(o),
		Active: objectEnabled(o) && inStart(o.name, start), Fields: map[string]json.RawMessage{}, Secrets: map[string]SecretState{}, ReadOnlyFields: []string{}, Issues: []Issue{}}
	for _, path := range AllowedFields(o.kind, o.typ) {
		if v, ok := getPath(o.raw, path); ok {
			view.Fields[path] = v
		}
	}
	if _, ok := view.Fields["enabled"]; !ok {
		view.Fields["enabled"] = json.RawMessage("true")
	}
	for _, path := range append(secretFields(o.kind, o.typ), "loadBalancer.groupKey", "plugin.password", "plugin.httpPassword") {
		if raw, ok := getPath(o.raw, path); ok {
			var value string
			_ = json.Unmarshal(raw, &value)
			view.Secrets[path] = SecretState{Present: value != ""}
		}
	}
	for key := range o.raw {
		if key == "name" || key == "type" || key == "transport" {
			continue
		}
		if _, ok := fieldSpecs(o.kind, o.typ)[key]; ok {
			continue
		}
		secret := false
		for _, path := range secretFields(o.kind, o.typ) {
			if key == path {
				secret = true
			}
		}
		if !secret {
			view.ReadOnlyFields = append(view.ReadOnlyFields, key)
		}
	}
	sort.Strings(view.ReadOnlyFields)
	if o.source != "store" {
		view.Issues = append(view.Issues, Issue{Code: "source_read_only"})
	}
	if conflict {
		view.Issues = append(view.Issues, Issue{Code: "ownership_conflict"})
	}
	if hasPlugin(o) {
		view.Issues = append(view.Issues, Issue{Code: "advanced_read_only"})
	}
	return view
}
