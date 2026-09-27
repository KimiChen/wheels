package control

import (
	"math/big"
	"strconv"
	"time"

	"github.com/fatedier/frp/extension/frpmonitor/shared"
)

type sample struct {
	id                 string
	at                 int64
	boot, iface, scope string
	rx, tx             uint64
}

func observation(id string, at time.Time, m shared.Metrics) *sample {
	if m.BootID.Quality != shared.QualityOK || m.BootID.Value == nil || *m.BootID.Value == "" ||
		m.Iface.Quality != shared.QualityOK || m.Iface.Value == nil ||
		m.NetRXTotal.Quality != shared.QualityOK || m.NetRXTotal.Value == nil ||
		m.NetTXTotal.Quality != shared.QualityOK || m.NetTXTotal.Value == nil {
		return nil
	}
	if m.Scope != shared.ScopeHost && m.Scope != shared.ScopeNamespace && m.Scope != shared.ScopeUnknown {
		return nil
	}
	// An empty iface is the collector's valid automatic interface selection.
	// BootID includes the selected-interface-set hash, so topology changes still
	// invalidate the baseline without inventing a non-empty interface label.
	return &sample{id: id, at: at.UnixMilli(), boot: *m.BootID.Value, iface: *m.Iface.Value, scope: string(m.Scope), rx: *m.NetRXTotal.Value, tx: *m.NetTXTotal.Value}
}

func ptr[T any](v T) *T { return &v }

// midnight uses calendar dates rather than 24-hour durations. If local midnight
// does not exist, advance to the first representable minute on that date.
func midnight(y int, m time.Month, d int, loc *time.Location) time.Time {
	t := time.Date(y, m, d, 0, 0, 0, 0, loc)
	wanted := time.Date(y, m, d, 0, 0, 0, 0, time.UTC).Format("2006-01-02 15:04")
	for t.In(loc).Format("2006-01-02 15:04") < wanted {
		t = t.Add(time.Minute)
	}
	return t
}

func resetInMonth(y int, m time.Month, day int, loc *time.Location) time.Time {
	last := time.Date(y, m+1, 0, 12, 0, 0, 0, time.UTC).Day()
	if day > last {
		day = last
	}
	return midnight(y, m, day, loc)
}

func periodBounds(at time.Time, day int, loc *time.Location) (time.Time, time.Time) {
	t := at.In(loc)
	start := resetInMonth(t.Year(), t.Month(), day, loc)
	if at.Before(start) {
		prev := time.Date(t.Year(), t.Month()-1, 1, 12, 0, 0, 0, time.UTC)
		start = resetInMonth(prev.Year(), prev.Month(), day, loc)
	}
	next := time.Date(start.Year(), start.Month()+1, 1, 12, 0, 0, 0, time.UTC)
	return start, resetInMonth(next.Year(), next.Month(), day, loc)
}

// loadResetLocation resolves the plan timezone. Configurations are validated
// before being stored, so a failure here means the row was modified outside
// the API; fall back to UTC instead of panicking inside the worker goroutine.
func loadResetLocation(name string) *time.Location {
	loc, err := time.LoadLocation(name)
	if err != nil {
		return time.UTC
	}
	return loc
}

// refresh advances the day and plan period boundaries and reports whether it
// changed the node. A boundary switch marks the totals partial: the crossing
// delta is prorated by receive time, i.e. an estimate. observe clears the flag
// once a sample continues the baseline completely inside the new boundary.
func (s *Store) refresh(n *Node, at time.Time) bool {
	changed := false
	local := at.In(s.cfg.Location)
	day := local.Format("2006-01-02")
	if n.TrafficDay == nil || *n.TrafficDay != day {
		n.TrafficDay = ptr(day)
		n.TrafficTodayRXBytes, n.TrafficTodayTXBytes = "0", "0"
		boundary := midnight(local.Year(), local.Month(), local.Day(), s.cfg.Location).UnixMilli()
		// A baseline exactly on the boundary keeps the new day exact; that is
		// rare, and every other crossing is cleared later by observe instead.
		n.TrafficTodayPartial = n.CounterReceivedAtMS == nil || *n.CounterReceivedAtMS != boundary
		changed = true
	}
	if n.TrafficResetMode == "monthly" {
		loc := loadResetLocation(n.TrafficResetTimezone)
		start, end := periodBounds(at, n.TrafficResetDay, loc)
		if n.TrafficPeriodStartAtMS == nil || n.TrafficPeriodEndAtMS == nil || at.UnixMilli() >= *n.TrafficPeriodEndAtMS {
			n.TrafficPeriodStartAtMS, n.TrafficPeriodEndAtMS = ptr(start.UnixMilli()), ptr(end.UnixMilli())
			n.TrafficPeriodRXBytes, n.TrafficPeriodTXBytes, n.TrafficAdjustmentBytes = "0", "0", "0"
			n.TrafficPeriodPartial = n.CounterReceivedAtMS == nil || *n.CounterReceivedAtMS != start.UnixMilli()
			changed = true
		}
	} else if n.TrafficPeriodStartAtMS == nil {
		n.TrafficPeriodStartAtMS = ptr(at.UnixMilli())
		n.TrafficPeriodEndAtMS = nil
		n.TrafficPeriodPartial = true
		changed = true
	}
	return changed
}

