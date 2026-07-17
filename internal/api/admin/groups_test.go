package admin

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"github.com/go-chi/chi/v5"
	"github.com/greensheep999/higgsgo/internal/domain"
	"github.com/greensheep999/higgsgo/internal/ports"
)

// fakeGroupStore is a partial ports.GroupStore for handler-level tests.
// Every method records its arguments so tests can assert both the routing
// (URL params reached the handler) and the response payload (view helper
// serialized correctly).
type fakeGroupStore struct {
	// Data.
	groups  map[string]*domain.Group
	members map[string][]string // group id → account ids
	binds   map[string][]string // group id → api key ids

	// Injectable errors.
	createErr error
	getErr    error
	listErr   error
	deleteErr error
	addErr    error
	rmErr     error
	bindErr   error
	unbindErr error

	// Call log.
	lastAddMember    struct{ GroupID, AccountID string }
	lastBindAPIKey   struct{ GroupID, APIKeyID string }
	lastRemoveMember struct{ GroupID, AccountID string }
}

func newFakeGroupStore() *fakeGroupStore {
	return &fakeGroupStore{
		groups:  map[string]*domain.Group{},
		members: map[string][]string{},
		binds:   map[string][]string{},
	}
}

func (f *fakeGroupStore) Get(_ context.Context, id string) (*domain.Group, error) {
	if f.getErr != nil {
		return nil, f.getErr
	}
	g, ok := f.groups[id]
	if !ok {
		return nil, domain.ErrGroupNotFound
	}
	return g, nil
}

func (f *fakeGroupStore) GetByName(_ context.Context, name string) (*domain.Group, error) {
	for _, g := range f.groups {
		if g.Name == name {
			return g, nil
		}
	}
	return nil, domain.ErrGroupNotFound
}

func (f *fakeGroupStore) Create(_ context.Context, g *domain.Group) error {
	if f.createErr != nil {
		return f.createErr
	}
	f.groups[g.ID] = g
	return nil
}

func (f *fakeGroupStore) Update(_ context.Context, g *domain.Group) error {
	if _, ok := f.groups[g.ID]; !ok {
		return domain.ErrGroupNotFound
	}
	f.groups[g.ID] = g
	return nil
}

func (f *fakeGroupStore) Delete(_ context.Context, id string) error {
	if f.deleteErr != nil {
		return f.deleteErr
	}
	if _, ok := f.groups[id]; !ok {
		return domain.ErrGroupNotFound
	}
	delete(f.groups, id)
	return nil
}

func (f *fakeGroupStore) List(_ context.Context) ([]domain.Group, error) {
	if f.listErr != nil {
		return nil, f.listErr
	}
	out := make([]domain.Group, 0, len(f.groups))
	for _, g := range f.groups {
		out = append(out, *g)
	}
	return out, nil
}

func (f *fakeGroupStore) AddMember(_ context.Context, groupID, accountID string, _ int) error {
	f.lastAddMember = struct{ GroupID, AccountID string }{groupID, accountID}
	if f.addErr != nil {
		return f.addErr
	}
	f.members[groupID] = append(f.members[groupID], accountID)
	return nil
}

func (f *fakeGroupStore) RemoveMember(_ context.Context, groupID, accountID string) error {
	f.lastRemoveMember = struct{ GroupID, AccountID string }{groupID, accountID}
	if f.rmErr != nil {
		return f.rmErr
	}
	kept := f.members[groupID][:0]
	for _, id := range f.members[groupID] {
		if id != accountID {
			kept = append(kept, id)
		}
	}
	f.members[groupID] = kept
	return nil
}

func (f *fakeGroupStore) ListMembers(_ context.Context, groupID string) ([]string, error) {
	return append([]string(nil), f.members[groupID]...), nil
}

func (f *fakeGroupStore) BindAPIKey(_ context.Context, apiKeyID, groupID string) error {
	f.lastBindAPIKey = struct{ GroupID, APIKeyID string }{groupID, apiKeyID}
	if f.bindErr != nil {
		return f.bindErr
	}
	f.binds[groupID] = append(f.binds[groupID], apiKeyID)
	return nil
}

func (f *fakeGroupStore) UnbindAPIKey(_ context.Context, apiKeyID, groupID string) error {
	if f.unbindErr != nil {
		return f.unbindErr
	}
	kept := f.binds[groupID][:0]
	for _, id := range f.binds[groupID] {
		if id != apiKeyID {
			kept = append(kept, id)
		}
	}
	f.binds[groupID] = kept
	return nil
}

