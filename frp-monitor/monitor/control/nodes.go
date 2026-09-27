package control

import (
	"context"
	"database/sql"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"math/big"
	"strconv"
	"strings"
	"time"
	"unicode/utf8"

	"github.com/fatedier/frp/extension/frpmonitor/shared"
)

const columns = `id,name,public_note,private_note,is_public,publish_billing,publish_traffic_plan,
price_minor,currency,billing_cycle,expires_at_ms,renewal_note,traffic_quota_bytes,traffic_mode,
traffic_reset_mode,traffic_reset_day,traffic_reset_timezone,traffic_period_start_at_ms,
traffic_period_end_at_ms,traffic_period_rx_bytes,traffic_period_tx_bytes,traffic_adjustment_bytes,
traffic_period_partial,traffic_day,traffic_today_rx_bytes,traffic_today_tx_bytes,traffic_today_partial,
counter_boot_id,counter_interface,counter_scope,counter_rx_bytes,counter_tx_bytes,counter_received_at_ms,
config_revision,created_at_ms,updated_at_ms,token_sha256,frp_binding`

type scanner interface{ Scan(...any) error }

func scanNode(row scanner) (*Node, error) {
	n := new(Node)
	var binding *string
	err := row.Scan(&n.ID, &n.Name, &n.PublicNote, &n.PrivateNote, &n.IsPublic, &n.PublishBilling, &n.PublishTrafficPlan,
		&n.PriceMinor, &n.Currency, &n.BillingCycle, &n.ExpiresAtMS, &n.RenewalNote, &n.TrafficQuotaBytes, &n.TrafficMode,
		&n.TrafficResetMode, &n.TrafficResetDay, &n.TrafficResetTimezone, &n.TrafficPeriodStartAtMS,
		&n.TrafficPeriodEndAtMS, &n.TrafficPeriodRXBytes, &n.TrafficPeriodTXBytes, &n.TrafficAdjustmentBytes,
		&n.TrafficPeriodPartial, &n.TrafficDay, &n.TrafficTodayRXBytes, &n.TrafficTodayTXBytes, &n.TrafficTodayPartial,
		&n.CounterBootID, &n.CounterInterface, &n.CounterScope, &n.CounterRXBytes, &n.CounterTXBytes, &n.CounterReceivedAtMS,
		&n.ConfigRevision, &n.CreatedAtMS, &n.UpdatedAtMS, &n.TokenSHA256, &binding)
	if errors.Is(err, sql.ErrNoRows) {
		return nil, ErrNotFound
	}
	if err != nil {
		return nil, err
	}
	if binding != nil {
		if err = json.Unmarshal([]byte(*binding), &n.Binding); err != nil {
			return nil, err
		}
	}
	return n, nil
}

func readNode(tx *sql.Tx, id string) (*Node, error) {
	if !validID(id) {
		return nil, fmt.Errorf("%w: node ID", ErrInvalid)
	}
	return scanNode(tx.QueryRow("SELECT "+columns+" FROM nodes WHERE id=?", id))
}

func nodeArgs(n *Node) []any {
	var binding any
	if n.Binding != nil {
		data, _ := json.Marshal(n.Binding)
		binding = string(data)
	}
	return []any{n.ID, n.Name, n.PublicNote, n.PrivateNote, n.IsPublic, n.PublishBilling, n.PublishTrafficPlan,
		n.PriceMinor, n.Currency, n.BillingCycle, n.ExpiresAtMS, n.RenewalNote, n.TrafficQuotaBytes, n.TrafficMode,
		n.TrafficResetMode, n.TrafficResetDay, n.TrafficResetTimezone, n.TrafficPeriodStartAtMS,
		n.TrafficPeriodEndAtMS, n.TrafficPeriodRXBytes, n.TrafficPeriodTXBytes, n.TrafficAdjustmentBytes,
		n.TrafficPeriodPartial, n.TrafficDay, n.TrafficTodayRXBytes, n.TrafficTodayTXBytes, n.TrafficTodayPartial,
		n.CounterBootID, n.CounterInterface, n.CounterScope, n.CounterRXBytes, n.CounterTXBytes, n.CounterReceivedAtMS,
		n.ConfigRevision, n.CreatedAtMS, n.UpdatedAtMS, n.TokenSHA256, binding}
}

