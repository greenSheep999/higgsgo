package v1

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"github.com/greensheep999/higgsgo/internal/domain"
	"github.com/greensheep999/higgsgo/internal/ports"
)

type fakePricingCatalogStore struct {
	ports.PricingStore
	snapshot *domain.PricingSnapshot
	rules    []domain.ModelCostRule
	err      error
}

func (f *fakePricingCatalogStore) LatestSnapshot(context.Context, string) (*domain.PricingSnapshot, error) {
	if f.err != nil {
		return nil, f.err
	}
	return f.snapshot, nil
}

func (f *fakePricingCatalogStore) ListLatestRules(context.Context, string) ([]domain.ModelCostRule, error) {
	if f.err != nil {
		return nil, f.err
	}
	return f.rules, nil
}

type fakePricingRegistry struct {
	ports.ModelRegistry
	models []*domain.ModelSpec
}

func (f *fakePricingRegistry) List(ports.ModelFilter) []*domain.ModelSpec { return f.models }

func TestPricingCatalog_UnavailableAndNotReady(t *testing.T) {
	for _, tc := range []struct {
		name    string
		handler *Handler
		errType string
	}{
		{name: "unwired", handler: &Handler{}, errType: "pricing_store_unavailable"},
		{name: "empty", handler: &Handler{Pricing: &fakePricingCatalogStore{}}, errType: "pricing_not_ready"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			rec := httptest.NewRecorder()
			tc.handler.HandlePricingCatalog(rec, httptest.NewRequest(http.MethodGet, "/v1/pricing", nil))
			if rec.Code != http.StatusServiceUnavailable {
				t.Fatalf("status = %d, want 503; body=%s", rec.Code, rec.Body.String())
			}
			var body struct {
				Error struct {
					Type string `json:"type"`
				} `json:"error"`
			}
			if err := json.Unmarshal(rec.Body.Bytes(), &body); err != nil {
				t.Fatalf("decode: %v", err)
			}
			if body.Error.Type != tc.errType {
				t.Fatalf("error type = %q, want %q", body.Error.Type, tc.errType)
			}
		})
	}
}

func TestPricingCatalog_ResponseAndAliasFilter(t *testing.T) {
	fetched := time.Date(2026, 7, 23, 9, 30, 0, 0, time.UTC)
	h := &Handler{
		Pricing: &fakePricingCatalogStore{
			snapshot: &domain.PricingSnapshot{
				ID: "price_1", Source: higgsPricingSource, SourceURL: "/job-sets/costs",
				PayloadSHA256: "abc", FetchedAt: fetched,
			},
			rules: []domain.ModelCostRule{
				{JST: "seedance_2_0", Unit: "per_second", Component: "cost_per_second", CreditsHundredths: 900, OriginalCreditsHundredths: 1200, Resolution: "1080p", DimensionsJSON: `{"resolution":"1080p"}`},
				{JST: "other_model", Unit: "per_request", CreditsHundredths: 125, DimensionsJSON: `{}`},
			},
		},
		Registry: &fakePricingRegistry{models: []*domain.ModelSpec{{
			Alias: "seedance-2", JST: "seedance_2_0", ExtraAliases: []string{"seedance-v2"},
		}}},
	}
	rec := httptest.NewRecorder()
	h.HandlePricingCatalog(rec, httptest.NewRequest(http.MethodGet, "/v1/pricing?model=seedance-v2", nil))
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200; body=%s", rec.Code, rec.Body.String())
	}
	var body struct {
		Object       string `json:"object"`
		PricingScope string `json:"pricing_scope"`
		SnapshotID   string `json:"snapshot_id"`
		FetchedAt    int64  `json:"fetched_at"`
		Data         []struct {
			JST               string         `json:"jst"`
			Aliases           []string       `json:"model_aliases"`
			Unit              string         `json:"unit"`
			Component         string         `json:"component"`
			Credits           float64        `json:"credits"`
			CreditsHundredths int64          `json:"credits_hundredths"`
			OriginalCredits   float64        `json:"original_credits"`
			Dimensions        map[string]any `json:"dimensions"`
		} `json:"data"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &body); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if body.Object != "pricing.catalog" || body.PricingScope != "upstream_credits_only" || body.SnapshotID != "price_1" {
		t.Fatalf("unexpected metadata: %+v", body)
	}
	if body.FetchedAt != fetched.Unix() {
		t.Fatalf("fetched_at = %d, want %d", body.FetchedAt, fetched.Unix())
	}
	if len(body.Data) != 1 {
		t.Fatalf("data len = %d, want 1; body=%s", len(body.Data), rec.Body.String())
	}
	item := body.Data[0]
	if item.JST != "seedance_2_0" || item.Unit != "per_second" || item.Component != "cost_per_second" ||
		item.Credits != 9 || item.CreditsHundredths != 900 || item.OriginalCredits != 12 {
		t.Fatalf("unexpected pricing item: %+v", item)
	}
	if len(item.Aliases) != 2 || item.Aliases[0] != "seedance-2" || item.Aliases[1] != "seedance-v2" {
		t.Fatalf("aliases = %v, want sorted aliases", item.Aliases)
	}
	if item.Dimensions["resolution"] != "1080p" {
		t.Fatalf("dimensions = %v", item.Dimensions)
	}
}

func TestPricingCatalog_ReadFailure(t *testing.T) {
	h := &Handler{Pricing: &fakePricingCatalogStore{err: errors.New("read failed")}}
	rec := httptest.NewRecorder()
	h.HandlePricingCatalog(rec, httptest.NewRequest(http.MethodGet, "/v1/pricing", nil))
	if rec.Code != http.StatusInternalServerError {
		t.Fatalf("status = %d, want 500; body=%s", rec.Code, rec.Body.String())
	}
}