func (f *fakeGroupStore) ListGroupsForAPIKey(_ context.Context, apiKeyID string) ([]domain.Group, error) {
	var out []domain.Group
	for gid, keys := range f.binds {
		for _, k := range keys {
			if k == apiKeyID {
				if g, ok := f.groups[gid]; ok {
					out = append(out, *g)
				}
			}
		}
	}
	return out, nil
}

func (f *fakeGroupStore) ListAPIKeys(_ context.Context, groupID string) ([]string, error) {
	return append([]string(nil), f.binds[groupID]...), nil
}

func (f *fakeGroupStore) IncrementUsed(_ context.Context, groupID string, delta int64) error {
	g, ok := f.groups[groupID]
	if !ok {
		return domain.ErrGroupNotFound
	}
	g.MonthlyCreditUsed += delta
	return nil
}

func (f *fakeGroupStore) CurrentInFlight(context.Context, string) (int, error) { return 0, nil }

// newGroupsRouter builds a chi.Router with the GroupsHandler mounted so
// tests exercise the same routing surface production uses.
func newGroupsRouter(store ports.GroupStore) chi.Router {
	r := chi.NewRouter()
	NewGroupsHandler(store).Register(r)
	return r
}

func TestGroupsHandler_Create(t *testing.T) {
	store := newFakeGroupStore()
	r := newGroupsRouter(store)

	body := `{"name":"payments-team","description":"stripe agents","monthly_credit_budget":50000,"route_strategy":"least_used"}`
	req := httptest.NewRequest(http.MethodPost, "/groups", bytes.NewBufferString(body))
	rec := httptest.NewRecorder()
	r.ServeHTTP(rec, req)

	if rec.Code != http.StatusCreated {
		t.Fatalf("status: got %d want 201; body=%s", rec.Code, rec.Body.String())
	}
	var view map[string]any
	if err := json.Unmarshal(rec.Body.Bytes(), &view); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if view["name"] != "payments-team" {
		t.Errorf("name: got %v want payments-team", view["name"])
	}
	if view["route_strategy"] != "least_used" {
		t.Errorf("route: got %v want least_used", view["route_strategy"])
	}
	// The handler assigns a grp_-prefixed id via idgen.
	if id, _ := view["id"].(string); len(id) < 4 || id[:4] != "grp_" {
		t.Errorf("id: got %v want grp_-prefixed", view["id"])
	}
	if len(store.groups) != 1 {
		t.Errorf("stored groups: got %d want 1", len(store.groups))
	}
}

func TestGroupsHandler_Create_MissingName(t *testing.T) {
	store := newFakeGroupStore()
	r := newGroupsRouter(store)

	req := httptest.NewRequest(http.MethodPost, "/groups", bytes.NewBufferString(`{}`))
	rec := httptest.NewRecorder()
	r.ServeHTTP(rec, req)

	if rec.Code != http.StatusBadRequest {
		t.Fatalf("status: got %d want 400; body=%s", rec.Code, rec.Body.String())
	}
}

func TestGroupsHandler_List(t *testing.T) {
	store := newFakeGroupStore()
	store.groups["grp_a"] = &domain.Group{
		ID: "grp_a", Name: "alpha", RouteStrategy: domain.RouteRoundRobin,
		OwnerType: domain.OwnerInternal, Status: "active",
		CreatedAt: time.Date(2026, 7, 17, 12, 0, 0, 0, time.UTC),
	}
	r := newGroupsRouter(store)

	req := httptest.NewRequest(http.MethodGet, "/groups", nil)
	rec := httptest.NewRecorder()
	r.ServeHTTP(rec, req)

	if rec.Code != http.StatusOK {
		t.Fatalf("status: got %d want 200", rec.Code)
	}
	var body map[string]any
	if err := json.Unmarshal(rec.Body.Bytes(), &body); err != nil {
		t.Fatalf("decode: %v", err)
	}
	data, ok := body["data"].([]any)
	if !ok {
		t.Fatalf("body.data missing or wrong type: %T", body["data"])
	}
	if len(data) != 1 {
		t.Fatalf("len: got %d want 1", len(data))
	}
}

