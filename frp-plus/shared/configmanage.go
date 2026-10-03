package shared

import (
	"bytes"
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"regexp"
	"strings"
)

const (
	ConfigManageCapability = "config.manage.v1"
	MaxConfigChanges       = 512
	MaxConfigChangeBytes   = 192 * 1024
)

var configOperationID = regexp.MustCompile(`^[0-9a-f]{8}-[0-9a-f]{4}-4[0-9a-f]{3}-[89ab][0-9a-f]{3}-[0-9a-f]{12}$`)

func NewConfigOperationID() (string, error) {
	var value [16]byte
	if _, err := rand.Read(value[:]); err != nil {
		return "", errors.New("configuration operation identity unavailable")
	}
	value[6], value[8] = value[6]&0x0f|0x40, value[8]&0x3f|0x80
	return fmt.Sprintf("%x-%x-%x-%x-%x", value[0:4], value[4:6], value[6:8], value[8:10], value[10:16]), nil
}

func ValidConfigOperationID(id string) bool { return configOperationID.MatchString(id) }

func ValidConfigDigest(value string) bool {
	if len(value) != 64 || strings.ToLower(value) != value {
		return false
	}
	_, err := hex.DecodeString(value)
	return err == nil
}

func ValidConfigOperationState(state string) bool {
	return detailEnum(state, "draft", "validated", "prepared", "applying", "verifying", "confirmed", "rejected", "conflict", "failed", "outcome_unknown", "cancelled", "rolling_back", "rolled_back", "rollback_failed")
}

// Uncertain outcomes and failed recovery retain the per-node operation lease.
// They must be reconciled from the Agent journal before another operation.
func ConfigOperationActive(state string) bool {
	return detailEnum(state, "draft", "validated", "prepared", "applying", "verifying", "outcome_unknown", "rolling_back", "rollback_failed")
}

func ConfigOperationTransitionAllowed(from, to string) bool {
	if !ValidConfigOperationState(from) || !ValidConfigOperationState(to) || from == to {
		return false
	}
	switch from {
	case "draft":
		return detailEnum(to, "validated", "rejected", "conflict", "failed", "cancelled", "outcome_unknown")
	case "validated":
		return detailEnum(to, "prepared", "rejected", "conflict", "failed", "cancelled", "outcome_unknown")
	case "prepared":
		return detailEnum(to, "applying", "conflict", "failed", "cancelled", "outcome_unknown")
	case "applying":
		return detailEnum(to, "verifying", "rolling_back", "failed", "outcome_unknown")
	case "verifying":
		return detailEnum(to, "confirmed", "rolling_back", "outcome_unknown")
	case "confirmed", "rollback_failed":
		return to == "rolling_back"
	case "rolling_back":
		return detailEnum(to, "rolled_back", "rollback_failed", "outcome_unknown")
	case "outcome_unknown":
		// The persisted Agent record, rather than a new execution, supplies this
		// transition. The coordinator must verify ID, service and digests first.
		return detailEnum(to, "validated", "prepared", "applying", "verifying", "confirmed", "rejected", "conflict", "failed", "cancelled", "rolling_back", "rolled_back", "rollback_failed")
	default:
		return false
	}
}

// Event codes are a closed vocabulary. Raw native, filesystem or peer errors
// must never enter the persisted administrative audit through this field.
func ValidConfigEventCode(code string) bool {
	return detailEnum(code, "created", "validated", "prepared", "applying", "persisted", "runtime_loaded", "verifying", "confirmed", "cancelled", "rejected", "conflict", "failed", "invalid_config", "validation_failed", "unsupported", "source_conflict", "source_drift", "expired", "busy", "write_failed", "runtime_failed", "verify_failed", "timeout", "connection_lost", "outcome_unknown", "recovery_started", "rolling_back", "rolled_back", "rollback_failed", "recovered", "revoked", "secret_stored", "external_change", "service_mismatch", "operation_not_found")
}

// ConfigChange is an edit instruction, never a redacted native configuration
// to be serialized back over the complete Store. Unedited fields stay local.
type ConfigChange struct {
	Operation string              `json:"operation"`
	Kind      string              `json:"kind"`
	Name      string              `json:"name"`
	Type      string              `json:"type"`
	CloneFrom string              `json:"clone_from,omitempty"`
	Fields    []ConfigFieldPatch  `json:"fields"`
	Secrets   []ConfigSecretPatch `json:"secrets"`
}

type ConfigFieldPatch struct {
	Path  string          `json:"path"`
	Value json.RawMessage `json:"value"`
}

type ConfigSecretPatch struct {
	Path      string `json:"path"`
	Mode      string `json:"mode"`
	Reference string `json:"reference"`
}

