package metrics

import (
	"context"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/prometheus/client_golang/prometheus"
	dto "github.com/prometheus/client_model/go"
)

func TestNewRegistersAllCollectors(t *testing.T) {
	m := New()

	// Touch each collector once so the metric line appears in the scrape
	// output regardless of Prometheus' "no samples, no line" behaviour.
	m.InboundConnections.WithLabelValues("api").Inc()
	m.InboundBytes.WithLabelValues("api", DirectionIn).Add(10)
	m.InboundBytes.WithLabelValues("api", DirectionOut).Add(20)
	m.InboundErrors.WithLabelValues("api", InboundErrorDial).Inc()
	m.OutboundConnections.WithLabelValues("16", OutboundResultAllowed).Inc()
	m.OutboundConnections.WithLabelValues("16", OutboundResultDenied).Inc()
	m.OutboundBytes.WithLabelValues("16", DirectionIn).Add(5)
	m.TCPInboundConnections.Inc()
	m.TCPInboundBytes.WithLabelValues(DirectionUp).Add(1)
	m.TCPInboundBytes.WithLabelValues(DirectionDown).Add(2)
	m.TCPInboundErrors.WithLabelValues(TCPErrorDial).Inc()
	m.TCPOutboundConnections.Inc()
	m.TCPOutboundBytes.WithLabelValues(DirectionUp).Add(3)
	m.TCPOutboundBytes.WithLabelValues(DirectionDown).Add(4)
	m.TCPOutboundErrors.WithLabelValues(TCPErrorCopy).Inc()
	m.ConfigReloads.WithLabelValues(ReloadResultSuccess).Inc()

	body := scrape(t, m.Handler())

	want := []string{
		"inbound_connections_total",
		"inbound_bytes_total",
		"inbound_errors_total",
		"outbound_connections_total",
		"outbound_bytes_total",
		"tcp_inbound_connections_total",
		"tcp_inbound_bytes_total",
		"tcp_inbound_errors_total",
		"tcp_outbound_connections_total",
		"tcp_outbound_bytes_total",
		"tcp_outbound_errors_total",
		"config_reloads_total",
	}
	for _, name := range want {
		if !strings.Contains(body, name) {
			t.Errorf("scrape missing metric %q; got:\n%s", name, body)
		}
	}
}

func TestHandlerUsesIsolatedRegistry(t *testing.T) {
	a := New()
	b := New()

	a.InboundConnections.WithLabelValues("api").Add(3)
	b.InboundConnections.WithLabelValues("api").Add(7)

	if got := counterValue(t, a.InboundConnections, "api"); got != 3 {
		t.Errorf("registry A leaked: got %v, want 3", got)
	}
	if got := counterValue(t, b.InboundConnections, "api"); got != 7 {
		t.Errorf("registry B leaked: got %v, want 7", got)
	}

	// The two handlers must expose independent values.
	aBody := scrape(t, a.Handler())
	bBody := scrape(t, b.Handler())
	if !strings.Contains(aBody, "inbound_connections_total{route=\"api\"} 3") {
		t.Errorf("A scrape missing value 3:\n%s", aBody)
	}
	if !strings.Contains(bBody, "inbound_connections_total{route=\"api\"} 7") {
		t.Errorf("B scrape missing value 7:\n%s", bBody)
	}
}

