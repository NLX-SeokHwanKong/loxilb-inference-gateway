package prometheus

import (
	"testing"

	"github.com/prometheus/client_golang/prometheus"
	dto "github.com/prometheus/client_model/go"
)

// The collector turns the snapshot into one histogram series per result and
// the two gauges, and reads zero with no source set.
func TestAIKeyLookupCollectorEmitsEveryFamily(t *testing.T) {
	snap := AIKeyLookupSnapshot{
		CircuitOpen: true,
		InFlight:    2,
		Results: []AIKeyLookupResult{
			{Result: "timeout", Count: 3, Sum: 0.9, Buckets: map[float64]uint64{0.1: 0, 0.3: 3, 1: 3}},
			{Result: "miss_found", Count: 0, Sum: 0, Buckets: map[float64]uint64{0.1: 0, 0.3: 0, 1: 0}},
		},
	}
	SetAIKeyLookupSource(func() AIKeyLookupSnapshot { return snap })
	t.Cleanup(func() { SetAIKeyLookupSource(func() AIKeyLookupSnapshot { return AIKeyLookupSnapshot{} }) })

	reg := prometheus.NewRegistry()
	reg.MustRegister(aiKeyLookupCollector{})
	mfs, err := reg.Gather()
	if err != nil {
		t.Fatal(err)
	}
	byName := map[string]*dto.MetricFamily{}
	for _, mf := range mfs {
		byName[mf.GetName()] = mf
	}
	h := byName["loxilb_ai_key_store_lookup_seconds"]
	if h == nil || h.GetType() != dto.MetricType_HISTOGRAM || len(h.Metric) != 2 {
		t.Fatalf("histogram family: %v", h)
	}
	for _, m := range h.Metric {
		if m.Label[0].GetValue() == "timeout" {
			if m.Histogram.GetSampleCount() != 3 || m.Histogram.GetSampleSum() != 0.9 {
				t.Fatalf("timeout series: %v", m.Histogram)
			}
		}
	}
	if g := byName["loxilb_ai_key_store_circuit_open"]; g == nil || g.GetType() != dto.MetricType_GAUGE || g.Metric[0].Gauge.GetValue() != 1 {
		t.Fatalf("circuit gauge: %v", g)
	}
	if g := byName["loxilb_ai_key_store_lookup_inflight"]; g == nil || g.Metric[0].Gauge.GetValue() != 2 {
		t.Fatalf("in-flight gauge: %v", g)
	}

	// No source: the gauges read zero and the histogram has no series.
	aiKeyLookupSource.Store(nil)
	mfs, _ = reg.Gather()
	for _, mf := range mfs {
		switch mf.GetName() {
		case "loxilb_ai_key_store_lookup_seconds":
			t.Fatalf("histogram series with no source: %v", mf)
		case "loxilb_ai_key_store_circuit_open":
			if mf.Metric[0].Gauge.GetValue() != 0 {
				t.Fatal("circuit gauge not zero with no source")
			}
		}
	}
}
