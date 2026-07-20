// Package v1 tests for the api-key → group auto-resolution helper.
//
// The generation handlers (POST /v1/video/generations,
// POST /v1/images/generations) both delegate group scoping to the
// resolveGroup helper. Rather than duplicate the same table-driven test in
// videos_test.go and images_test.go, we cover the resolution behaviour
// once here and let the handler-side test suite (if/when it grows) trust
// the helper's contract.
package v1

import (
	"context"
	"errors"
	"testing"

	"github.com/greensheep999/higgsgo/internal/domain"
	"github.com/greensheep999/higgsgo/internal/ports"
)

// fakeGroupStore is a stub ports.GroupStore that only implements the
// method resolveGroup exercises. Every unused method panics so a future
// refactor that starts relying on them shows up in test output.
type fakeGroupStore struct {
	// listResult is the value returned by ListGroupsForAPIKey.
	listResult []domain.Group
	// listErr, when non-nil, is returned by ListGroupsForAPIKey instead of listResult.
	listErr error
	// listCalls counts how many times ListGroupsForAPIKey was invoked.
	listCalls int
	// lastAPIKeyID records the argument to the most recent call.
	lastAPIKeyID string
}

func (f *fakeGroupStore) ListGroupsForAPIKey(_ context.Context, apiKeyID string) ([]domain.Group, error) {
	f.listCalls++
	f.lastAPIKeyID = apiKeyID
	if f.listErr != nil {
		return nil, f.listErr
	}
	return f.listResult, nil
}

// The remaining ports.GroupStore methods are unused by resolveGroup; they
// panic so an accidental future dependency is caught loudly.
func (f *fakeGroupStore) Get(context.Context, string) (*domain.Group, error) {
	panic("Get not implemented")
}
func (f *fakeGroupStore) GetByName(context.Context, string) (*domain.Group, error) {
	panic("GetByName not implemented")
}
func (f *fakeGroupStore) Create(context.Context, *domain.Group) error {
	panic("Create not implemented")
}
func (f *fakeGroupStore) Update(context.Context, *domain.Group) error {
	panic("Update not implemented")
}
func (f *fakeGroupStore) Delete(context.Context, string) error {
	panic("Delete not implemented")
}
func (f *fakeGroupStore) List(context.Context) ([]domain.Group, error) {
	panic("List not implemented")
}
func (f *fakeGroupStore) AddMember(context.Context, string, string, int) error {
	panic("AddMember not implemented")
}
func (f *fakeGroupStore) RemoveMember(context.Context, string, string) error {
	panic("RemoveMember not implemented")
}
func (f *fakeGroupStore) ListMembers(context.Context, string) ([]string, error) {
	panic("ListMembers not implemented")
}
func (f *fakeGroupStore) ListMembersWithPriority(context.Context, string) ([]ports.GroupMember, error) {
	panic("ListMembersWithPriority not implemented")
}
func (f *fakeGroupStore) BindAPIKey(context.Context, string, string) error {
	panic("BindAPIKey not implemented")
}
func (f *fakeGroupStore) UnbindAPIKey(context.Context, string, string) error {
	panic("UnbindAPIKey not implemented")
}
func (f *fakeGroupStore) ListAPIKeys(context.Context, string) ([]string, error) {
	panic("ListAPIKeys not implemented")
}
func (f *fakeGroupStore) IncrementUsed(context.Context, string, int64) error {
	panic("IncrementUsed not implemented")
}
func (f *fakeGroupStore) CurrentInFlight(context.Context, string) (int, error) {
	panic("CurrentInFlight not implemented")
}

// Assert fakeGroupStore satisfies the interface at compile time.
var _ ports.GroupStore = (*fakeGroupStore)(nil)

// keyRef is a small helper that returns a *domain.APIKey for the given id
// (and optional GroupID). It keeps the table-driven cases below readable.
func keyRef(id, groupID string) *domain.APIKey {
	return &domain.APIKey{ID: id, GroupID: groupID}
}

