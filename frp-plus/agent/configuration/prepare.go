package configuration

import (
	"encoding/json"
	"regexp"
	"sort"

	nativeconfig "github.com/fatedier/frp/pkg/config"
	v1 "github.com/fatedier/frp/pkg/config/v1"
	"github.com/fatedier/frp/pkg/config/v1/validation"
	"github.com/fatedier/frp/pkg/policy/security"
)

var localReference = regexp.MustCompile(`^[0-9a-f]{8}-[0-9a-f]{4}-4[0-9a-f]{3}-[89ab][0-9a-f]{3}-[0-9a-f]{12}$`)

func applyChange(o object, change Change, secrets map[string]string) (object, error) {
	if hasPlugin(o) {
		return o, failure("advanced_read_only")
	}
	if change.Operation != "create" && change.Type != "" {
		return o, failure("invalid_change")
	}
	if len(change.Fields) > 64 || len(change.Secrets) > 8 {
		return o, failure("limit_exceeded")
	}
	if change.Operation == "enable" || change.Operation == "disable" || change.Operation == "delete" {
		if len(change.Fields) > 0 || len(change.Secrets) > 0 {
			return o, failure("invalid_change")
		}
		if change.Operation == "enable" {
			o.raw["enabled"] = json.RawMessage("true")
		}
		if change.Operation == "disable" {
			o.raw["enabled"] = json.RawMessage("false")
		}
	} else if change.Operation != "create" && change.Operation != "update" {
		return o, failure("invalid_change")
	}
	for path, value := range change.Fields {
		spec, ok := fieldSpecs(o.kind, o.typ)[path]
		if !ok {
			return o, failure("field_read_only")
		}
		if err := validateField(spec, value); err != nil {
			return o, err
		}
		if err := setPath(o.raw, path, value); err != nil {
			return o, err
		}
	}
	for path, action := range change.Secrets {
		allowed := false
		for _, field := range secretFields(o.kind, o.typ) {
			if field == path {
				allowed = true
			}
		}
		if !allowed {
			return o, failure("field_read_only")
		}
		if action.Mode != "reference" && action.Reference != "" {
			return o, failure("invalid_secret")
		}
		switch action.Mode {
		case "keep":
		case "clear":
			if err := setPath(o.raw, path, json.RawMessage("null")); err != nil {
				return o, err
			}
		case "reference":
			if !localReference.MatchString(action.Reference) {
				return o, failure("invalid_secret_reference")
			}
			value, ok := secrets[action.Reference]
			if !ok || len(value) > 16*1024 {
				return o, failure("secret_reference_unavailable")
			}
			data, _ := json.Marshal(value)
			if err := setPath(o.raw, path, data); err != nil {
				return o, err
			}
		default:
			return o, failure("invalid_secret")
		}
	}
	data, err := json.Marshal(o.raw)
	if err != nil {
		return o, failure("invalid_change")
	}
	return parseObject(o.kind, "store", data)
}

