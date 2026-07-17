package sqlite

// Tests for AuditStore: Insert, List filters (actor / resource / method),
// paging, and the default / cap Limit behaviour. All backed by an
// in-memory SQLite handle via openMem (see sqlite_test.go).

import (
	"context"
	"testing"
	"time"

	"github.com/greensheep999/higgsgo/internal/domain"
	"github.com/greensheep999/higgsgo/internal/ports"
)

// newAuditEvent returns an AuditEvent populated with sensible defaults so
// each test only needs to override the fields it cares about.
func newAuditEvent(id string, ts time.Time) *domain.AuditEvent {
	return &domain.AuditEvent{
		ID:           id,
		TS:           ts,
		Actor:        "sk-hg-ab",
		Method:       "POST",
		Path:         "/admin/keys",
		Route:        "/keys",
		Status:       201,
		ResourceType: "apikey",
		ResourceID:   "",
		BodyHash:     "deadbeef",
	}
}

func TestAuditStore_InsertList(t *testing.T) {
	db := openMem(t)
	store := NewAuditStore(db)
	ctx := context.Background()

	// Insert three rows spread across time so ORDER BY ts DESC has
	// something to shuffle.
	t1 := time.Date(2026, 7, 15, 9, 0, 0, 0, time.UTC)
	t2 := time.Date(2026, 7, 16, 10, 0, 0, 0, time.UTC)
	t3 := time.Date(2026, 7, 17, 11, 0, 0, 0, time.UTC)

	for _, e := range []*domain.AuditEvent{
		newAuditEvent("a1", t1),
		newAuditEvent("a2", t2),
		newAuditEvent("a3", t3),
	} {
		if err := store.Insert(ctx, e); err != nil {
			t.Fatalf("insert %s: %v", e.ID, err)
		}
	}

	got, err := store.List(ctx, ports.AuditFilter{})
	if err != nil {
		t.Fatalf("list: %v", err)
	}
	if len(got) != 3 {
		t.Fatalf("list len: got %d want 3", len(got))
	}
	if got[0].ID != "a3" || got[1].ID != "a2" || got[2].ID != "a1" {
		t.Errorf("list order: got %v want [a3 a2 a1]", auditIDList(got))
	}
	// TS round-trip: fmtTime → parseTime should preserve the seconds.
	if !got[0].TS.Equal(t3) {
		t.Errorf("ts round-trip: got %v want %v", got[0].TS, t3)
	}
}

func TestAuditStore_FilterByActor(t *testing.T) {
	db := openMem(t)
	store := NewAuditStore(db)
	ctx := context.Background()

	base := time.Date(2026, 7, 17, 12, 0, 0, 0, time.UTC)
	e1 := newAuditEvent("a1", base)
	e1.Actor = "sk-hg-aa"
	e2 := newAuditEvent("a2", base.Add(time.Minute))
	e2.Actor = "sk-hg-bb"
	e3 := newAuditEvent("a3", base.Add(2*time.Minute))
	e3.Actor = "sk-hg-aa"
	for _, e := range []*domain.AuditEvent{e1, e2, e3} {
		if err := store.Insert(ctx, e); err != nil {
			t.Fatalf("insert %s: %v", e.ID, err)
		}
	}

	got, err := store.List(ctx, ports.AuditFilter{Actor: "sk-hg-aa"})
	if err != nil {
		t.Fatalf("list by actor: %v", err)
	}
	if len(got) != 2 {
		t.Fatalf("actor filter: got %d want 2 (%v)", len(got), auditIDList(got))
	}
	if got[0].ID != "a3" || got[1].ID != "a1" {
		t.Errorf("actor filter order: got %v want [a3 a1]", auditIDList(got))
	}
}

