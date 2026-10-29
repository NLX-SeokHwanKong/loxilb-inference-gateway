/*
 * Copyright (c) 2026 NetLOX Inc
 * SPDX-License-Identifier: Apache-2.0
 */

package guard

import (
	"errors"
	"strings"
	"testing"
	"time"
)

func budgetAt(perMinute, maxFail int) (*ChangeBudget, *time.Time) {
	now := time.Date(2026, 10, 29, 10, 0, 0, 0, time.UTC)
	b := NewChangeBudget(perMinute, maxFail)
	b.SetClock(func() time.Time { return now })
	return b, &now
}

func TestChangeBudgetDefaultsAndFloors(t *testing.T) {
	if pm, mf := NewChangeBudget(0, 0).Limits(); pm != DefaultMutationsPerMinute || mf != DefaultMaxConsecutiveFail {
		t.Errorf("zero = default, got %d/%d", pm, mf)
	}
	if pm, mf := NewChangeBudget(-3, -1).Limits(); pm != DefaultMutationsPerMinute || mf != DefaultMaxConsecutiveFail {
		t.Errorf("negative = default, got %d/%d", pm, mf)
	}
	if pm, mf := NewChangeBudget(1, 1).Limits(); pm != 1 || mf != 1 {
		t.Errorf("the floors are configurable values, got %d/%d", pm, mf)
	}
	var nilBudget *ChangeBudget
	if err := nilBudget.Check("x"); err != nil {
		t.Errorf("a nil budget meters nothing: %v", err)
	}
	nilBudget.Record("x", false)
	nilBudget.ReadSucceeded("x")
	nilBudget.SetCaller("x", 4)
}

func TestChangeBudgetSlidingMinute(t *testing.T) {
	b, now := budgetAt(2, 3)
	for i := 0; i < 2; i++ {
		if err := b.Check("c"); err != nil {
			t.Fatalf("mutation %d refused: %v", i+1, err)
		}
		b.Record("c", true)
		*now = now.Add(10 * time.Second)
	}
	err := b.Check("c")
	var be *BudgetError
	if !errors.As(err, &be) || be.Kind != "exceeded" || be.Sent != 2 || be.PerMinute != 2 {
		t.Fatalf("third mutation inside the minute must be refused as exceeded: %v", err)
	}
	// 20 s passed since the first was sent: the window frees it in 40 s.
	if be.RetryAfter != 40*time.Second {
		t.Errorf("retry_after = %v, want 40s", be.RetryAfter)
	}
	if !strings.Contains(err.Error(), "2 of 2 mutations per minute") || !strings.Contains(err.Error(), "Retry in 40 s") || !strings.Contains(err.Error(), "nothing was sent") {
		t.Errorf("message: %s", err)
	}
	// Another caller has its own window.
	if err := b.Check("other"); err != nil {
		t.Errorf("another caller is metered separately: %v", err)
	}
	*now = now.Add(41 * time.Second)
	if err := b.Check("c"); err != nil {
		t.Errorf("after the oldest left the window: %v", err)
	}
	if sent, _, _ := b.State("c"); sent != 1 {
		t.Errorf("window holds %d, want 1", sent)
	}
}

func TestChangeBudgetStopsAfterConsecutiveFailuresUntilARead(t *testing.T) {
	b, _ := budgetAt(60, 2)
	b.Record("c", false)
	if err := b.Check("c"); err != nil {
		t.Fatalf("one failure must not stop: %v", err)
	}
	b.Record("c", true) // a success resets the run
	b.Record("c", false)
	if _, failures, stopped := b.State("c"); failures != 1 || stopped {
		t.Fatalf("after success+failure: failures %d stopped %v", failures, stopped)
	}
	b.Record("c", false)
	err := b.Check("c")
	var be *BudgetError
	if !errors.As(err, &be) || be.Kind != "stopped" || be.Failures != 2 {
		t.Fatalf("two failures in a row must stop the caller: %v", err)
	}
	if !strings.Contains(err.Error(), "stopped after 2 consecutive failed mutations") || !strings.Contains(err.Error(), "read-only call") {
		t.Errorf("message: %s", err)
	}
	if err := b.Check("other"); err != nil {
		t.Errorf("the stop is per caller: %v", err)
	}
	b.ReadSucceeded("c")
	if err := b.Check("c"); err != nil {
		t.Errorf("a successful read lifts the stop: %v", err)
	}
	if _, failures, stopped := b.State("c"); failures != 0 || stopped {
		t.Errorf("after the read: failures %d stopped %v", failures, stopped)
	}
}

func TestChangeBudgetPerCallerAllowance(t *testing.T) {
	b, _ := budgetAt(6, 3)
	b.SetCaller("tight", 1)
	b.SetCaller("floor", 0) // ignored: 0 keeps the default
	b.SetCaller("neg", -4)  // ignored too
	b.Record("tight", true)
	err := b.Check("tight")
	var be *BudgetError
	if !errors.As(err, &be) || be.Kind != "exceeded" || be.PerMinute != 1 {
		t.Fatalf("the caller's own allowance must apply: %v", err)
	}
	for _, key := range []string{"floor", "neg", "default"} {
		b.Record(key, true)
		if err := b.Check(key); err != nil {
			t.Errorf("%s keeps the default of 6: %v", key, err)
		}
	}
	if pm, _ := b.Limits(); pm != 6 {
		t.Errorf("Limits reports the default, got %d", pm)
	}
}
