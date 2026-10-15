package snapshot

import (
	"os"
	"testing"
)

// This tests the actual file codec and replacement engine for every registered
// domain, with a nonempty source fixture. The hooks are a model backend; Linux
// REST/effective-state qualification remains a separate test layer.
func TestEveryDomainSaveChangeRestore(t *testing.T) {
	h := newMockHooks()
	fixture := sampleDocument()
	fixture.Domains.LoadBalancer[0] = fullFieldLbRule()
	for _, entry := range ApplyOrder() {
		if _, _, err := entry.Apply(h, fixture, false); err != nil {
			t.Fatalf("fixture %s: %v", entry.Name, err)
		}
	}
	saved, err := Capture(h, "test", "test", TriggerManual, nil)
	if err != nil {
		t.Fatal(err)
	}
	dir := t.TempDir()
	path, _, _, err := Persist(saved, dir)
	if err != nil {
		t.Fatal(err)
	}
	raw, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	// Delete configuration after saving; restoring must materialize every
	// fixture rather than merely compare an unchanged source to itself.
	if _, err := Wipe(h, nil); err != nil {
		t.Fatal(err)
	}
	result, err := newTestEngine(h, t.TempDir()).Restore(raw, RestoreOptions{Mode: ModeCommit})
	if err != nil || result.Result != ResultOK {
		t.Fatalf("replacement: %+v %v", result, err)
	}
	actual, err := Capture(h, "test", "test", TriggerManual, nil)
	if err != nil {
		t.Fatal(err)
	}
	for _, entry := range ApplyOrder() {
		t.Run(entry.Name, func(t *testing.T) {
			wantN, wantD, err := DomainContent(entry.Name, &saved.Domains)
			if err != nil {
				t.Fatal(err)
			}
			if wantN == 0 {
				t.Fatalf("empty fixture for %s", entry.Name)
			}
			gotN, gotD, err := DomainContent(entry.Name, &actual.Domains)
			if err != nil {
				t.Fatal(err)
			}
			if wantN != gotN || wantD != gotD {
				t.Fatalf("lost %s after save/change/restore: %d/%s != %d/%s", entry.Name, wantN, wantD, gotN, gotD)
			}
		})
	}
}