func (s *snapshot) validateCandidate(store []object) (Objects, []Issue, error) {
	all := native(append(append([]object{}, s.files...), store...))
	// Validation is deliberately BEFORE the enabled/start filter so invalid
	// disabled objects cannot be persisted and unexpectedly activated later.
	for _, p := range all.Proxies {
		p.Complete()
	}
	for _, v := range all.Visitors {
		v.Complete()
	}
	common, err := commonCopy(s.in.ReloadCommon)
	if err != nil {
		return Objects{}, nil, err
	}
	for i, path := range common.IncludeConfigFiles {
		common.IncludeConfigFiles[i] = s.resolve(path)
	}
	unsafe := security.NewUnsafeFeatures(s.in.UnsafeFeatures)
	warning, err := validation.ValidateAllClientConfig(common, all.Proxies, all.Visitors, unsafe)
	if err != nil {
		return Objects{}, nil, failure("validation_failed")
	}
	// Native visitor validation accepts negative SUDP ports, but its UDP
	// listener cannot represent the fallback-only behavior of STCP/XTCP.
	for _, v := range all.Visitors {
		base := v.GetBaseConfig()
		if base.BindPort > 65535 || base.BindPort < -1 || (base.Type == "sudp" && base.BindPort < 1) {
			return Objects{}, nil, failure("validation_failed")
		}
	}
	startup, err := commonCopy(s.in.StartupCommon)
	if err != nil {
		return Objects{}, nil, err
	}
	for i, path := range startup.IncludeConfigFiles {
		startup.IncludeConfigFiles[i] = s.resolve(path)
	}
	startupWarning, err := validation.ValidateAllClientConfig(startup, nil, nil, unsafe)
	if err != nil {
		return Objects{}, nil, failure("validation_failed")
	}
	proxies, visitors := nativeconfig.FilterClientConfigurers(common, all.Proxies, all.Visitors)
	if validation.GetClientConfigRequirements(common, proxies, visitors).VirtualNet && !s.in.VirtualNetReady {
		return Objects{}, nil, failure("runtime_capability_required")
	}
	// Fallbacks must refer to another effective visitor; cycles would otherwise
	// produce a runtime-only failure after a successfully persisted candidate.
	if err := validateFallbacks(visitors); err != nil {
		return Objects{}, nil, err
	}
	warnings := []Issue{}
	if warning != nil || startupWarning != nil {
		warnings = append(warnings, Issue{Code: "native_warning"})
	}
	return Objects{Proxies: proxies, Visitors: visitors}, warnings, nil
}

func validateFallbacks(visitors []v1.VisitorConfigurer) error {
	byName := map[string]v1.VisitorConfigurer{}
	edges := map[string]string{}
	for _, v := range visitors {
		byName[v.GetBaseConfig().Name] = v
		if x, ok := v.(*v1.XTCPVisitorConfig); ok && x.FallbackTo != "" {
			edges[x.Name] = x.FallbackTo
		}
	}
	for name, next := range edges {
		target, ok := byName[next]
		if !ok || target.GetBaseConfig().Type == "sudp" {
			return failure("dependency_conflict")
		}
		seen := map[string]bool{name: true}
		for next != "" {
			if seen[next] {
				return failure("dependency_conflict")
			}
			seen[next] = true
			next = edges[next]
		}
	}
	return nil
}

