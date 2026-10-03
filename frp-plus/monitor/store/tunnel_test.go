package store

import (
	"context"
	"math"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/VictoriaMetrics/VictoriaMetrics/lib/storage"
	"github.com/fatedier/frp/extension/frpmonitor/shared"
)

func tunnelCounter(rx, tx, connections string) shared.TunnelCounters {
	return shared.TunnelCounters{Quality: "ok", ConnectionsQuality: "ok", Epoch: "11111111-1111-4111-8111-111111111111", Accounting: "connection_close", ByteScope: "forwarded_stream", RXBytes: &rx, TXBytes: &tx, Connections: &connections}
}
func TestTunnelTSDBExactDeltaBeforeFloatAndSeparateSeries(t *testing.T) {
	s := openTest(t)
	at := minute()
	s.AcceptTunnel("100", at, tunnelCounter("9007199254740993", "0", "2"))
	s.AcceptTunnel("100", at.Add(time.Second), tunnelCounter("9007199254740994", "7", "1"))
	s.AcceptTunnel("101", at, tunnelCounter("0", "0", "1"))
	s.AcceptTunnel("101", at.Add(time.Second), tunnelCounter("200", "300", "0"))
	flush(t, s)
	h, err := s.TunnelHistory(context.Background(), "100", at, at.Add(2*time.Minute), 0)
	if err != nil {
		t.Fatal(err)
	}
	p := h.Points[0]
	if p.RXRecordedBytes == nil || *p.RXRecordedBytes != "1" || *p.TXRecordedBytes != "7" || p.Samples != 1 || p.CoverageSeconds != 1 || *p.RXEstimatedBytesPerSecond != 1 || p.Connections == nil || *p.Connections != 1.5 {
		t.Fatal("counter cast before subtraction or mixed series", p)
	}
	if h.Points[1].RXRecordedBytes != nil || h.Points[1].Connections != nil {
		t.Fatal("missing filled zero")
	}
	other, err := s.TunnelHistory(context.Background(), "101", at, at.Add(time.Minute), 0)
	if err != nil || *other.Points[0].RXRecordedBytes != "200" {
		t.Fatal(other, err)
	}
	var mn storage.MetricName
	row := tunnelRow("rx", "100", at.UnixMilli(), 1)
	if err = mn.UnmarshalRaw(row.MetricNameRaw); err != nil {
		t.Fatal(err)
	}
	for _, tag := range mn.Tags {
		if string(tag.Key) != "tunnel_series_id" {
			t.Fatal("unexpected high cardinality label", string(tag.Key))
		}
	}
}
func TestTunnelTSDBGapResetAndUnsupportedStayUnknown(t *testing.T) {
	s := openTest(t)
	at := minute()
	s.AcceptTunnel("100", at, tunnelCounter("0", "0", "1"))
	s.AcceptTunnel("100", at.Add(30*time.Second), tunnelCounter("300", "600", "1"))
	reset := tunnelCounter("2", "3", "0")
	reset.Epoch = "22222222-2222-4222-8222-222222222222"
	s.AcceptTunnel("100", at.Add(31*time.Second), reset)
	s.AcceptTunnel("100", at.Add(32*time.Second), tunnelCounter("1", "1", "0"))
	s.AcceptTunnel("102", at, shared.TunnelCounters{Quality: "unsupported", ConnectionsQuality: "unsupported", Accounting: "not_observed", ByteScope: "not_observed"})
	flush(t, s)
	h, err := s.TunnelHistory(context.Background(), "100", at, at.Add(time.Minute), 0)
	if err != nil {
		t.Fatal(err)
	}
	if len(h.GapIntervals) != 1 || h.Points[0].RXEstimatedBytesPerSecond != nil || h.Points[0].RXRecordedBytes != nil {
		t.Fatal("gap invented instantaneous bytes/rate", h)
	}
	if h.GapIntervals[0].Reason != "counter_reset" || h.GapIntervals[0].RXRecordedBytes != nil {
		t.Fatal(h.GapIntervals)
	}
	none, err := s.TunnelHistory(context.Background(), "102", at, at.Add(time.Minute), 0)
	if err != nil || none.Points[0].RXRecordedBytes != nil {
		t.Fatal(none, err)
	}
	for _, bad := range []string{"-1", "1e3", "18446744073709551616", "01"} {
		c := tunnelCounter(bad, "0", "0")
		if s.AcceptTunnel("1", at, c) {
			t.Fatal("invalid decimal accepted")
		}
	}
}
func TestTunnelTSDBBoundedBaselineAndOnlySafeLabels(t *testing.T) {
	s := &Store{tunnelBaselines: map[string]tunnelBaseline{}}
	at := minute()
	for i := 0; i < 1024; i++ {
		s.tunnelBaselines[strconv.Itoa(i+1)] = tunnelBaseline{at: at}
	}

	s.consumeTunnel(tunnelSample{series: "2000", at: at.Add(time.Second), counters: tunnelCounter("0", "0", "1")})
	if len(s.tunnelBaselines) != 1024 {
		t.Fatal("unbounded baseline", len(s.tunnelBaselines))
	}
	for _, r := range s.rows {
		if math.IsNaN(r.Value) || math.IsInf(r.Value, 0) {
			t.Fatal("nonfinite row")
		}
		if strings.Contains(string(r.MetricNameRaw), "raw_name") {
			t.Fatal("identity label leaked")
		}
	}
}

