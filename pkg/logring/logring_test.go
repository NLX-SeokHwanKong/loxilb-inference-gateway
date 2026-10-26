package logring

import (
	"bytes"
	"fmt"
	"log"
	"strings"
	"sync"
	"testing"
	"time"
)

// lockedBuf is a destination the test can read while the writer writes.
type lockedBuf struct {
	mu sync.Mutex
	b  bytes.Buffer
}

func (l *lockedBuf) Write(p []byte) (int, error) {
	l.mu.Lock()
	defer l.mu.Unlock()
	return l.b.Write(p)
}

func (l *lockedBuf) String() string {
	l.mu.Lock()
	defer l.mu.Unlock()
	return l.b.String()
}

// stalled is a destination whose Write does not return until released,
// which is what a log file on a stalled device is.
type stalled struct {
	lockedBuf
	gate    chan struct{}
	entered chan struct{}
	once    sync.Once
}

func newStalled() *stalled {
	return &stalled{gate: make(chan struct{}), entered: make(chan struct{})}
}

func (s *stalled) Write(p []byte) (int, error) {
	s.once.Do(func() { close(s.entered) })
	<-s.gate
	return s.lockedBuf.Write(p)
}

func dropped(r *Ring) uint64 {
	var n uint64
	for _, l := range Levels() {
		n += r.Dropped(l)
	}
	return n
}

func TestLineIsWrittenByItsCallerUntilStart(t *testing.T) {
	file, tty := &lockedBuf{}, &lockedBuf{}
	r := New(file, tty, nil, 4)
	lg := log.New(r.Leveled(Info, len("INFO: ")), "INFO: ", log.Ldate|log.Ltime)
	lg.Printf("starting %d\n", 1)
	got := file.String()
	if !strings.HasPrefix(got, "INFO: ") || !strings.HasSuffix(got, " starting 1\n") {
		t.Fatalf("file line = %q", got)
	}
	// The terminal copy is the date with dashes and the message, no prefix:
	// the shape the gateway's stdout has always had.
	term := tty.String()
	if strings.Contains(term, "INFO") || !strings.HasSuffix(term, " starting 1\n") || term[4] != '-' || term[7] != '-' {
		t.Fatalf("terminal line = %q", term)
	}
}

func TestFullQueueNeverBlocksTheCaller(t *testing.T) {
	file := newStalled()
	r := New(file, nil, nil, 64)
	r.heartbeat = 5 * time.Millisecond
	r.Start()
	lg := log.New(r.Leveled(Info, len("INFO: ")), "INFO: ", log.Ldate|log.Ltime)
	dbg := log.New(r.Leveled(Debug, len("DBG:  ")), "DBG:  ", log.Ldate|log.Ltime)

	lg.Printf("first\n")
	<-file.entered // the writer is inside the stalled Write from here on

	const n = 20000
	var worst time.Duration
	start := time.Now()
	for i := 0; i < n; i++ {
		t0 := time.Now()
		if i%2 == 0 {
			lg.Printf("line %d\n", i)
		} else {
			dbg.Printf("line %d\n", i)
		}
		if d := time.Since(t0); d > worst {
			worst = d
		}
	}
	total := time.Since(start)
	if worst > 50*time.Millisecond {
		t.Fatalf("a log call waited %v with the file stalled", worst)
	}
	if total > 2*time.Second {
		t.Fatalf("%d log calls took %v with the file stalled", n, total)
	}
	// The queue holds 64 lines; every other line was dropped and counted
	// under its own level.
	if got := dropped(r); got != n-64 {
		t.Fatalf("dropped = %d, want %d", got, n-64)
	}
	if r.Dropped(Info) == 0 || r.Dropped(Debug) == 0 || r.Dropped(Info)+r.Dropped(Debug) != n-64 {
		t.Fatalf("by level: info %d debug %d", r.Dropped(Info), r.Dropped(Debug))
	}
	if r.Flush(20 * time.Millisecond) {
		t.Fatal("Flush reported success while the file was stalled")
	}

	// The file comes back: the writer resumes, the queued lines are
	// written and the drop is said in the log.
	close(file.gate)
	if !r.Flush(5 * time.Second) {
		t.Fatal("Flush did not return after the file came back")
	}
	before := dropped(r)
	lg.Printf("after the stall\n")
	// Several heartbeats pass: what was reported is not reported again.
	time.Sleep(60 * time.Millisecond)
	if !r.Stop(5 * time.Second) {
		t.Fatal("Stop timed out")
	}
	out := file.String()
	if c := strings.Count(out, " line "); c != 64 {
		t.Fatalf("%d queued lines written, want 64", c)
	}
	if !strings.Contains(out, "after the stall\n") {
		t.Fatal("a line logged after the stall was not written")
	}
	want := fmt.Sprintf("log: dropped %d lines, the queue was full", n-64)
	if !strings.Contains(out, want) || strings.Count(out, "the queue was full") != 1 {
		t.Fatalf("want one report %q, log has %d", want, strings.Count(out, "the queue was full"))
	}
	if dropped(r) != before {
		t.Fatal("the drop counter moved after the file came back")
	}
}

