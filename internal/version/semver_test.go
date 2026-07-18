package version

import "testing"

func TestCompare(t *testing.T) {
	cases := []struct {
		a, b string
		want int
	}{
		// Canonical triples.
		{"v0.2.0", "v0.2.1", -1},
		{"v0.2.1", "v0.2.0", 1},
		{"v0.2.0", "v0.2.0", 0},
		{"v1.0.0", "v0.9.9", 1},
		{"v0.10.0", "v0.2.9", 1}, // numeric compare, not lexical

		// Short forms — missing segments default to 0.
		{"v0.2", "v0.2.0", 0},
		{"v0.2", "v0.2.1", -1},
		{"v1", "v0.99.99", 1},

		// Case + prefix tolerance.
		{"V0.2.0", "v0.2.0", 0},
		{"0.2.0", "v0.2.0", 0},

		// Pre-release / build metadata is stripped for the numeric
		// compare (we don't care to rank rc.1 vs rc.2 here — the
		// release workflow only cuts plain vX.Y.Z tags).
		{"v0.3.0-rc.1", "v0.3.0", 0},
		{"v0.3.0+build.5", "v0.3.0", 0},

		// Junk inputs sink to 0.0.0.
		{"", "v0.0.1", -1},
		{"latest", "v0.0.1", -1},
		{"v0.0.0", "", 0},
	}
	for _, tc := range cases {
		t.Run(tc.a+"_vs_"+tc.b, func(t *testing.T) {
			if got := Compare(tc.a, tc.b); got != tc.want {
				t.Errorf("Compare(%q, %q) = %d; want %d", tc.a, tc.b, got, tc.want)
			}
		})
	}
}

func TestIsNewer(t *testing.T) {
	if !IsNewer("v0.3.1", "v0.2.0") {
		t.Error("IsNewer(v0.3.1, v0.2.0) = false; want true")
	}
	if IsNewer("v0.2.0", "v0.2.0") {
		t.Error("IsNewer(v0.2.0, v0.2.0) = true; want false")
	}
	if IsNewer("v0.1.9", "v0.2.0") {
		t.Error("IsNewer(v0.1.9, v0.2.0) = true; want false")
	}
}
