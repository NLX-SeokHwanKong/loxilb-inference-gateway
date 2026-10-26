/*
 * Copyright (c) 2026 NetLOX Inc
 * Licensed under the Apache License, Version 2.0 (the "License");
 * you may obtain a copy at http://www.apache.org/licenses/LICENSE-2.0
 */
package snapshot

import (
	"errors"
	"fmt"
	"testing"

	cmn "github.com/loxilb-io/loxilb/common"
)

type bfdAvailabilityHooks struct {
	*mockHooks
	err     error
	deletes int
}

func (h *bfdAvailabilityHooks) NetBFDGet() ([]cmn.BFDMod, error)   { return nil, h.err }
func (h *bfdAvailabilityHooks) NetBFDAdd(*cmn.BFDMod) (int, error) { return 0, h.err }
func (h *bfdAvailabilityHooks) NetBFDDel(*cmn.BFDMod) (int, error) { h.deletes++; return 0, h.err }

func TestBFDSentinelCaptureAndDelete(t *testing.T) {
	for _, tc := range []struct {
		name string
		err  error
	}{
		{"uninitialized", cmn.ErrBFDUnavailable},
		{"unsupported", cmn.ErrBFDUnsupported},
		{"wrapped uninitialized", fmt.Errorf("read: %w", cmn.ErrBFDUnavailable)},
		{"wrapped unsupported", fmt.Errorf("read: %w", cmn.ErrBFDUnsupported)},
	} {
		t.Run(tc.name, func(t *testing.T) {
			h := &bfdAvailabilityHooks{mockHooks: newMockHooks(), err: tc.err}
			doc := &Document{}
			doc.Domains.BFD = []cmn.BFDMod{{Instance: "stale"}}
			if err := getBFD(h, doc); err != nil {
				t.Fatalf("getBFD must skip unavailable BFD: %v", err)
			}
			if len(doc.Domains.BFD) != 0 {
				t.Fatal("unavailable BFD kept stale rows")
			}
			if n, err := deleteBFD(h); err != nil || n != 0 {
				t.Fatalf("deleteBFD: n=%d err=%v", n, err)
			}
			if h.deletes != 0 {
				t.Fatal("unavailable BFD attempted a session mutation")
			}
			if _, err := Capture(h, "test", "test", TriggerManual, nil); err != nil {
				t.Fatalf("capture: %v", err)
			}
			if _, err := Wipe(h, []string{DomainBFD}); err != nil {
				t.Fatalf("wipe: %v", err)
			}
			doc.Domains.BFD = []cmn.BFDMod{{Instance: "restore"}}
			for _, tolerateExists := range []bool{false, true} {
				if _, _, err := applyBFD(h, doc, tolerateExists); !errors.Is(err, tc.err) {
					t.Fatalf("restore must retain unsupported/unavailable error: %v", err)
				}
			}
		})
	}
}

func TestBFDSnapshotUnexpectedErrorsRemainFailures(t *testing.T) {
	for _, err := range []error{errors.New("unexpected BFD fault"), errors.New(cmn.ErrBFDUnavailable.Error()), errors.New(cmn.ErrBFDUnsupported.Error())} {
		h := &bfdAvailabilityHooks{mockHooks: newMockHooks(), err: err}
		if got := getBFD(h, &Document{}); !errors.Is(got, err) {
			t.Fatalf("capture swallowed unexpected error: %v", got)
		}
		if _, got := deleteBFD(h); !errors.Is(got, err) {
			t.Fatalf("delete swallowed unexpected error: %v", got)
		}
	}
}