// singleGroup is a shorthand for asserting resolveGroup returned a
// one-element slice with the expected id. Removes the visual noise of
// checking len() and index [0] in every case.
func singleGroup(t *testing.T, got []string, want string) {
	t.Helper()
	if len(got) != 1 {
		t.Fatalf("resolved candidates = %v, want [%q]", got, want)
	}
	if got[0] != want {
		t.Fatalf("resolved candidate = %q, want %q", got[0], want)
	}
}

// TestResolveGroup_SingleBinding: a one-group binding is applied silently.
func TestResolveGroup_SingleBinding(t *testing.T) {
	store := &fakeGroupStore{listResult: []domain.Group{{ID: "g1"}}}

	got, herr := resolveGroup(context.Background(), store, nil, keyRef("key_1", ""), "")
	if herr != nil {
		t.Fatalf("unexpected httpError: %+v", herr)
	}
	singleGroup(t, got, "g1")
	if store.listCalls != 1 {
		t.Fatalf("ListGroupsForAPIKey call count = %d, want 1", store.listCalls)
	}
	if store.lastAPIKeyID != "key_1" {
		t.Fatalf("api key forwarded to store = %q, want %q", store.lastAPIKeyID, "key_1")
	}
}

// TestResolveGroup_MultiBinding: multi-binding now returns the ordered
// spillover list instead of an ambiguous_group error (ROADMAP P3-10).
// The order is stable sort by group name.
func TestResolveGroup_MultiBinding(t *testing.T) {
	store := &fakeGroupStore{listResult: []domain.Group{
		{ID: "g2", Name: "primary"},
		{ID: "g1", Name: "fallback"},
	}}

	got, herr := resolveGroup(context.Background(), store, nil, keyRef("key_1", ""), "")
	if herr != nil {
		t.Fatalf("unexpected httpError: %+v", herr)
	}
	// Sorted by name asc: "fallback" (g1) < "primary" (g2)
	if len(got) != 2 || got[0] != "g1" || got[1] != "g2" {
		t.Fatalf("resolved candidates = %v, want [g1, g2] (sorted by name)", got)
	}
}

// TestResolveGroup_NoBinding: unbound api key resolves to a single empty
// candidate — the "global pool" scope.
func TestResolveGroup_NoBinding(t *testing.T) {
	store := &fakeGroupStore{listResult: nil}

	got, herr := resolveGroup(context.Background(), store, nil, keyRef("key_1", ""), "")
	if herr != nil {
		t.Fatalf("unexpected httpError: %+v", herr)
	}
	singleGroup(t, got, "")
	if store.listCalls != 1 {
		t.Fatalf("ListGroupsForAPIKey call count = %d, want 1", store.listCalls)
	}
}

// TestResolveGroup_ExplicitOverride: an explicit group_id short-circuits the
// store call so callers can always disambiguate deterministically.
func TestResolveGroup_ExplicitOverride(t *testing.T) {
	store := &fakeGroupStore{listResult: []domain.Group{{ID: "g1"}, {ID: "g2"}}}

	got, herr := resolveGroup(context.Background(), store, nil, keyRef("key_1", ""), "gX")
	if herr != nil {
		t.Fatalf("unexpected httpError: %+v", herr)
	}
	singleGroup(t, got, "gX")
	if store.listCalls != 0 {
		t.Fatalf("ListGroupsForAPIKey call count = %d, want 0 (store must not be consulted)", store.listCalls)
	}
}

// TestResolveGroup_GroupStoreError: a transient store failure must not block
// the request — we fall back to the empty group scope so the generation call
// can still proceed.
func TestResolveGroup_GroupStoreError(t *testing.T) {
	store := &fakeGroupStore{listErr: errors.New("boom")}

	got, herr := resolveGroup(context.Background(), store, nil, keyRef("key_1", ""), "")
	if herr != nil {
		t.Fatalf("unexpected httpError: %+v", herr)
	}
	singleGroup(t, got, "")
	if store.listCalls != 1 {
		t.Fatalf("ListGroupsForAPIKey call count = %d, want 1", store.listCalls)
	}
}

