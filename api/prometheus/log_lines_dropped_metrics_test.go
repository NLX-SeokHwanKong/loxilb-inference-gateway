package prometheus

import (
	"strings"
	"testing"

	"github.com/prometheus/client_golang/prometheus/testutil"
)

func TestLogLinesDroppedReportsEveryLevelFromStart(t *testing.T) {
	logLinesDroppedSource.Store(nil)
	want := `
# HELP loxilb_log_lines_dropped_total ` + logLinesDroppedHelp + `
# TYPE loxilb_log_lines_dropped_total counter
loxilb_log_lines_dropped_total{level="debug"} 0
loxilb_log_lines_dropped_total{level="error"} 0
loxilb_log_lines_dropped_total{level="info"} 0
loxilb_log_lines_dropped_total{level="notice"} 0
loxilb_log_lines_dropped_total{level="trace"} 0
loxilb_log_lines_dropped_total{level="warn"} 0
`
	if err := testutil.CollectAndCompare(logLinesDroppedCollector{}, strings.NewReader(want)); err != nil {
		t.Fatal(err)
	}
}

func TestLogLinesDroppedReadsItsSourceAtScrape(t *testing.T) {
	n := uint64(3)
	SetLogLinesDroppedSource(func() map[string]uint64 {
		n += 4
		// A level outside the set is not a series.
		return map[string]uint64{"debug": n, "info": 2, "fatal": 9}
	})
	defer logLinesDroppedSource.Store(nil)
	want := `
# HELP loxilb_log_lines_dropped_total ` + logLinesDroppedHelp + `
# TYPE loxilb_log_lines_dropped_total counter
loxilb_log_lines_dropped_total{level="debug"} 7
loxilb_log_lines_dropped_total{level="error"} 0
loxilb_log_lines_dropped_total{level="info"} 2
loxilb_log_lines_dropped_total{level="notice"} 0
loxilb_log_lines_dropped_total{level="trace"} 0
loxilb_log_lines_dropped_total{level="warn"} 0
`
	if err := testutil.CollectAndCompare(logLinesDroppedCollector{}, strings.NewReader(want)); err != nil {
		t.Fatal(err)
	}
	if got := testutil.CollectAndCount(logLinesDroppedCollector{}); got != 6 {
		t.Fatalf("%d series, want 6", got)
	}
}