func TestEveryLineArrivesInItsCallersOrder(t *testing.T) {
	file := &lockedBuf{}
	r := New(file, nil, nil, 0)
	r.heartbeat = 5 * time.Millisecond
	r.Start()
	const producers, lines = 8, 500
	var wg sync.WaitGroup
	for p := 0; p < producers; p++ {
		wg.Add(1)
		go func(p int) {
			defer wg.Done()
			lg := log.New(r.Leveled(Info, len("INFO: ")), "INFO: ", log.Ldate|log.Ltime)
			for i := 0; i < lines; i++ {
				lg.Printf("p=%d seq=%d\n", p, i)
				if i%50 == 0 {
					time.Sleep(time.Millisecond)
				}
			}
		}(p)
	}
	wg.Wait()
	if !r.Flush(5 * time.Second) {
		t.Fatal("Flush timed out")
	}
	next := make([]int, producers)
	total := 0
	for _, ln := range strings.Split(file.String(), "\n") {
		i := strings.Index(ln, "p=")
		if i < 0 {
			continue
		}
		var p, seq int
		if _, err := fmt.Sscanf(ln[i:], "p=%d seq=%d", &p, &seq); err != nil {
			t.Fatalf("line %q: %v", ln, err)
		}
		if seq != next[p] {
			t.Fatalf("producer %d: line %d arrived where %d was due", p, seq, next[p])
		}
		next[p]++
		total++
	}
	if total != producers*lines || dropped(r) != 0 {
		t.Fatalf("%d lines written, %d dropped; want %d and 0", total, dropped(r), producers*lines)
	}
	r.Stop(time.Second)
}

func TestRawLinesGoToTheirSecondWriterAndAreCountedByTheirLevel(t *testing.T) {
	file := newStalled()
	raw := &lockedBuf{}
	r := New(file, nil, raw, 2)
	r.Start()
	w := r.Raw()
	fmt.Fprintf(w, "time=x level=info msg=first\n")
	<-file.entered
	for i := 0; i < 2; i++ {
		fmt.Fprintf(w, "time=x level=info msg=queued\n")
	}
	fmt.Fprintf(w, "time=x level=debug msg=lost\n")
	fmt.Fprintf(w, "time=x level=warning msg=lost\n")
	fmt.Fprintf(w, "no level in this one\n")
	if r.Dropped(Debug) != 1 || r.Dropped(Warn) != 1 || r.Dropped(Info) != 1 {
		t.Fatalf("debug %d warn %d info %d, want 1 each", r.Dropped(Debug), r.Dropped(Warn), r.Dropped(Info))
	}
	close(file.gate)
	if !r.Stop(5 * time.Second) {
		t.Fatal("Stop timed out")
	}
	if c := strings.Count(raw.String(), "msg="); c != 3 {
		t.Fatalf("second writer got %d raw lines, want 3", c)
	}
	if strings.Contains(raw.String(), "the queue was full") {
		t.Fatal("the drop report went to the raw lines' writer")
	}
}

func TestFatalLineIsWrittenByItsCaller(t *testing.T) {
	file := &lockedBuf{}
	r := New(file, nil, nil, 1)
	r.heartbeat = time.Hour
	r.Start()
	defer r.Stop(time.Second)
	lg := log.New(r.Leveled(Fatal, len("CRIT: ")), "CRIT: ", log.Ldate|log.Ltime)
	lg.Printf("cannot go on\n")
	// No Flush: the line is in the file when Printf returns.
	if !strings.Contains(file.String(), "cannot go on\n") {
		t.Fatal("a fatal line was queued")
	}
}

func TestStopWritesWhatIsQueuedAndGivesTheWritingBack(t *testing.T) {
	file := &lockedBuf{}
	r := New(file, nil, nil, 0)
	r.Start()
	r.Start() // twice is harmless
	lg := log.New(r.Leveled(Info, len("INFO: ")), "INFO: ", log.Ldate|log.Ltime)
	for i := 0; i < 100; i++ {
		lg.Printf("queued %d\n", i)
	}
	if !r.Stop(5 * time.Second) {
		t.Fatal("Stop timed out")
	}
	if c := strings.Count(file.String(), "queued "); c != 100 {
		t.Fatalf("%d of 100 queued lines written at Stop", c)
	}
	lg.Printf("after the stop\n")
	if !strings.Contains(file.String(), "after the stop\n") {
		t.Fatal("a line after Stop was not written by its caller")
	}
	if !r.Stop(time.Second) || !r.Flush(time.Second) {
		t.Fatal("Stop or Flush on a stopped Ring did not return true")
	}
}

// slow is a destination that takes its time over every line.
type slow struct {
	lockedBuf
	d time.Duration
}

func (s *slow) Write(p []byte) (int, error) {
	time.Sleep(s.d)
	return s.lockedBuf.Write(p)
}

func TestFlushWaitsForWhatWasQueued(t *testing.T) {
	file := &slow{d: time.Millisecond}
	r := New(file, nil, nil, 0)
	r.Start()
	defer r.Stop(5 * time.Second)
	lg := log.New(r.Leveled(Info, len("INFO: ")), "INFO: ", log.Ldate|log.Ltime)
	const n = 200
	for i := 0; i < n; i++ {
		lg.Printf("slow %d\n", i)
	}
	if !r.Flush(10 * time.Second) {
		t.Fatal("Flush timed out")
	}
	// No waiting here: Flush has returned, so the lines are in the file.
	if c := strings.Count(file.String(), "slow "); c != n {
		t.Fatalf("%d of %d lines written when Flush returned", c, n)
	}
}
