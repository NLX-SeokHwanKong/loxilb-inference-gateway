/*
 * Copyright (c) 2026 LoxiLB Authors
 *
 * Licensed under the Apache License, Version 2.0 (the "License");
 * you may not use this file except in compliance with the License.
 * You may obtain a copy of the License at:
 *
 *     http://www.apache.org/licenses/LICENSE-2.0
 *
 * Unless required by applicable law or agreed to in writing, software
 * distributed under the License is distributed on an "AS IS" BASIS,
 * WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or implied.
 * See the License for the specific language governing permissions and
 * limitations under the License.
 */

// Package logring puts a bounded in-memory queue and one writer goroutine
// between the gateway's log calls and the files and terminals they end on.
//
// The gateway's request path logs from cgo callbacks that run on the data
// plane's event-loop threads. A log line written by the caller is a
// write(2) on such a thread, and a device that stalls then stops the thread
// and every connection it serves. With a Ring between them the caller
// copies its line into a buffered channel with a non-blocking send and
// returns; the writer goroutine does every write, and the file's rotation
// with it. A line that finds the queue full is dropped and counted by
// level, and the writer says so in the log once per heartbeat.
//
// Before Start and after Stop a line is written by its caller, which is
// what start-up and shutdown want. A Fatal line is always written by its
// caller.
package logring

import (
	"bytes"
	"fmt"
	"io"
	"sync"
	"sync/atomic"
	"time"
)

// Level is the severity a dropped line is counted under.
type Level int

// The levels, most severe first. Fatal lines are never queued.
const (
	Fatal Level = iota
	Error
	Warn
	Notice
	Info
	Debug
	Trace
	nLevels
)

var levelNames = [nLevels]string{"fatal", "error", "warn", "notice", "info", "debug", "trace"}

func (l Level) String() string {
	if l < 0 || l >= nLevels {
		return "info"
	}
	return levelNames[l]
}

// Levels lists every level a line can be dropped at, in a fixed order.
func Levels() []Level {
	return []Level{Error, Warn, Notice, Info, Debug, Trace}
}

const (
	// DefaultSize is the number of lines the queue holds.
	DefaultSize = 8192
	// DefaultHeartbeat is how often the writer reports dropped lines.
	DefaultHeartbeat = time.Second

	kindLeveled = iota // a log.Logger line: file as it is, terminal without the level prefix
	kindRaw            // a finished line: file and the second writer as it is
)

type line struct {
	b      []byte
	kind   int
	prefix int
}

// Ring is the queue and its writer. The zero value is not usable; see New.
type Ring struct {
	file io.Writer // the log file; rotation happens inside its Write
	tty  io.Writer // the terminal copy of a leveled line; nil for none
	raw  io.Writer // the second destination of a raw line; nil for none

	ch        chan *line
	async     atomic.Bool
	dropped   [nLevels]atomic.Uint64
	pool      sync.Pool
	syncMu    sync.Mutex // callers writing for themselves; never taken by the writer
	flushReq  chan chan struct{}
	stop      chan struct{}
	done      chan struct{}
	ctl       sync.Mutex
	running   bool
	heartbeat time.Duration
	warned    uint64
	now       func() time.Time
}

// New returns a Ring that writes to file, copies leveled lines to tty and
// raw lines to raw (either may be nil). size is the queue's length in
// lines; zero or less means DefaultSize. Lines are written by their
// callers until Start.
func New(file, tty, raw io.Writer, size int) *Ring {
	if size <= 0 {
		size = DefaultSize
	}
	r := &Ring{
		file:      file,
		tty:       tty,
		raw:       raw,
		ch:        make(chan *line, size),
		flushReq:  make(chan chan struct{}),
		heartbeat: DefaultHeartbeat,
		now:       time.Now,
	}
	r.pool.New = func() any { return &line{b: make([]byte, 0, 256)} }
	return r
}

// Start hands the writing to the writer goroutine.
func (r *Ring) Start() {
	r.ctl.Lock()
	defer r.ctl.Unlock()
	if r.running {
		return
	}
	r.stop = make(chan struct{})
	r.done = make(chan struct{})
	r.running = true
	go r.run(r.stop, r.done)
	r.async.Store(true)
}

// Stop makes callers write for themselves again and waits up to timeout
// for the writer to put out what is queued. It reports whether it did.
func (r *Ring) Stop(timeout time.Duration) bool {
	r.ctl.Lock()
	defer r.ctl.Unlock()
	if !r.running {
		return true
	}
	r.async.Store(false)
	close(r.stop)
	r.running = false
	select {
	case <-r.done:
		return true
	case <-time.After(timeout):
		return false
	}
}

// Flush waits up to timeout until every line queued before the call has
// been written. It reports whether that happened.
func (r *Ring) Flush(timeout time.Duration) bool {
	if !r.async.Load() {
		return true
	}
	ack := make(chan struct{})
	t := time.NewTimer(timeout)
	defer t.Stop()
	select {
	case r.flushReq <- ack:
	case <-t.C:
		return false
	}
	select {
	case <-ack:
		return true
	case <-t.C:
		return false
	}
}

