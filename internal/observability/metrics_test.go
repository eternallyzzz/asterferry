package observability

import (
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"asterferry/internal/transport"
)

func TestObserveQUICAndMetricsEndpointSnapshot(t *testing.T) {
	m := &Metrics{}
	m.ObserveQUIC(transport.ConnectionStats{
		RTT:             1500 * time.Microsecond,
		BytesSent:       10,
		BytesReceived:   20,
		BytesLost:       1,
		PacketsSent:     2,
		PacketsReceived: 3,
		PacketsLost:     1,
		GSO:             true,
	})
	m.BeginDrain()
	m.RecordShutdown(true)
	output := httptest.NewRecorder()
	writeMetrics(output, m)
	for _, want := range []string{
		"asterferry_quic_rtt_microseconds 1500",
		"asterferry_quic_bytes_sent 10",
		"asterferry_quic_bytes_received 20",
		"asterferry_quic_packets_lost 1",
		"asterferry_quic_gso 1",
		"asterferry_quic_stats_samples 1",
		"asterferry_draining 1",
		"asterferry_shutdowns_total 1",
		"asterferry_forced_shutdowns_total 1",
	} {
		if !strings.Contains(output.Body.String(), want) {
			t.Fatalf("metrics output does not contain %q:\n%s", want, output.Body.String())
		}
	}
}