func saveNode(tx *sql.Tx, n *Node) error {
	names := strings.Split(columns, ",")[1:]
	for i, name := range names {
		names[i] = strings.TrimSpace(name) + "=?"
	}
	args := nodeArgs(n)[1:]
	args = append(args, n.ID)
	_, err := tx.Exec("UPDATE nodes SET "+strings.Join(names, ",")+" WHERE id=?", args...)
	return err
}

func validID(id string) bool {
	n, err := strconv.ParseInt(id, 10, 64)
	return err == nil && n > 0 && strconv.FormatInt(n, 10) == id
}

func validBytes(v string) bool {
	if v == "0" {
		return true
	}
	if len(v) == 0 || len(v) > 78 || v[0] == '0' {
		return false
	}
	for _, c := range v {
		if c < '0' || c > '9' {
			return false
		}
	}
	return true
}

func validText(v string, max int) bool {
	return len(v) <= max && utf8.ValidString(v) && !strings.ContainsRune(v, 0)
}

func validateConfig(c NodeConfig) error {
	bad := fmt.Errorf("%w: invalid node configuration", ErrInvalid)
	if strings.TrimSpace(c.Name) == "" || !validText(c.Name, 128) || !validText(c.PublicNote, 4096) || !validText(c.PrivateNote, 4096) || !validText(c.RenewalNote, 4096) {
		return bad
	}
	if c.PriceMinor != nil {
		v, e := strconv.ParseInt(*c.PriceMinor, 10, 64)
		if e != nil || v < 0 || strconv.FormatInt(v, 10) != *c.PriceMinor {
			return bad
		}
	}
	if c.Currency != nil {
		if len(*c.Currency) != 3 {
			return bad
		}
		for _, v := range *c.Currency {
			if v < 'A' || v > 'Z' {
				return bad
			}
		}
	}
	if c.PriceMinor != nil && c.Currency == nil {
		return bad
	}
	if c.BillingCycle != nil && !validText(*c.BillingCycle, 128) {
		return bad
	}
	if c.ExpiresAtMS != nil && *c.ExpiresAtMS < 0 {
		return bad
	}
	if c.TrafficQuotaBytes != nil && !validBytes(*c.TrafficQuotaBytes) {
		return bad
	}
	if c.TrafficMode != "max" && c.TrafficMode != "total" && c.TrafficMode != "rx" && c.TrafficMode != "tx" {
		return bad
	}
	if c.TrafficResetMode != "monthly" && c.TrafficResetMode != "manual" {
		return bad
	}
	if c.TrafficResetDay < 1 || c.TrafficResetDay > 31 {
		return bad
	}
	if c.TrafficResetTimezone == "" || c.TrafficResetTimezone == "Local" {
		return bad
	}
	if _, e := time.LoadLocation(c.TrafficResetTimezone); e != nil {
		return bad
	}
	return nil
}

func validateHash(hash string) error {
	data, err := hex.DecodeString(hash)
	if err != nil || len(data) != 32 || strings.ToLower(hash) != hash {
		return fmt.Errorf("%w: invalid node credential hash", ErrInvalid)
	}
	return nil
}

func validateBinding(b *shared.FRPBinding) error {
	if b != nil && b.Validate() != nil {
		return fmt.Errorf("%w: invalid FRP binding", ErrInvalid)
	}
	return nil
}

func (s *Store) CreateNode(ctx context.Context, cfg NodeConfig, hash string, binding *shared.FRPBinding) (*Node, error) {
	if err := validateConfig(cfg); err != nil {
		return nil, err
	}
	if err := validateHash(hash); err != nil {
		return nil, err
	}
	if err := validateBinding(binding); err != nil {
		return nil, err
	}
	var result *Node
	err := s.call(ctx, func(tx *sql.Tx) error {
		var count int
		if err := tx.QueryRow("SELECT count(*) FROM nodes").Scan(&count); err != nil {
			return err
		}
		if count >= 1024 {
			return fmt.Errorf("%w: node limit exceeded", ErrConflict)
		}
		now := s.cfg.Now()
		n := &Node{NodeConfig: cfg, TokenSHA256: hash, Binding: binding, TrafficPeriodRXBytes: "0", TrafficPeriodTXBytes: "0", TrafficAdjustmentBytes: "0", TrafficTodayRXBytes: "0", TrafficTodayTXBytes: "0", ConfigRevision: 1, CreatedAtMS: now.UnixMilli(), UpdatedAtMS: now.UnixMilli()}
		s.refresh(n, now)
		args := nodeArgs(n)[1:]
		res, err := tx.Exec("INSERT INTO nodes ("+strings.Join(strings.Split(columns, ",")[1:], ",")+") VALUES ("+strings.TrimSuffix(strings.Repeat("?,", len(args)), ",")+")", args...)
		if err != nil {
			return err
		}
		id, err := res.LastInsertId()
		if err != nil {
			return err
		}
		n.ID = strconv.FormatInt(id, 10)
		result = n
		return nil
	})
	if err != nil {
		return nil, err
	}
	return result, nil
}

