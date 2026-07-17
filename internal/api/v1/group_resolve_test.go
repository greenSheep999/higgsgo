// Package v1 tests for the api-key → group auto-resolution helper.
//
// The generation handlers (POST /v1/videos/generations,
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
func (f *fakeGroupStore) BindAPIKey(context.Context, string, string) error {
	panic("BindAPIKey not implemented")
}
func (f *fakeGroupStore) UnbindAPIKey(context.Context, string, string) error {
	panic("UnbindAPIKey not implemented")
}
func (f *fakeGroupStore) IncrementUsed(context.Context, string, int64) error {
	panic("IncrementUsed not implemented")
}
func (f *fakeGroupStore) CurrentInFlight(context.Context, string) (int, error) {
	panic("CurrentInFlight not implemented")
}

// Assert fakeGroupStore satisfies the interface at compile time.
var _ ports.GroupStore = (*fakeGroupStore)(nil)

// TestResolveGroup_SingleBinding: a one-group binding is applied silently.
func TestResolveGroup_SingleBinding(t *testing.T) {
	store := &fakeGroupStore{listResult: []domain.Group{{ID: "g1"}}}

	got, herr := resolveGroup(context.Background(), store, nil, "key_1", "")
	if herr != nil {
		t.Fatalf("unexpected httpError: %+v", herr)
	}
	if got != "g1" {
		t.Fatalf("resolved group = %q, want %q", got, "g1")
	}
	if store.listCalls != 1 {
		t.Fatalf("ListGroupsForAPIKey call count = %d, want 1", store.listCalls)
	}
	if store.lastAPIKeyID != "key_1" {
		t.Fatalf("api key forwarded to store = %q, want %q", store.lastAPIKeyID, "key_1")
	}
}

// TestResolveGroup_MultiBinding: multi-binding forces the caller to pick.
func TestResolveGroup_MultiBinding(t *testing.T) {
	store := &fakeGroupStore{listResult: []domain.Group{{ID: "g1"}, {ID: "g2"}}}

	got, herr := resolveGroup(context.Background(), store, nil, "key_1", "")
	if got != "" {
		t.Fatalf("resolved group = %q, want empty on ambiguity", got)
	}
	if herr == nil {
		t.Fatal("expected ambiguous_group httpError, got nil")
	}
	if herr.Status != 400 || herr.Kind != "ambiguous_group" {
		t.Fatalf("httpError = %+v, want 400/ambiguous_group", herr)
	}
	if herr.Message == "" {
		t.Fatal("httpError message must be non-empty for client guidance")
	}
}

// TestResolveGroup_NoBinding: unbound api key resolves to the empty scope.
func TestResolveGroup_NoBinding(t *testing.T) {
	store := &fakeGroupStore{listResult: nil}

	got, herr := resolveGroup(context.Background(), store, nil, "key_1", "")
	if herr != nil {
		t.Fatalf("unexpected httpError: %+v", herr)
	}
	if got != "" {
		t.Fatalf("resolved group = %q, want empty for zero bindings", got)
	}
	if store.listCalls != 1 {
		t.Fatalf("ListGroupsForAPIKey call count = %d, want 1", store.listCalls)
	}
}

// TestResolveGroup_ExplicitOverride: an explicit group_id short-circuits the
// store call so callers can always disambiguate deterministically.
func TestResolveGroup_ExplicitOverride(t *testing.T) {
	store := &fakeGroupStore{listResult: []domain.Group{{ID: "g1"}, {ID: "g2"}}}

	got, herr := resolveGroup(context.Background(), store, nil, "key_1", "gX")
	if herr != nil {
		t.Fatalf("unexpected httpError: %+v", herr)
	}
	if got != "gX" {
		t.Fatalf("resolved group = %q, want %q (explicit override)", got, "gX")
	}
	if store.listCalls != 0 {
		t.Fatalf("ListGroupsForAPIKey call count = %d, want 0 (store must not be consulted)", store.listCalls)
	}
}

// TestResolveGroup_GroupStoreError: a transient store failure must not block
// the request — we fall back to the empty group scope so the generation call
// can still proceed.
func TestResolveGroup_GroupStoreError(t *testing.T) {
	store := &fakeGroupStore{listErr: errors.New("boom")}

	got, herr := resolveGroup(context.Background(), store, nil, "key_1", "")
	if herr != nil {
		t.Fatalf("unexpected httpError: %+v", herr)
	}
	if got != "" {
		t.Fatalf("resolved group = %q, want empty on store failure", got)
	}
	if store.listCalls != 1 {
		t.Fatalf("ListGroupsForAPIKey call count = %d, want 1", store.listCalls)
	}
}

// TestResolveGroup_NilStore: a handler wired without a GroupStore must skip
// auto-resolution entirely — no panic, no error, empty group.
func TestResolveGroup_NilStore(t *testing.T) {
	got, herr := resolveGroup(context.Background(), nil, nil, "key_1", "")
	if herr != nil {
		t.Fatalf("unexpected httpError: %+v", herr)
	}
	if got != "" {
		t.Fatalf("resolved group = %q, want empty when store is nil", got)
	}
}

// TestResolveGroup_EmptyAPIKey: an unauthenticated (or misconfigured) caller
// with no api key must not trigger a store lookup. This mirrors the
// behaviour of /v1/models which is discoverable without a key.
func TestResolveGroup_EmptyAPIKey(t *testing.T) {
	store := &fakeGroupStore{listResult: []domain.Group{{ID: "g1"}}}

	got, herr := resolveGroup(context.Background(), store, nil, "", "")
	if herr != nil {
		t.Fatalf("unexpected httpError: %+v", herr)
	}
	if got != "" {
		t.Fatalf("resolved group = %q, want empty when api key is empty", got)
	}
	if store.listCalls != 0 {
		t.Fatalf("ListGroupsForAPIKey call count = %d, want 0", store.listCalls)
	}
}