func addFraction(current string, delta uint64, from, to, boundary int64) string {
	if boundary >= to {
		return current
	}
	n := new(big.Int).SetUint64(delta)
	if boundary > from {
		n.Mul(n, big.NewInt(to-boundary))
		n.Quo(n, big.NewInt(to-from))
	}
	return n.Add(n, number(current)).String()
}

func (s *Store) observe(n *Node, next sample) bool {
	// Duplicate/reordered reports cannot move the durable baseline backwards.
	if n.CounterReceivedAtMS != nil && next.at <= *n.CounterReceivedAtMS {
		return false
	}
	at := time.UnixMilli(next.at)
	// A queued report from before midnight must not restore yesterday after a
	// current snapshot has already advanced the row to today's date.
	refreshAt := at
	if now := s.cfg.Now(); now.After(refreshAt) {
		refreshAt = now
	}
	s.refresh(n, refreshAt)
	valid := n.CounterReceivedAtMS != nil && n.CounterBootID != nil && n.CounterInterface != nil && n.CounterScope != nil && n.CounterRXBytes != nil && n.CounterTXBytes != nil
	var oldRX, oldTX uint64
	if valid {
		var e1, e2 error
		oldRX, e1 = strconv.ParseUint(*n.CounterRXBytes, 10, 64)
		oldTX, e2 = strconv.ParseUint(*n.CounterTXBytes, 10, 64)
		valid = e1 == nil && e2 == nil && *n.CounterBootID == next.boot && *n.CounterInterface == next.iface && *n.CounterScope == next.scope && oldRX <= next.rx && oldTX <= next.tx
	}
	if valid {
		from := *n.CounterReceivedAtMS
		dayStart, _ := time.Parse("2006-01-02", *n.TrafficDay)
		dayBoundary := midnight(dayStart.Year(), dayStart.Month(), dayStart.Day(), s.cfg.Location).UnixMilli()
		periodBoundary := *n.TrafficPeriodStartAtMS
		rx, tx := next.rx-oldRX, next.tx-oldTX
		n.TrafficTodayRXBytes = addFraction(n.TrafficTodayRXBytes, rx, from, next.at, dayBoundary)
		n.TrafficTodayTXBytes = addFraction(n.TrafficTodayTXBytes, tx, from, next.at, dayBoundary)
		n.TrafficPeriodRXBytes = addFraction(n.TrafficPeriodRXBytes, rx, from, next.at, periodBoundary)
		n.TrafficPeriodTXBytes = addFraction(n.TrafficPeriodTXBytes, tx, from, next.at, periodBoundary)
		gap := next.at-from > max(10*time.Second, 3*s.cfg.ReportInterval).Milliseconds()
		// A gap or a boundary-crossing estimate marks the totals partial. The
		// flag clears once a sample continues the baseline completely inside
		// the boundary (from >= boundary, no gap): from then on every counted
		// byte of the new day/period is attributed exactly, not estimated.
		n.TrafficTodayPartial = gap || from < dayBoundary
		n.TrafficPeriodPartial = gap || from < periodBoundary
	} else {
		n.TrafficTodayPartial, n.TrafficPeriodPartial = true, true
	}
	n.CounterBootID, n.CounterInterface, n.CounterScope = ptr(next.boot), ptr(next.iface), ptr(next.scope)
	n.CounterRXBytes, n.CounterTXBytes = ptr(strconv.FormatUint(next.rx, 10)), ptr(strconv.FormatUint(next.tx, 10))
	n.CounterReceivedAtMS = ptr(next.at)
	return true
}
