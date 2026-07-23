package proxy

import (
	"testing"

	"github.com/greensheep999/higgsgo/internal/domain"
)

// TestBuildBody_UseUnlimToggle verifies the top-level use_unlim flag
// tracks the useUnlim argument. Generate resolves that argument from an
// account's active unlim window (see the routing block that pairs it with
// the /jobs/v2/{unlim_jst} endpoint switch); higgsfield rejects a request
// whose endpoint and use_unlim disagree, so this flag must never be
// hard-wired again.
func TestBuildBody_UseUnlimToggle(t *testing.T) {
	spec := &domain.ModelSpec{
		Alias:           "seedance-2-0",
		JST:             "seedance_2_0",
		Endpoint:        "/jobs/v2/seedance_2_0",
		UnlimJobSetType: "seedance_2_unlimited",
		ExampleBodyJSON: `{"params":{"prompt":"x"}}`,
	}
	req := GenerationRequest{Model: "seedance-2-0"}

	t.Run("unlim on", func(t *testing.T) {
		body := buildBody(spec, req, true)
		if body["use_unlim"] != true {
			t.Fatalf("use_unlim = %v, want true", body["use_unlim"])
		}
	})

	t.Run("unlim off", func(t *testing.T) {
		body := buildBody(spec, req, false)
		if body["use_unlim"] != false {
			t.Fatalf("use_unlim = %v, want false", body["use_unlim"])
		}
		// use_seedream_bonus is an unrelated hard default; confirm the
		// toggle did not disturb it.
		if body["use_seedream_bonus"] != false {
			t.Fatalf("use_seedream_bonus = %v, want false", body["use_seedream_bonus"])
		}
	})
}