func TestGroupsHandler_Get(t *testing.T) {
	store := newFakeGroupStore()
	store.groups["grp_a"] = &domain.Group{
		ID: "grp_a", Name: "alpha", RouteStrategy: domain.RouteRoundRobin,
		OwnerType: domain.OwnerInternal, Status: "active",
		CreatedAt: time.Date(2026, 7, 17, 12, 0, 0, 0, time.UTC),
	}
	r := newGroupsRouter(store)

	req := httptest.NewRequest(http.MethodGet, "/groups/grp_a", nil)
	rec := httptest.NewRecorder()
	r.ServeHTTP(rec, req)

	if rec.Code != http.StatusOK {
		t.Fatalf("status: got %d want 200", rec.Code)
	}
	var view map[string]any
	if err := json.Unmarshal(rec.Body.Bytes(), &view); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if view["id"] != "grp_a" {
		t.Errorf("id: got %v want grp_a", view["id"])
	}
}

func TestGroupsHandler_Get_NotFound(t *testing.T) {
	store := newFakeGroupStore()
	r := newGroupsRouter(store)

	req := httptest.NewRequest(http.MethodGet, "/groups/grp_missing", nil)
	rec := httptest.NewRecorder()
	r.ServeHTTP(rec, req)

	if rec.Code != http.StatusNotFound {
		t.Fatalf("status: got %d want 404", rec.Code)
	}
}

func TestGroupsHandler_AddMemberRoundTrip(t *testing.T) {
	store := newFakeGroupStore()
	store.groups["grp_a"] = &domain.Group{ID: "grp_a", Name: "alpha"}
	r := newGroupsRouter(store)

	req := httptest.NewRequest(http.MethodPost, "/groups/grp_a/members",
		bytes.NewBufferString(`{"account_id":"acc_1","priority":200}`))
	rec := httptest.NewRecorder()
	r.ServeHTTP(rec, req)

	if rec.Code != http.StatusOK {
		t.Fatalf("add status: got %d want 200; body=%s", rec.Code, rec.Body.String())
	}
	if store.lastAddMember.GroupID != "grp_a" || store.lastAddMember.AccountID != "acc_1" {
		t.Errorf("store.AddMember args: got %+v", store.lastAddMember)
	}

	// ListMembers should reflect the add.
	listReq := httptest.NewRequest(http.MethodGet, "/groups/grp_a/members", nil)
	listRec := httptest.NewRecorder()
	r.ServeHTTP(listRec, listReq)
	if listRec.Code != http.StatusOK {
		t.Fatalf("list status: got %d", listRec.Code)
	}
	var listBody map[string]any
	_ = json.Unmarshal(listRec.Body.Bytes(), &listBody)
	members, ok := listBody["members"].([]any)
	if !ok || len(members) != 1 || members[0] != "acc_1" {
		t.Errorf("members: got %v", listBody["members"])
	}

	// Round-trip: DELETE the member and confirm the store call.
	rmReq := httptest.NewRequest(http.MethodDelete, "/groups/grp_a/members/acc_1", nil)
	rmRec := httptest.NewRecorder()
	r.ServeHTTP(rmRec, rmReq)
	if rmRec.Code != http.StatusOK {
		t.Fatalf("remove status: got %d", rmRec.Code)
	}
	if store.lastRemoveMember.GroupID != "grp_a" || store.lastRemoveMember.AccountID != "acc_1" {
		t.Errorf("RemoveMember args: got %+v", store.lastRemoveMember)
	}
}

func TestGroupsHandler_BindAPIKeyRoundTrip(t *testing.T) {
	store := newFakeGroupStore()
	store.groups["grp_a"] = &domain.Group{ID: "grp_a", Name: "alpha"}
	r := newGroupsRouter(store)

	req := httptest.NewRequest(http.MethodPost, "/groups/grp_a/bindings",
		bytes.NewBufferString(`{"api_key_id":"key_xyz"}`))
	rec := httptest.NewRecorder()
	r.ServeHTTP(rec, req)

	if rec.Code != http.StatusOK {
		t.Fatalf("bind status: got %d want 200; body=%s", rec.Code, rec.Body.String())
	}
	if store.lastBindAPIKey.GroupID != "grp_a" || store.lastBindAPIKey.APIKeyID != "key_xyz" {
		t.Errorf("BindAPIKey args: got %+v", store.lastBindAPIKey)
	}

	unbindReq := httptest.NewRequest(http.MethodDelete, "/groups/grp_a/bindings/key_xyz", nil)
	unbindRec := httptest.NewRecorder()
	r.ServeHTTP(unbindRec, unbindReq)
	if unbindRec.Code != http.StatusOK {
		t.Fatalf("unbind status: got %d", unbindRec.Code)
	}
	// After unbind, the binding must be gone from the store.
	if got := store.binds["grp_a"]; len(got) != 0 {
		t.Errorf("binds after unbind: got %v want empty", got)
	}
}