// The union below is deliberately smaller than the full native schema. Native
// validation still decides which of these paths belongs to a specific type.
func ValidConfigFieldPath(path string) bool {
	return detailEnum(path, "enabled", "localIP", "localPort", "remotePort", "transport.useEncryption", "transport.useCompression", "transport.bandwidthLimit", "transport.bandwidthLimitMode", "transport.proxyProtocolVersion", "customDomains", "subdomain", "locations", "hostHeaderRewrite", "httpUser", "routeByHTTPUser", "multiplexer", "allowUsers", "serverUser", "serverName", "bindAddr", "bindPort", "protocol", "keepTunnelOpen", "maxRetriesAnHour", "minRetryInterval", "fallbackTo", "fallbackTimeoutMs")
}

func ValidConfigSecretPath(path string) bool {
	return detailEnum(path, "secretKey", "httpPassword", "loadBalancer.groupKey")
}

func (field ConfigFieldPatch) Validate() error {
	invalid := errors.New("invalid configuration field patch")
	if !ValidConfigFieldPath(field.Path) || len(field.Value) == 0 || len(field.Value) > 16*1024 {
		return invalid
	}
	decoder := json.NewDecoder(bytes.NewReader(field.Value))
	decoder.UseNumber()
	var value any
	if err := decoder.Decode(&value); err != nil {
		return invalid
	}
	if decoder.Decode(new(any)) != io.EOF {
		return invalid
	}
	// There are no writable structured objects: headers, plugin options,
	// annotations, metadata and credential objects cannot hide in a patch.
	switch v := value.(type) {
	case nil, bool:
	case json.Number:
		if _, err := v.Int64(); err != nil {
			return invalid
		}
	case string:
		if !detailText(v, 1024, true) {
			return invalid
		}
	case []any:
		if len(v) > MaxDetailEndpoints {
			return invalid
		}
		for _, item := range v {
			word, ok := item.(string)
			if !ok || !detailText(word, 1024, false) {
				return invalid
			}
		}
	default:
		return invalid
	}
	return nil
}

func (secret ConfigSecretPatch) Validate() error {
	if !ValidConfigSecretPath(secret.Path) || !detailEnum(secret.Mode, "keep", "clear", "reference") ||
		(secret.Mode == "reference" && !ValidConfigOperationID(secret.Reference)) ||
		(secret.Mode != "reference" && secret.Reference != "") {
		return errors.New("invalid configuration secret instruction")
	}
	return nil
}

func (change ConfigChange) Validate() error {
	invalid := errors.New("invalid configuration object change")
	if !detailEnum(change.Operation, "create", "update", "delete", "enable", "disable") ||
		!detailEnum(change.Kind, "proxy", "visitor") || !detailText(change.Name, 256, false) ||
		change.Fields == nil || change.Secrets == nil || len(change.Fields) > 64 || len(change.Secrets) > 8 {
		return invalid
	}
	// CloneFrom identifies an original Store object of the same kind/type;
	// source existence, ownership and type are verified by the native adapter.
	if change.CloneFrom != "" && (change.Operation != "create" || !detailText(change.CloneFrom, 256, false) || change.CloneFrom == change.Name) {
		return invalid
	}
	if change.Operation == "create" {
		if change.Kind == "proxy" && !detailEnum(change.Type, "tcp", "udp", "http", "https", "tcpmux", "stcp", "sudp", "xtcp") ||
			change.Kind == "visitor" && !detailEnum(change.Type, "stcp", "sudp", "xtcp") {
			return invalid
		}
	} else if change.Type != "" {
		return invalid
	}
	if detailEnum(change.Operation, "delete", "enable", "disable") && (len(change.Fields) != 0 || len(change.Secrets) != 0) {
		return invalid
	}
	seen := map[string]bool{}
	for _, field := range change.Fields {
		if field.Validate() != nil || seen[field.Path] {
			return invalid
		}
		seen[field.Path] = true
	}
	for _, secret := range change.Secrets {
		if secret.Validate() != nil || seen[secret.Path] {
			return invalid
		}
		seen[secret.Path] = true
	}
	return nil
}

func ValidateConfigChanges(changes []ConfigChange) error {
	if len(changes) == 0 || len(changes) > MaxConfigChanges {
		return errors.New("invalid configuration change count")
	}
	seen := map[[2]string]bool{}
	for _, change := range changes {
		if err := change.Validate(); err != nil {
			return err
		}
		key := [2]string{change.Kind, change.Name}
		if seen[key] {
			return errors.New("duplicate configuration object change")
		}
		seen[key] = true
	}
	data, err := json.Marshal(changes)
	if err != nil || len(data) > MaxConfigChangeBytes {
		return errors.New("configuration changes exceed size limit")
	}
	return nil
}