func TestTunnelUnsupportedAndLongGapsRemainBounded(t *testing.T) {
	s := openTest(t)
	at := minute().Add(-3 * time.Hour)
	c := shared.TunnelCounters{Quality: "unsupported", ConnectionsQuality: "ok", Epoch: "11111111-1111-4111-8111-111111111111", Accounting: "not_observed", ByteScope: "not_observed"}
	n := "1"
	c.Connections = &n
	for i := 0; i < 600; i++ {
		if !s.AcceptTunnel("100", at.Add(time.Duration(i)*11*time.Second), c) || !s.AcceptTunnel("101", at.Add(time.Duration(i)*11*time.Second), tunnelCounter(strconv.Itoa(i), strconv.Itoa(i), "1")) {
			t.Fatal("queue rejected bounded test input")
		}
	}
	flush(t, s)
	h, err := s.TunnelHistory(context.Background(), "100", at, at.Add(3*time.Hour), 0)
	if err != nil || len(h.GapIntervals) != 0 {
		t.Fatal("unsupported bytes fabricated gaps", len(h.GapIntervals), err)
	}
	for _, p := range h.Points {
		if p.RXRecordedBytes != nil {
			t.Fatal("unsupported bytes invented values")
		}
	}
	gaps, err := s.TunnelHistory(context.Background(), "101", at, at.Add(3*time.Hour), 0)
	if err != nil || len(gaps.GapIntervals) == 0 || len(gaps.GapIntervals) > len(gaps.Points) {
		t.Fatal("unbounded gap response", len(gaps.GapIntervals), err)
	}
}

func TestTunnelMidnightAndClockRollbackNeverDoubleCount(t *testing.T) {
	s := openTest(t)
	at := time.Now().UTC().Truncate(24 * time.Hour).Add(-time.Second)
	samples := []struct {
		seconds int
		bytes   string
	}{{0, "100"}, {2, "110"}, {1, "101"}, {3, "111"}, {4, "113"}, {5, "1"}, {6, "3"}}
	for _, v := range samples {
		s.AcceptTunnel("100", at.Add(time.Duration(v.seconds)*time.Second), tunnelCounter(v.bytes, v.bytes, "1"))
	}
	flush(t, s)
	h, err := s.TunnelHistory(context.Background(), "100", at.Add(-time.Minute), at.Add(time.Minute), time.Minute)
	if err != nil {
		t.Fatal(err)
	}
	total := 0
	for _, p := range h.Points {
		if p.RXRecordedBytes != nil {
			n, e := strconv.Atoi(*p.RXRecordedBytes)
			if e != nil {
				t.Fatal(e)
			}
			total += n
		}
	}
	if total != 14 {
		t.Fatal("midnight reset or clock/counter overlap fabricated bytes", total, h)
	}
	if len(h.GapIntervals) != 1 || h.Points[len(h.Points)-1].RXEstimatedBytesPerSecond != nil {
		t.Fatal("clock/counter reset lacked unknown interval", h)
	}
}