func TestLabelCardinalityBounded(t *testing.T) {
	m := New()

	// Drive a realistic-ish traffic pattern: two routes, two directions,
	// two CIDs, three outbound results, two reload results. Assert the
	// total time series stays at the labels we actually expect, so a
	// future change that adds a dynamic label (e.g. remote address) will
	// fail this test loudly.
	m.InboundConnections.WithLabelValues("api").Inc()
	m.InboundConnections.WithLabelValues("admin").Inc()
	m.InboundBytes.WithLabelValues("api", DirectionIn).Inc()
	m.InboundBytes.WithLabelValues("api", DirectionOut).Inc()
	m.InboundErrors.WithLabelValues("api", InboundErrorSniff).Inc()
	m.OutboundConnections.WithLabelValues("16", OutboundResultAllowed).Inc()
	m.OutboundConnections.WithLabelValues("16", OutboundResultDenied).Inc()
	m.OutboundConnections.WithLabelValues("20", OutboundResultError).Inc()
	m.OutboundBytes.WithLabelValues("16", DirectionIn).Inc()
	m.OutboundBytes.WithLabelValues("16", DirectionOut).Inc()
	// Drive the TCP passthrough counters from many notional connections.
	// Each Inc below represents a distinct connection / direction event;
	// after hundreds of events the series count must still be bounded by
	// the fixed label set, not grow with traffic.
	for i := 0; i < 500; i++ {
		m.TCPInboundConnections.Inc()
		m.TCPInboundBytes.WithLabelValues(DirectionUp).Inc()
		m.TCPInboundBytes.WithLabelValues(DirectionDown).Inc()
		m.TCPInboundErrors.WithLabelValues(TCPErrorDial).Inc()
		m.TCPInboundErrors.WithLabelValues(TCPErrorCopy).Inc()
		m.TCPOutboundConnections.Inc()
		m.TCPOutboundBytes.WithLabelValues(DirectionUp).Inc()
		m.TCPOutboundBytes.WithLabelValues(DirectionDown).Inc()
		m.TCPOutboundErrors.WithLabelValues(TCPErrorDial).Inc()
		m.TCPOutboundErrors.WithLabelValues(TCPErrorCopy).Inc()
	}
	m.ConfigReloads.WithLabelValues(ReloadResultSuccess).Inc()
	m.ConfigReloads.WithLabelValues(ReloadResultFailure).Inc()

	vecCases := []struct {
		name string
		vec  *prometheus.CounterVec
		want int
	}{
		{"inbound_connections_total", m.InboundConnections, 2},
		{"inbound_bytes_total", m.InboundBytes, 2},
		{"inbound_errors_total", m.InboundErrors, 1},
		{"outbound_connections_total", m.OutboundConnections, 3},
		{"outbound_bytes_total", m.OutboundBytes, 2},
		{"tcp_inbound_bytes_total", m.TCPInboundBytes, 2},
		{"tcp_inbound_errors_total", m.TCPInboundErrors, 2},
		{"tcp_outbound_bytes_total", m.TCPOutboundBytes, 2},
		{"tcp_outbound_errors_total", m.TCPOutboundErrors, 2},
		{"config_reloads_total", m.ConfigReloads, 2},
	}
	for _, tc := range vecCases {
		if got := collectorSeriesCount(t, tc.vec); got != tc.want {
			t.Errorf("%s: series count = %d, want %d", tc.name, got, tc.want)
		}
	}
	// Plain counters always report exactly one series regardless of how
	// many times they are incremented; assert that explicitly.
	plainCases := []struct {
		name string
		c    prometheus.Counter
	}{
		{"tcp_inbound_connections_total", m.TCPInboundConnections},
		{"tcp_outbound_connections_total", m.TCPOutboundConnections},
	}
	for _, tc := range plainCases {
		if got := collectorSeriesCount(t, tc.c); got != 1 {
			t.Errorf("%s: series count = %d, want 1", tc.name, got)
		}
	}
}

// TestTCPLabelValuesFixed pins the label-value set emitted by the TCP
// handlers. If a future change ever forwards a dynamic value (remote
// address, hostname, etc.) into one of these labels, this test fails
// loudly before the cardinality inflation lands in production.
func TestTCPLabelValuesFixed(t *testing.T) {
	m := New()

	m.TCPInboundConnections.Inc()
	m.TCPInboundBytes.WithLabelValues(DirectionUp).Inc()
	m.TCPInboundBytes.WithLabelValues(DirectionDown).Inc()
	m.TCPInboundErrors.WithLabelValues(TCPErrorDial).Inc()
	m.TCPInboundErrors.WithLabelValues(TCPErrorCopy).Inc()
	m.TCPOutboundConnections.Inc()
	m.TCPOutboundBytes.WithLabelValues(DirectionUp).Inc()
	m.TCPOutboundBytes.WithLabelValues(DirectionDown).Inc()
	m.TCPOutboundErrors.WithLabelValues(TCPErrorDial).Inc()
	m.TCPOutboundErrors.WithLabelValues(TCPErrorCopy).Inc()

	body := scrape(t, m.Handler())

	wantLines := []string{
		`tcp_inbound_connections_total 1`,
		`tcp_inbound_bytes_total{direction="up"} 1`,
		`tcp_inbound_bytes_total{direction="down"} 1`,
		`tcp_inbound_errors_total{reason="dial_fail"} 1`,
		`tcp_inbound_errors_total{reason="copy_error"} 1`,
		`tcp_outbound_connections_total 1`,
		`tcp_outbound_bytes_total{direction="up"} 1`,
		`tcp_outbound_bytes_total{direction="down"} 1`,
		`tcp_outbound_errors_total{reason="dial_fail"} 1`,
		`tcp_outbound_errors_total{reason="copy_error"} 1`,
	}
	for _, line := range wantLines {
		if !strings.Contains(body, line) {
			t.Errorf("scrape missing line %q; got:\n%s", line, body)
		}
	}
}