func TestAuditStore_FilterByResource(t *testing.T) {
	db := openMem(t)
	store := NewAuditStore(db)
	ctx := context.Background()

	base := time.Date(2026, 7, 17, 12, 0, 0, 0, time.UTC)
	// Two apikey rows (one with matching id, one without), one job row.
	e1 := newAuditEvent("a1", base)
	e1.ResourceType = "apikey"
	e1.ResourceID = "key_target"
	e2 := newAuditEvent("a2", base.Add(time.Minute))
	e2.ResourceType = "apikey"
	e2.ResourceID = "key_other"
	e3 := newAuditEvent("a3", base.Add(2*time.Minute))
	e3.ResourceType = "job"
	e3.ResourceID = "key_target"
	for _, e := range []*domain.AuditEvent{e1, e2, e3} {
		if err := store.Insert(ctx, e); err != nil {
			t.Fatalf("insert %s: %v", e.ID, err)
		}
	}

	// Filter by resource_type alone.
	got, err := store.List(ctx, ports.AuditFilter{ResourceType: "apikey"})
	if err != nil {
		t.Fatalf("list by resource_type: %v", err)
	}
	if len(got) != 2 {
		t.Fatalf("resource_type filter: got %d want 2 (%v)", len(got), auditIDList(got))
	}

	// Filter by both should narrow to a single row.
	got, err = store.List(ctx, ports.AuditFilter{ResourceType: "apikey", ResourceID: "key_target"})
	if err != nil {
		t.Fatalf("list by resource_type+id: %v", err)
	}
	if len(got) != 1 || got[0].ID != "a1" {
		t.Errorf("resource_type+id filter: got %v want [a1]", auditIDList(got))
	}

	// Method filter is orthogonal — every seed row is POST, so a DELETE
	// filter should return nothing.
	got, err = store.List(ctx, ports.AuditFilter{Method: "DELETE"})
	if err != nil {
		t.Fatalf("list by method: %v", err)
	}
	if len(got) != 0 {
		t.Errorf("method=DELETE: got %v want []", auditIDList(got))
	}
}

func TestAuditStore_Pagination(t *testing.T) {
	db := openMem(t)
	store := NewAuditStore(db)
	ctx := context.Background()

	// Five rows so a Limit=2 page walks the full set.
	base := time.Date(2026, 7, 17, 12, 0, 0, 0, time.UTC)
	for i := 0; i < 5; i++ {
		e := newAuditEvent(auditSeq(i), base.Add(time.Duration(i)*time.Minute))
		if err := store.Insert(ctx, e); err != nil {
			t.Fatalf("insert %d: %v", i, err)
		}
	}

	page1, err := store.List(ctx, ports.AuditFilter{Limit: 2})
	if err != nil {
		t.Fatalf("page1: %v", err)
	}
	if len(page1) != 2 || page1[0].ID != "a4" || page1[1].ID != "a3" {
		t.Errorf("page1: got %v want [a4 a3]", auditIDList(page1))
	}

	page2, err := store.List(ctx, ports.AuditFilter{Limit: 2, Offset: 2})
	if err != nil {
		t.Fatalf("page2: %v", err)
	}
	if len(page2) != 2 || page2[0].ID != "a2" || page2[1].ID != "a1" {
		t.Errorf("page2: got %v want [a2 a1]", auditIDList(page2))
	}

	page3, err := store.List(ctx, ports.AuditFilter{Limit: 2, Offset: 4})
	if err != nil {
		t.Fatalf("page3: %v", err)
	}
	if len(page3) != 1 || page3[0].ID != "a0" {
		t.Errorf("page3: got %v want [a0]", auditIDList(page3))
	}

	// Excessive Limit is capped at maxAuditLimit; verify the cap does
	// not error out and simply returns everything we inserted.
	all, err := store.List(ctx, ports.AuditFilter{Limit: 100_000})
	if err != nil {
		t.Fatalf("list uncapped: %v", err)
	}
	if len(all) != 5 {
		t.Errorf("cap: got %d want 5", len(all))
	}
}

// auditSeq returns "a0"..."a9" so seed row IDs sort naturally.
func auditSeq(i int) string {
	return "a" + string(rune('0'+i))
}

// auditIDList extracts IDs for concise assertion messages.
func auditIDList(events []domain.AuditEvent) []string {
	out := make([]string, len(events))
	for i, e := range events {
		out[i] = e.ID
	}
	return out
}
