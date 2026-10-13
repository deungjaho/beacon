package state

import "testing"

func TestRegistryStore_UpsertListRemove(t *testing.T) {
	dir := t.TempDir()
	reg, err := NewRegistryStore(dir)
	if err != nil {
		t.Fatal(err)
	}
	reg.SetNow(func() int64 { return 1000 })

	if err := reg.Upsert(ResourceRecord{Type: "tab", ID: "t1", OwnerPane: "%5", Producer: "argus", Metadata: map[string]any{"url": "http://x"}}); err != nil {
		t.Fatal(err)
	}
	recs := reg.List("tab", "", false)
	if len(recs) != 1 || recs[0].CreatedAt != 1000 || recs[0].LastSeen != 1000 {
		t.Fatalf("unexpected records: %+v", recs)
	}

	// Re-upsert keeps CreatedAt, refreshes LastSeen, clears stale.
	reg.SetNow(func() int64 { return 2000 })
	reg.MarkStaleForDeadPanes([]string{"%9"})
	if recs := reg.List("", "", true); len(recs) != 1 {
		t.Fatalf("expected 1 stale record, got %+v", recs)
	}
	if err := reg.Upsert(ResourceRecord{Type: "tab", ID: "t1", OwnerPane: "%5", Producer: "argus"}); err != nil {
		t.Fatal(err)
	}
	rec := reg.List("", "", false)[0]
	if rec.CreatedAt != 1000 || rec.LastSeen != 2000 || rec.Stale {
		t.Fatalf("upsert semantics broken: %+v", rec)
	}

	// Filters.
	if err := reg.Upsert(ResourceRecord{Type: "devserver", ID: "d1", OwnerPane: "%5", Producer: "devproxy"}); err != nil {
		t.Fatal(err)
	}
	if got := reg.List("tab", "", false); len(got) != 1 {
		t.Fatalf("type filter broken: %+v", got)
	}
	if got := reg.List("", "%5", false); len(got) != 2 {
		t.Fatalf("pane filter broken: %+v", got)
	}
	if got := reg.List("", "%6", false); len(got) != 0 {
		t.Fatalf("pane filter should exclude others: %+v", got)
	}

	if err := reg.Remove("tab", "t1"); err != nil {
		t.Fatal(err)
	}
	if got := reg.List("", "", false); len(got) != 1 {
		t.Fatalf("remove broken: %+v", got)
	}
}

func TestRegistryStore_MarkStaleKeepsLive(t *testing.T) {
	dir := t.TempDir()
	reg, _ := NewRegistryStore(dir)
	_ = reg.Upsert(ResourceRecord{Type: "tab", ID: "a", OwnerPane: "%1"})
	_ = reg.Upsert(ResourceRecord{Type: "tab", ID: "b", OwnerPane: "%2"})
	reg.MarkStaleForDeadPanes([]string{"%1"})
	recs := reg.List("", "", false)
	for _, r := range recs {
		if r.ID == "a" && r.Stale {
			t.Errorf("live pane resource marked stale")
		}
		if r.ID == "b" && !r.Stale {
			t.Errorf("dead pane resource not marked stale")
		}
	}
}