// Prepare creates transaction material from exact original Store objects and
// allowlisted object instructions. It never writes files or mutates its Input.
func Prepare(input Input, request Request) (*Prepared, error) {
	s, err := takeSnapshot(input)
	if err != nil {
		return nil, err
	}
	if request.ExpectedRevision == "" || request.ExpectedRevision != s.revision {
		return nil, failure("revision_conflict")
	}
	if len(s.issues) > 0 {
		return nil, failure(s.issues[0].Code)
	}
	if len(request.Changes) == 0 || len(request.Changes) > MaxChanges {
		return nil, failure("invalid_change")
	}
	data, err := json.Marshal(request.Changes)
	if err != nil || len(data) > 192*1024 {
		return nil, failure("limit_exceeded")
	}
	owners := map[string]bool{}
	for _, o := range s.files {
		owners[o.key()] = true
	}
	entries := map[string]object{}
	// Capture raw original objects before any edit can mutate an entries map.
	// A rename is delete+create, and cloning must have the same result regardless
	// of their order (or another edit to the source within this transaction).
	originals := map[string]json.RawMessage{}
	for _, o := range s.store {
		entries[o.key()] = o
		original, err := json.Marshal(o.raw)
		if err != nil {
			return nil, failure("invalid_source")
		}
		originals[o.key()] = original
	}
	seen := map[string]bool{}
	out := &Prepared{BaseRevision: s.revision, ContextRevision: s.contextRevision, Changes: []ChangePreview{}, Warnings: []Issue{}}
	out.OriginalStoreBytes = append([]byte(nil), s.originalStore...)
	out.OriginalStoreExists = s.originalStoreExists
	for _, change := range request.Changes {
		if !identifier(change.Name) || (change.Kind != "proxy" && change.Kind != "visitor") {
			return nil, failure("invalid_change")
		}
		if change.CloneFrom != "" && (change.Operation != "create" || !identifier(change.CloneFrom) || change.CloneFrom == change.Name) {
			return nil, failure("invalid_change")
		}
		key := change.Kind + "/" + change.Name
		if seen[key] {
			return nil, failure("duplicate_change")
		}
		seen[key] = true
		if owners[key] {
			return nil, failure("ownership_conflict")
		}
		o, exists := entries[key]
		preview := ChangePreview{Operation: change.Operation}
		if change.Operation == "create" {
			if exists {
				return nil, failure("already_exists")
			}
			if fieldSpecs(change.Kind, change.Type) == nil {
				return nil, failure("invalid_type")
			}
			if change.CloneFrom != "" {
				sourceKey := change.Kind + "/" + change.CloneFrom
				if owners[sourceKey] {
					return nil, failure("ownership_conflict")
				}
				raw, exists := originals[sourceKey]
				if !exists {
					return nil, failure("not_found")
				}
				o, err = parseObject(change.Kind, "store", raw)
				if err != nil {
					return nil, err
				}
				if o.typ != change.Type {
					return nil, failure("invalid_type")
				}
				if hasPlugin(o) {
					return nil, failure("advanced_read_only")
				}
				// parseObject owns fresh raw bytes and native objects. Only the
				// identity changes here; applyChange preserves untouched secrets
				// and known advanced members, then validates the complete object.
				o.raw["name"], _ = json.Marshal(change.Name)
			} else {
				raw, _ := json.Marshal(map[string]string{"name": change.Name, "type": change.Type})
				o, err = parseObject(change.Kind, "store", raw)
				if err != nil {
					return nil, err
				}
			}
		} else {
			if !exists {
				return nil, failure("not_found")
			}
			before := viewObject(o, s.in.ReloadCommon.Start, false)
			preview.Before = &before
		}
		o, err = applyChange(o, change, input.LocalSecrets)
		if err != nil {
			return nil, err
		}
		if change.Operation == "delete" {
			delete(entries, key)
		} else {
			entries[key] = o
			after := viewObject(o, s.in.ReloadCommon.Start, false)
			preview.After = &after
		}
		out.Changes = append(out.Changes, preview)
	}
	if len(entries)+len(s.files) > MaxObjects {
		return nil, failure("limit_exceeded")
	}
	candidate := make([]object, 0, len(entries))
	for _, o := range entries {
		candidate = append(candidate, o)
	}
	sort.Slice(candidate, func(i, j int) bool { return candidate[i].key() < candidate[j].key() })
	out.Effective, out.Warnings, err = s.validateCandidate(candidate)
	if err != nil {
		return nil, err
	}
	for _, change := range out.Changes {
		if change.After != nil && !change.After.Active {
			if raw, ok := change.After.Fields["enabled"]; ok && string(raw) == "true" {
				out.Warnings = append(out.Warnings, Issue{Code: "start_filtered"})
				break
			}
		}
	}
	out.StoreBytes, err = encodeStore(candidate)
	if err != nil {
		return nil, err
	}
	if len(out.StoreBytes) > MaxInputBytes {
		return nil, failure("limit_exceeded")
	}
	out.Store = native(candidate)
	out.CandidateSHA256 = hash(out.StoreBytes)
	// Recheck disk and dependency inputs to reject changes during an expensive
	// validation. Commit still requires its own locked recheck by the Engine.
	after, err := takeSnapshot(input)
	if err != nil {
		return nil, err
	}
	if after.revision != s.revision {
		return nil, failure("revision_conflict")
	}
	return out, nil
}
