package models

import (
	"encoding/json"
	"testing"
)

func TestFcEffectiveNumericZerosAreObservable(t *testing.T) {
	absent := &LoadbalanceEntryServiceArguments{}
	wire, err := json.Marshal(absent)
	if err != nil {
		t.Fatal(err)
	}
	var fields map[string]json.RawMessage
	if err := json.Unmarshal(wire, &fields); err != nil {
		t.Fatal(err)
	}
	if _, exists := fields["fc_effective"]; exists {
		t.Fatal("unread dataplane pool must remain absent")
	}
	installed := &LoadbalanceEntryServiceArguments{FcEffective: &LoadbalanceEntryServiceArgumentsFcEffective{Mode: "off"}}
	wire, err = json.Marshal(installed)
	if err != nil {
		t.Fatal(err)
	}
	if err := json.Unmarshal(wire, &fields); err != nil {
		t.Fatal(err)
	}
	var effective map[string]json.RawMessage
	if err := json.Unmarshal(fields["fc_effective"], &effective); err != nil {
		t.Fatal(err)
	}
	for _, key := range []string{"max_outstanding", "ep_max_inflight", "prefill_max_inflight", "decode_max_inflight", "queue_depth", "queue_wait_ms", "inflight", "queued", "queue_memory_bound_mib", "telemetry_stale_ms", "warmup_ms", "ttft_target_ms", "effective_max_outstanding", "warming_endpoints", "tenant_max_share_pct", "tenants_active", "tenant_slots", "default_max_tokens", "reservation_clamped_burst", "reservation_clamped_budget", "reservation_epoch", "reservation_held_claims"} {
		value, exists := effective[key]
		if !exists || string(value) != "0" {
			t.Errorf("installed %s=%s present=%t, want explicit zero", key, value, exists)
		}
	}
	var decoded LoadbalanceEntryServiceArguments
	if err := json.Unmarshal(wire, &decoded); err != nil {
		t.Fatal(err)
	}
	if decoded.FcEffective == nil || decoded.FcEffective.Mode != "off" {
		t.Fatal("zero-valued installed state lost in round trip")
	}
}
