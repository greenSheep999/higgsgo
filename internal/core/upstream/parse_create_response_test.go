package upstream

import (
	"strings"
	"testing"
)

// TestParseCreateResponse_CostIntAndFloat is the regression test for
// the gpt-image production 500: higgsfield.ai sometimes returns
// `"cost": 50.0` (JSON number that is mathematically an integer). The
// pre-fix parser typed Cost as int64 and encoding/json refused to
// unmarshal a float into it, so every gpt-image create failed with an
// upstream-parse error even though the transaction was fine.
//
// The fix uses json.Number and coerces "50", "50.0", "50.00" into
// int64(50); non-integer floats and garbage return an error.
func TestParseCreateResponse_CostIntAndFloat(t *testing.T) {
	cases := []struct {
		name    string
		body    string
		wantCost int64
		wantErr  string // substring; empty means expect success
	}{
		{
			name:     "integer cost",
			body:     `{"id":"j1","job_sets":[{"id":"s1","cost":50,"jobs":[{"id":"job_1"}]}]}`,
			wantCost: 50,
		},
		{
			name:     "float-shaped whole number (regression)",
			body:     `{"id":"j1","job_sets":[{"id":"s1","cost":50.0,"jobs":[{"id":"job_1"}]}]}`,
			wantCost: 50,
		},
		{
			name:     "float-shaped whole with trailing zeros",
			body:     `{"id":"j1","job_sets":[{"id":"s1","cost":50.00,"jobs":[{"id":"job_1"}]}]}`,
			wantCost: 50,
		},
		{
			name:     "missing cost falls back to 0",
			body:     `{"id":"j1","job_sets":[{"id":"s1","jobs":[{"id":"job_1"}]}]}`,
			wantCost: 0,
		},
		{
			name:    "fractional cost rejected — silently rounding would hide upstream drift",
			body:    `{"id":"j1","job_sets":[{"id":"s1","cost":50.5,"jobs":[{"id":"job_1"}]}]}`,
			wantErr: "expected whole number",
		},
		{
			name:    "negative cost rejected",
			body:    `{"id":"j1","job_sets":[{"id":"s1","cost":-1.0,"jobs":[{"id":"job_1"}]}]}`,
			wantErr: "expected whole number",
		},
		{
			name:    "non-numeric cost is a parse error",
			body:    `{"id":"j1","job_sets":[{"id":"s1","cost":"lots","jobs":[{"id":"job_1"}]}]}`,
			wantErr: "parse create response",
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got, err := parseCreateResponse([]byte(tc.body), 200)
			if tc.wantErr != "" {
				if err == nil {
					t.Fatalf("want error containing %q, got success (cost=%d)", tc.wantErr, got.Cost)
				}
				if !strings.Contains(err.Error(), tc.wantErr) {
					t.Fatalf("want error containing %q, got %v", tc.wantErr, err)
				}
				return
			}
			if err != nil {
				t.Fatalf("unexpected error: %v", err)
			}
			if got.Cost != tc.wantCost {
				t.Errorf("cost: got %d want %d", got.Cost, tc.wantCost)
			}
		})
	}
}