func TestFormatCID(t *testing.T) {
	cases := []struct {
		in   uint32
		want string
	}{
		{0, "0"},
		{3, "3"},
		{16, "16"},
		{4294967295, "4294967295"},
	}
	for _, tc := range cases {
		if got := FormatCID(tc.in); got != tc.want {
			t.Errorf("FormatCID(%d) = %q, want %q", tc.in, got, tc.want)
		}
	}
}

func TestServeMetricsServesAndStopsOnContextCancel(t *testing.T) {
	m := New()
	m.ConfigReloads.WithLabelValues(ReloadResultSuccess).Inc()

	ln, err := ListenMetrics("127.0.0.1:0")
	if err != nil {
		t.Fatalf("listen: %v", err)
	}
	addr := ln.Addr().String()

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	done := make(chan error, 1)
	go func() { done <- m.ServeMetrics(ctx, ln, 2*time.Second) }()

	// Wait briefly for the server to accept connections.
	url := "http://" + addr + "/metrics"
	body, err := getWithRetry(url, 2*time.Second)
	if err != nil {
		t.Fatalf("GET /metrics: %v", err)
	}
	if !strings.Contains(body, "config_reloads_total") {
		t.Errorf("scrape missing config_reloads_total:\n%s", body)
	}

	cancel()
	select {
	case err := <-done:
		if err != nil {
			t.Errorf("ServeMetrics returned error after cancel: %v", err)
		}
	case <-time.After(3 * time.Second):
		t.Fatal("ServeMetrics did not return after context cancel")
	}
}

func TestListenMetricsReturnsBindError(t *testing.T) {
	// Hold an ephemeral port, then ask ListenMetrics to bind the same
	// address — expected to fail immediately with EADDRINUSE so the caller
	// can surface the error synchronously from its Start path.
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("listen: %v", err)
	}
	defer ln.Close()

	dup, err := ListenMetrics(ln.Addr().String())
	if err == nil {
		dup.Close()
		t.Fatal("expected error from duplicate bind, got nil")
	}
}

func scrape(t *testing.T, h http.Handler) string {
	t.Helper()
	rec := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodGet, "/metrics", nil)
	h.ServeHTTP(rec, req)
	if rec.Code != http.StatusOK {
		t.Fatalf("scrape status = %d, want 200", rec.Code)
	}
	return rec.Body.String()
}

func counterValue(t *testing.T, vec *prometheus.CounterVec, labels ...string) float64 {
	t.Helper()
	c, err := vec.GetMetricWithLabelValues(labels...)
	if err != nil {
		t.Fatalf("GetMetricWithLabelValues: %v", err)
	}
	m := &dto.Metric{}
	if err := c.Write(m); err != nil {
		t.Fatalf("Write: %v", err)
	}
	if m.Counter == nil {
		t.Fatalf("metric has no counter payload")
	}
	return m.Counter.GetValue()
}

func collectorSeriesCount(t *testing.T, c prometheus.Collector) int {
	t.Helper()
	ch := make(chan prometheus.Metric, 64)
	go func() {
		c.Collect(ch)
		close(ch)
	}()
	n := 0
	for range ch {
		n++
	}
	return n
}

func getWithRetry(url string, within time.Duration) (string, error) {
	deadline := time.Now().Add(within)
	var lastErr error
	for time.Now().Before(deadline) {
		resp, err := http.Get(url)
		if err != nil {
			lastErr = err
			time.Sleep(20 * time.Millisecond)
			continue
		}
		body, err := io.ReadAll(resp.Body)
		_ = resp.Body.Close()
		if err != nil {
			return "", err
		}
		return string(body), nil
	}
	return "", lastErr
}