// TestResolveGroup_NilStore: a handler wired without a GroupStore must skip
// auto-resolution entirely — no panic, no error, empty group.
func TestResolveGroup_NilStore(t *testing.T) {
	got, herr := resolveGroup(context.Background(), nil, nil, keyRef("key_1", ""), "")
	if herr != nil {
		t.Fatalf("unexpected httpError: %+v", herr)
	}
	singleGroup(t, got, "")
}

// TestResolveGroup_EmptyAPIKey: an unauthenticated (or misconfigured) caller
// with no api key must not trigger a store lookup. This mirrors the
// behaviour of /v1/models which is discoverable without a key.
func TestResolveGroup_EmptyAPIKey(t *testing.T) {
	store := &fakeGroupStore{listResult: []domain.Group{{ID: "g1"}}}

	got, herr := resolveGroup(context.Background(), store, nil, nil, "")
	if herr != nil {
		t.Fatalf("unexpected httpError: %+v", herr)
	}
	singleGroup(t, got, "")
	if store.listCalls != 0 {
		t.Fatalf("ListGroupsForAPIKey call count = %d, want 0", store.listCalls)
	}
}

// TestResolveGroup_DirectBindingWins: when the caller's APIKey carries a
// non-empty GroupID (migration 005 direct 1:1 binding), resolveGroup must
// return that value without consulting the M:N binding table.
func TestResolveGroup_DirectBindingWins(t *testing.T) {
	store := &fakeGroupStore{listResult: []domain.Group{{ID: "g_mn"}}}

	got, herr := resolveGroup(context.Background(), store, nil, keyRef("key_direct", "g_direct"), "")
	if herr != nil {
		t.Fatalf("unexpected httpError: %+v", herr)
	}
	singleGroup(t, got, "g_direct")
	if store.listCalls != 0 {
		t.Fatalf("ListGroupsForAPIKey call count = %d, want 0 (direct binding must short-circuit M:N lookup)", store.listCalls)
	}
}

// TestResolveGroup_DirectBindingPreemptsMulti: a direct 1:1 binding beats
// even a multi-binding M:N list, so the direct column stays the
// authoritative single-group override. Spillover only kicks in when the
// M:N table is the source of truth.
func TestResolveGroup_DirectBindingPreemptsMulti(t *testing.T) {
	store := &fakeGroupStore{listResult: []domain.Group{{ID: "g_a"}, {ID: "g_b"}}}

	got, herr := resolveGroup(context.Background(), store, nil, keyRef("key_mix", "g_direct"), "")
	if herr != nil {
		t.Fatalf("unexpected httpError: %+v", herr)
	}
	singleGroup(t, got, "g_direct")
	if store.listCalls != 0 {
		t.Fatalf("ListGroupsForAPIKey call count = %d, want 0", store.listCalls)
	}
}

// TestResolveGroup_ExplicitOverridesDirectBinding: an explicit `group_id`
// in the request body still beats the direct 1:1 binding. Tier 1 remains
// the caller's escape hatch even for direct-bound keys.
func TestResolveGroup_ExplicitOverridesDirectBinding(t *testing.T) {
	store := &fakeGroupStore{listResult: nil}

	got, herr := resolveGroup(context.Background(), store, nil, keyRef("key_direct", "g_direct"), "g_explicit")
	if herr != nil {
		t.Fatalf("unexpected httpError: %+v", herr)
	}
	singleGroup(t, got, "g_explicit")
	if store.listCalls != 0 {
		t.Fatalf("ListGroupsForAPIKey call count = %d, want 0", store.listCalls)
	}
}

// TestResolveGroup_FallsBackToBinding: when APIKey.GroupID is empty, the
// helper must still consult the M:N binding table. This documents that
// migration 005 does not remove the fallback path — both binding modes
// coexist.
func TestResolveGroup_FallsBackToBinding(t *testing.T) {
	store := &fakeGroupStore{listResult: []domain.Group{{ID: "g_from_mn"}}}

	got, herr := resolveGroup(context.Background(), store, nil, keyRef("key_mn", ""), "")
	if herr != nil {
		t.Fatalf("unexpected httpError: %+v", herr)
	}
	singleGroup(t, got, "g_from_mn")
	if store.listCalls != 1 {
		t.Fatalf("ListGroupsForAPIKey call count = %d, want 1", store.listCalls)
	}
}
