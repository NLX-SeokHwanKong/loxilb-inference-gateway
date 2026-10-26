package snapshot

import (
	"errors"
	"fmt"
	cmn "github.com/loxilb-io/loxilb/common"
	"slices"
)

// Assignments form a separate domain so documents predating schema 1.12
// cannot claim coverage of configuration they never captured. Apply follows
// BGP definitions; reverse deletion detaches bindings before deleting policies.
func getBGPPolicyAssignment(h Hooks, d *Document) error {
	items, err := h.NetGoBGPPolicyApplyGet()
	if isSubsystemUnavailable(err) {
		return nil
	}
	if err != nil {
		return err
	}
	d.Domains.BGPPolicyAssignment = items
	return nil
}
func applyBGPPolicyAssignment(h Hooks, d *Document, tolerateExists bool) (int, int, error) {
	// Empty captured domains are valid when the optional speaker is disabled.
	// Boot replay has no wipe to perform and no bindings to reconcile here.
	// Nonempty desired assignments still require a successful speaker read.
	if len(d.Domains.BGPPolicyAssignment) == 0 {
		return 0, 0, nil
	}

	n, skipped := 0, 0
	var live []cmn.GoBGPPolicyApply
	if tolerateExists {
		var err error
		live, err = h.NetGoBGPPolicyApplyGet()
		if err != nil {
			return 0, 0, err
		}
	}
	for _, item := range d.Domains.BGPPolicyAssignment {
		duplicate := false
		for _, existing := range live {
			if existing.NeighIPAddress == item.NeighIPAddress && existing.PolicyType == item.PolicyType {
				if existing.RouteAction != item.RouteAction || !slices.Equal(existing.Polices, item.Polices) {
					return n, skipped, fmt.Errorf("conflicting existing BGP policy assignment %s/%s", item.NeighIPAddress, item.PolicyType)
				}
				duplicate = true
				break
			}
		}
		if duplicate {
			skipped++
			continue
		}

		if _, err := h.NetGoBGPPolicyApplyAdd(&item); err != nil {
			if tolerateExists && isIdempotentExists(err) {
				skipped++
				continue
			}
			return n, skipped, fmt.Errorf("apply BGP policy assignment %s/%s: %w", item.NeighIPAddress, item.PolicyType, err)
		}
		n++
	}
	return n, skipped, nil
}
func deleteBGPPolicyAssignment(h Hooks) (int, error) {
	items, err := h.NetGoBGPPolicyApplyGet()
	if isSubsystemUnavailable(err) {
		return 0, nil
	}
	if err != nil {
		return 0, err
	}
	n := 0
	var errs []error
	for _, item := range items {
		// Reset removes only operator policies and the default action;
		// the speaker-owned LB/HA policy names remain attached.
		if _, err := h.NetGoBGPPolicyApplyReset(&item); err != nil {
			errs = append(errs, fmt.Errorf("delete BGP policy assignment %s/%s: %w", item.NeighIPAddress, item.PolicyType, err))
			continue
		}
		n++
	}
	return n, errors.Join(errs...)
}