func TestGroupsHandler_Delete_NotFound(t *testing.T) {
	store := newFakeGroupStore()
	r := newGroupsRouter(store)

	req := httptest.NewRequest(http.MethodDelete, "/groups/grp_missing", nil)
	rec := httptest.NewRecorder()
	r.ServeHTTP(rec, req)

	if rec.Code != http.StatusNotFound {
		t.Fatalf("status: got %d want 404", rec.Code)
	}
}

func TestGroupsHandler_Update(t *testing.T) {
	store := newFakeGroupStore()
	store.groups["grp_a"] = &domain.Group{
		ID: "grp_a", Name: "alpha", Description: "old desc",
		RouteStrategy: domain.RouteRoundRobin,
		OwnerType:     domain.OwnerInternal, Status: "active",
		CreatedAt: time.Date(2026, 7, 17, 12, 0, 0, 0, time.UTC),
	}
	r := newGroupsRouter(store)

	body := `{"name":"alpha-renamed","description":"new desc"}`
	req := httptest.NewRequest(http.MethodPut, "/groups/grp_a",
		bytes.NewBufferString(body))
	rec := httptest.NewRecorder()
	r.ServeHTTP(rec, req)

	if rec.Code != http.StatusOK {
		t.Fatalf("status: got %d want 200; body=%s", rec.Code, rec.Body.String())
	}
	var view map[string]any
	if err := json.Unmarshal(rec.Body.Bytes(), &view); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if view["name"] != "alpha-renamed" {
		t.Errorf("name: got %v want alpha-renamed", view["name"])
	}
	if view["description"] != "new desc" {
		t.Errorf("description: got %v want new desc", view["description"])
	}
	if got := store.groups["grp_a"].Name; got != "alpha-renamed" {
		t.Errorf("store name: got %q want alpha-renamed", got)
	}
}

func TestGroupsHandler_Update_NotFound(t *testing.T) {
	store := newFakeGroupStore()
	r := newGroupsRouter(store)

	req := httptest.NewRequest(http.MethodPut, "/groups/grp_missing",
		bytes.NewBufferString(`{"name":"x"}`))
	rec := httptest.NewRecorder()
	r.ServeHTTP(rec, req)

	if rec.Code != http.StatusNotFound {
		t.Fatalf("status: got %d want 404", rec.Code)
	}
}

func TestGroupsHandler_ListBindings(t *testing.T) {
	store := newFakeGroupStore()
	store.groups["grp_a"] = &domain.Group{ID: "grp_a", Name: "alpha"}
	r := newGroupsRouter(store)

	for _, k := range []string{"key_1", "key_2"} {
		bindReq := httptest.NewRequest(http.MethodPost, "/groups/grp_a/bindings",
			bytes.NewBufferString(`{"api_key_id":"`+k+`"}`))
		bindRec := httptest.NewRecorder()
		r.ServeHTTP(bindRec, bindReq)
		if bindRec.Code != http.StatusOK {
			t.Fatalf("bind %s status: got %d want 200; body=%s", k, bindRec.Code, bindRec.Body.String())
		}
	}

	req := httptest.NewRequest(http.MethodGet, "/groups/grp_a/bindings", nil)
	rec := httptest.NewRecorder()
	r.ServeHTTP(rec, req)

	if rec.Code != http.StatusOK {
		t.Fatalf("status: got %d want 200; body=%s", rec.Code, rec.Body.String())
	}
	var body map[string]any
	if err := json.Unmarshal(rec.Body.Bytes(), &body); err != nil {
		t.Fatalf("decode: %v", err)
	}
	data, ok := body["data"].([]any)
	if !ok {
		t.Fatalf("body.data missing or wrong type: %T", body["data"])
	}
	if len(data) != 2 {
		t.Fatalf("len: got %d want 2 (data=%v)", len(data), data)
	}
}

func TestGroupsHandler_AddMember_StorageError(t *testing.T) {
	store := newFakeGroupStore()
	store.groups["grp_a"] = &domain.Group{ID: "grp_a", Name: "alpha"}
	store.addErr = errors.New("boom")
	r := newGroupsRouter(store)

	req := httptest.NewRequest(http.MethodPost, "/groups/grp_a/members",
		bytes.NewBufferString(`{"account_id":"acc_1"}`))
	rec := httptest.NewRecorder()
	r.ServeHTTP(rec, req)

	if rec.Code != http.StatusInternalServerError {
		t.Fatalf("status: got %d want 500", rec.Code)
	}
}
