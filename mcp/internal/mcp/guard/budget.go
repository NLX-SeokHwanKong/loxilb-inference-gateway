/*
 * Copyright (c) 2026 NetLOX Inc
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

package guard

import (
	"fmt"
	"sync"
	"time"
)

// The mutation budget (change-gate requirement 4): a runaway agent that
// repeats a change, or walks through a bad one over and over, is stopped by
// the bridge rather than by the gateway. Two brakes, both per caller (the
// authenticated HTTP client, or the stdio session):
//
//   - at most PerMinute mutations are SENT in any sliding minute; the next
//     one is refused with the time until the oldest leaves the window;
//   - after MaxConsecutiveFailures mutations in a row were answered with an
//     error, no further mutation is sent until a read-only call succeeds
//     (the agent must look at the state before it changes it again).
//
// A refusal is an error the agent can read; nothing queues. Previews and
// reads are never counted: only a request that would change the gateway is.

// Budget defaults and floors. Neither brake can be configured below its
// floor: the budget can be made slow, not switched off.
const (
	DefaultMutationsPerMinute = 6
	DefaultMaxConsecutiveFail = 3
	MinMutationsPerMinute     = 1
	MinMaxConsecutiveFail     = 1
)

// ChangeBudget meters executed mutations per caller key.
type ChangeBudget struct {
	mu        sync.Mutex
	perMinute int
	maxFail   int
	now       func() time.Time
	callers   map[string]*callerBudget
}

type callerBudget struct {
	sent      []time.Time // mutations sent inside the last minute
	failures  int         // consecutive failed mutations since the last successful read
	stopped   bool
	perMinute int // this caller's own allowance; 0 = the budget's default
}

// NewChangeBudget builds a budget; values below the floors are raised to
// them, so a zero value means the default.
func NewChangeBudget(perMinute, maxConsecutiveFailures int) *ChangeBudget {
	if perMinute <= 0 {
		perMinute = DefaultMutationsPerMinute
	}
	if perMinute < MinMutationsPerMinute {
		perMinute = MinMutationsPerMinute
	}
	if maxConsecutiveFailures <= 0 {
		maxConsecutiveFailures = DefaultMaxConsecutiveFail
	}
	if maxConsecutiveFailures < MinMaxConsecutiveFail {
		maxConsecutiveFailures = MinMaxConsecutiveFail
	}
	return &ChangeBudget{perMinute: perMinute, maxFail: maxConsecutiveFailures,
		now: time.Now, callers: map[string]*callerBudget{}}
}

// SetClock overrides the time source (tests only).
func (b *ChangeBudget) SetClock(now func() time.Time) { b.now = now }

// Limits reports the configured brakes.
func (b *ChangeBudget) Limits() (perMinute, maxConsecutiveFailures int) {
	return b.perMinute, b.maxFail
}

// SetCaller gives one caller key its own per-minute allowance (a client
// entry's mutations_per_minute); a value below the floor is raised to it.
// The consecutive-failure stop is not per caller: it is the same brake for
// everyone.
func (b *ChangeBudget) SetCaller(key string, perMinute int) {
	if b == nil || perMinute <= 0 {
		return
	}
	if perMinute < MinMutationsPerMinute {
		perMinute = MinMutationsPerMinute
	}
	b.mu.Lock()
	defer b.mu.Unlock()
	b.caller(key).perMinute = perMinute
}

// limitOf is the per-minute allowance in force for a caller (lock held).
func (b *ChangeBudget) limitOf(c *callerBudget) int {
	if c.perMinute > 0 {
		return c.perMinute
	}
	return b.perMinute
}

// BudgetError is the refusal a caller gets when a brake is on. Kind is
// "exceeded" (the per-minute window is full; RetryAfter says when one
// slot frees) or "stopped" (consecutive failures; a successful read-only
// call lifts it).
type BudgetError struct {
	Kind       string
	Caller     string
	RetryAfter time.Duration
	Sent       int
	PerMinute  int
	Failures   int
}

func (e *BudgetError) Error() string {
	switch e.Kind {
	case "stopped":
		return fmt.Sprintf("mutation budget: stopped after %d consecutive failed mutations for %s; "+
			"nothing was sent. Make a successful read-only call (inspect the state the failures left) before the next mutation",
			e.Failures, e.Caller)
	default:
		secs := int(e.RetryAfter.Seconds() + 0.999)
		if secs < 1 {
			secs = 1
		}
		return fmt.Sprintf("mutation budget: %d of %d mutations per minute already sent for %s; "+
			"nothing was sent. Retry in %d s, or stop and report if the change keeps being needed",
			e.Sent, e.PerMinute, e.Caller, secs)
	}
}

func (b *ChangeBudget) caller(key string) *callerBudget {
	c, ok := b.callers[key]
	if !ok {
		c = &callerBudget{}
		b.callers[key] = c
	}
	return c
}

func (c *callerBudget) prune(now time.Time) {
	cut := now.Add(-time.Minute)
	i := 0
	for i < len(c.sent) && !c.sent[i].After(cut) {
		i++
	}
	c.sent = c.sent[i:]
}

// Check reports whether the caller may send a mutation now. It consumes
// nothing: Record does, once the request was sent.
func (b *ChangeBudget) Check(key string) error {
	if b == nil {
		return nil
	}
	b.mu.Lock()
	defer b.mu.Unlock()
	c := b.caller(key)
	now := b.now()
	c.prune(now)
	if c.stopped {
		return &BudgetError{Kind: "stopped", Caller: key, Failures: c.failures}
	}
	if limit := b.limitOf(c); len(c.sent) >= limit {
		return &BudgetError{Kind: "exceeded", Caller: key, Sent: len(c.sent), PerMinute: limit,
			RetryAfter: c.sent[0].Add(time.Minute).Sub(now)}
	}
	return nil
}

// Record counts a mutation that was sent. ok says whether the gateway
// accepted it; a run of maxFail refusals stops the caller.
func (b *ChangeBudget) Record(key string, ok bool) {
	if b == nil {
		return
	}
	b.mu.Lock()
	defer b.mu.Unlock()
	c := b.caller(key)
	now := b.now()
	c.prune(now)
	c.sent = append(c.sent, now)
	if ok {
		c.failures = 0
		return
	}
	c.failures++
	if c.failures >= b.maxFail {
		c.stopped = true
	}
}

// ReadSucceeded lifts the consecutive-failure stop: the caller has looked
// at the gateway and may change it again.
func (b *ChangeBudget) ReadSucceeded(key string) {
	if b == nil {
		return
	}
	b.mu.Lock()
	defer b.mu.Unlock()
	c := b.caller(key)
	c.failures = 0
	c.stopped = false
}

// State reports the caller's window and stop (tests, diagnostics).
func (b *ChangeBudget) State(key string) (sent int, failures int, stopped bool) {
	if b == nil {
		return 0, 0, false
	}
	b.mu.Lock()
	defer b.mu.Unlock()
	c := b.caller(key)
	c.prune(b.now())
	return len(c.sent), c.failures, c.stopped
}