func (s *Store) Get(ctx context.Context, id string) (*Node, error) {
	var result *Node
	err := s.call(ctx, func(tx *sql.Tx) error {
		n, err := readNode(tx, id)
		if err != nil {
			return err
		}
		before := *n
		s.refresh(n, s.cfg.Now())
		if before != *n {
			if err = saveNode(tx, n); err != nil {
				return err
			}
		}
		result = n
		return nil
	})
	if err != nil {
		return nil, err
	}
	return result, nil
}

func (s *Store) Nodes(ctx context.Context) ([]*Node, error) {
	result := make([]*Node, 0)
	err := s.call(ctx, func(tx *sql.Tx) error {
		rows, err := tx.Query("SELECT " + columns + " FROM nodes ORDER BY id")
		if err != nil {
			return err
		}
		for rows.Next() {
			n, e := scanNode(rows)
			if e != nil {
				rows.Close()
				return e
			}
			result = append(result, n)
		}
		err = rows.Err()
		rows.Close()
		if err != nil {
			return err
		}
		now := s.cfg.Now()
		for _, n := range result {
			before := *n
			s.refresh(n, now)
			if before != *n {
				if err = saveNode(tx, n); err != nil {
					return err
				}
			}
		}
		return nil
	})
	if err != nil {
		return nil, err
	}
	return result, nil
}

// UpdateNode keeps the displayed usage when switching accounting mode. A
// provided usedBytes then calibrates that displayed value. Reset clears only
// the plan period, preserving daily traffic and the system counter baseline.
func (s *Store) UpdateNode(ctx context.Context, id string, cfg NodeConfig, usedBytes *string, reset bool, expectedRevision int64) (*Node, error) {
	if err := validateConfig(cfg); err != nil {
		return nil, err
	}
	if usedBytes != nil && !validBytes(*usedBytes) {
		return nil, fmt.Errorf("%w: invalid used bytes", ErrInvalid)
	}
	var result *Node
	err := s.call(ctx, func(tx *sql.Tx) error {
		n, err := readNode(tx, id)
		if err != nil {
			return err
		}
		if expectedRevision <= 0 || n.ConfigRevision != expectedRevision {
			return ErrConflict
		}
		now := s.cfg.Now()
		s.refresh(n, now)
		used := n.UsedBytes()
		oldMode := n.TrafficMode
		n.NodeConfig = cfg
		if oldMode != n.TrafficMode {
			n.TrafficAdjustmentBytes = new(big.Int).Sub(number(used), n.rawUsed()).String()
		}
		if n.TrafficResetMode == "monthly" {
			loc, _ := time.LoadLocation(n.TrafficResetTimezone)
			_, end := periodBounds(now, n.TrafficResetDay, loc)
			n.TrafficPeriodEndAtMS = ptr(end.UnixMilli())
		} else {
			n.TrafficPeriodEndAtMS = nil
		}
		if reset {
			n.TrafficPeriodStartAtMS = ptr(now.UnixMilli())
			n.TrafficPeriodRXBytes, n.TrafficPeriodTXBytes, n.TrafficAdjustmentBytes = "0", "0", "0"
			n.TrafficPeriodPartial = n.CounterReceivedAtMS == nil || *n.CounterReceivedAtMS != now.UnixMilli()
		}
		if usedBytes != nil {
			n.TrafficAdjustmentBytes = new(big.Int).Sub(number(*usedBytes), n.rawUsed()).String()
		}
		n.ConfigRevision++
		n.UpdatedAtMS = now.UnixMilli()
		if err = saveNode(tx, n); err != nil {
			return err
		}
		result = n
		return nil
	})
	if err != nil {
		return nil, err
	}
	return result, nil
}