// Dropped returns the number of lines of that level dropped on a full
// queue since the Ring was made.
func (r *Ring) Dropped(l Level) uint64 {
	if l < 0 || l >= nLevels {
		return 0
	}
	return r.dropped[l].Load()
}

// Leveled returns the writer for a log.Logger whose lines start with a
// prefix of prefixLen bytes followed by "2006/01/02 15:04:05 " (log.Ldate |
// log.Ltime). The file gets the line as it is; the terminal gets
// "2006-01-02 15:04:05 " and the message, without the prefix.
func (r *Ring) Leveled(l Level, prefixLen int) io.Writer {
	return &producer{r: r, level: l, kind: kindLeveled, prefix: prefixLen}
}

// Raw returns the writer for finished lines (logrus). A line that carries
// "level=<name>" is counted under that level when it is dropped, any other
// under Info.
func (r *Ring) Raw() io.Writer {
	return &producer{r: r, level: Info, kind: kindRaw, sniff: true}
}

type producer struct {
	r      *Ring
	level  Level
	kind   int
	prefix int
	sniff  bool
}

// Write never waits for the file or the terminal once the Ring is started:
// the line is copied and queued, or dropped and counted.
func (p *producer) Write(b []byte) (int, error) {
	r := p.r
	if !r.async.Load() || p.level == Fatal {
		r.syncMu.Lock()
		r.emit(b, p.kind, p.prefix)
		r.syncMu.Unlock()
		return len(b), nil
	}
	ln := r.pool.Get().(*line)
	ln.b = append(ln.b[:0], b...)
	ln.kind, ln.prefix = p.kind, p.prefix
	select {
	case r.ch <- ln:
	default:
		lvl := p.level
		if p.sniff {
			lvl = sniffLevel(b)
		}
		r.dropped[lvl].Add(1)
		r.pool.Put(ln)
	}
	return len(b), nil
}

var sniffed = []struct {
	tag []byte
	l   Level
}{
	{[]byte("level=debug"), Debug}, {[]byte("level=trace"), Trace},
	{[]byte("level=warning"), Warn}, {[]byte("level=error"), Error},
}

func sniffLevel(b []byte) Level {
	for _, s := range sniffed {
		if bytes.Contains(b, s.tag) {
			return s.l
		}
	}
	return Info
}

const stampLen = len("2006/01/02 15:04:05 ")

// emit writes one line to its destinations. A destination that fails is
// skipped: there is nowhere to report it but the log itself.
func (r *Ring) emit(b []byte, kind, prefix int) {
	if r.file != nil {
		_, _ = r.file.Write(b)
	}
	if kind == kindRaw {
		if r.raw != nil {
			_, _ = r.raw.Write(b)
		}
		return
	}
	if r.tty == nil {
		return
	}
	if len(b) < prefix+stampLen {
		_, _ = r.tty.Write(b)
		return
	}
	// "2006/01/02 15:04:05 msg" -> "2006-01-02 15:04:05 msg"
	var stamp [stampLen]byte
	copy(stamp[:], b[prefix:prefix+stampLen])
	stamp[4], stamp[7] = '-', '-'
	_, _ = r.tty.Write(stamp[:])
	_, _ = r.tty.Write(b[prefix+stampLen:])
}

func (r *Ring) take(ln *line) {
	r.emit(ln.b, ln.kind, ln.prefix)
	if cap(ln.b) <= 4096 {
		r.pool.Put(ln)
	}
}

// drain writes what is queued now, and no more than the queue can hold, so
// a caller that keeps logging cannot keep the writer in here.
func (r *Ring) drain() {
	for n := cap(r.ch); n > 0; n-- {
		select {
		case ln := <-r.ch:
			r.take(ln)
		default:
			return
		}
	}
}

func (r *Ring) run(stop, done chan struct{}) {
	defer close(done)
	tick := time.NewTicker(r.heartbeat)
	defer tick.Stop()
	for {
		select {
		case ln := <-r.ch:
			r.take(ln)
		case ack := <-r.flushReq:
			r.drain()
			close(ack)
		case <-tick.C:
			r.report()
		case <-stop:
			r.drain()
			r.report()
			return
		}
	}
}

// report writes one line for the lines dropped since the last report. It
// runs on the writer goroutine, never on the goroutine that dropped.
func (r *Ring) report() {
	var sum uint64
	var by bytes.Buffer
	for _, l := range Levels() {
		v := r.dropped[l].Load()
		sum += v
		if v > 0 {
			if by.Len() > 0 {
				by.WriteString(", ")
			}
			fmt.Fprintf(&by, "%s %d", l, v)
		}
	}
	if sum <= r.warned {
		return
	}
	msg := fmt.Sprintf("WARN: %s log: dropped %d lines, the queue was full (%d since start: %s)\n",
		r.now().Format("2006/01/02 15:04:05"), sum-r.warned, sum, by.String())
	r.warned = sum
	r.emit([]byte(msg), kindLeveled, len("WARN: "))
}