func (s *Store) RotateToken(ctx context.Context, id, hash string) error {
	if err := validateHash(hash); err != nil {
		return err
	}
	return s.call(ctx, func(tx *sql.Tx) error {
		n, err := readNode(tx, id)
		if err != nil {
			return err
		}
		n.TokenSHA256 = hash
		n.ConfigRevision++
		n.UpdatedAtMS = s.cfg.Now().UnixMilli()
		return saveNode(tx, n)
	})
}

func (s *Store) SetBinding(ctx context.Context, id string, binding *shared.FRPBinding, expectedRevision int64) error {
	if err := validateBinding(binding); err != nil {
		return err
	}
	return s.call(ctx, func(tx *sql.Tx) error {
		n, err := readNode(tx, id)
		if err != nil {
			return err
		}
		if expectedRevision <= 0 || n.ConfigRevision != expectedRevision {
			return ErrConflict
		}
		n.Binding = binding
		n.ConfigRevision++
		n.UpdatedAtMS = s.cfg.Now().UnixMilli()
		return saveNode(tx, n)
	})
}

type probeDocument struct {
	Version uint64            `json:"version"`
	Nodes   []json.RawMessage `json:"nodes"`
}

func readProbes(tx *sql.Tx) ([]byte, error) {
	var data []byte
	err := tx.QueryRow("SELECT probe_json FROM settings WHERE id=1").Scan(&data)
	return data, err
}

func (s *Store) ReadProbes(ctx context.Context) ([]byte, error) {
	var data []byte
	err := s.call(ctx, func(tx *sql.Tx) error { var err error; data, err = readProbes(tx); return err })
	if err != nil {
		return nil, err
	}
	return data, nil
}

func (s *Store) WriteProbes(ctx context.Context, data []byte) error {
	if len(data) > 1024*1024 {
		return fmt.Errorf("%w: probe document too large", ErrInvalid)
	}
	var doc probeDocument
	if err := json.Unmarshal(data, &doc); err != nil || doc.Version == 0 || doc.Nodes == nil || len(doc.Nodes) > 1024 {
		return fmt.Errorf("%w: invalid probe document", ErrInvalid)
	}
	return s.call(ctx, func(tx *sql.Tx) error {
		old, err := readProbes(tx)
		if err != nil {
			return err
		}
		var previous probeDocument
		if err = json.Unmarshal(old, &previous); err != nil {
			return err
		}
		if string(old) == string(data) {
			return nil
		}
		if doc.Version <= previous.Version {
			return ErrConflict
		}
		seen := map[string]bool{}
		for _, entry := range doc.Nodes {
			var ref struct {
				AgentID string `json:"agent_id"`
			}
			if json.Unmarshal(entry, &ref) != nil || seen[ref.AgentID] {
				return fmt.Errorf("%w: invalid probe node", ErrInvalid)
			}
			if _, err = readNode(tx, ref.AgentID); err != nil {
				if errors.Is(err, ErrNotFound) || errors.Is(err, ErrInvalid) {
					return fmt.Errorf("%w: unknown probe node", ErrInvalid)
				}
				return err
			}
			seen[ref.AgentID] = true
		}
		_, err = tx.Exec("UPDATE settings SET probe_json=? WHERE id=1", string(data))
		return err
	})
}

func (s *Store) DeleteNode(ctx context.Context, id string) error {
	return s.call(ctx, func(tx *sql.Tx) error {
		if _, err := readNode(tx, id); err != nil {
			return err
		}
		data, err := readProbes(tx)
		if err != nil {
			return err
		}
		var doc probeDocument
		if err = json.Unmarshal(data, &doc); err != nil {
			return err
		}
		kept := make([]json.RawMessage, 0, len(doc.Nodes))
		for _, entry := range doc.Nodes {
			var ref struct {
				AgentID string `json:"agent_id"`
			}
			if err = json.Unmarshal(entry, &ref); err != nil {
				return err
			}
			if ref.AgentID != id {
				kept = append(kept, entry)
			}
		}
		if len(kept) != len(doc.Nodes) {
			if doc.Version == ^uint64(0) {
				return errors.New("probe version exhausted")
			}
			doc.Version++
			doc.Nodes = kept
			data, err = json.Marshal(doc)
			if err != nil {
				return err
			}
			if _, err = tx.Exec("UPDATE settings SET probe_json=? WHERE id=1", string(data)); err != nil {
				return err
			}
		}
		_, err = tx.Exec("DELETE FROM nodes WHERE id=?", id)
		return err
	})
}
