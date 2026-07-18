package version

import (
	"strconv"
	"strings"
)

// Compare returns -1 if a < b, 0 if equal, +1 if a > b using the "vX.Y.Z"
// convention. Missing segments are treated as zero, so "v0.2" < "v0.2.1"
// and "v0.2" == "v0.2.0". A leading 'v' or 'V' is stripped; anything after
// the numeric triple (e.g. "-rc.1", "+meta") is dropped for the numeric
// compare — this is intentionally simpler than full SemVer 2.0.0 because
// higgsgo tags are plain "vX.Y.Z" per the release workflow.
//
// Non-numeric or empty inputs sort as 0.0.0 so a bogus GitHub payload
// (empty string, "latest", …) never accidentally shows as newer than a
// real tag.
func Compare(a, b string) int {
	pa := parse(a)
	pb := parse(b)
	for i := 0; i < 3; i++ {
		if pa[i] < pb[i] {
			return -1
		}
		if pa[i] > pb[i] {
			return 1
		}
	}
	return 0
}

// IsNewer is a convenience: reports whether latest > current under Compare.
func IsNewer(latest, current string) bool {
	return Compare(latest, current) > 0
}

func parse(s string) [3]int {
	var out [3]int
	if s == "" {
		return out
	}
	// Strip leading v/V.
	if s[0] == 'v' || s[0] == 'V' {
		s = s[1:]
	}
	// Trim off pre-release / build metadata suffixes; they do not
	// participate in the numeric compare.
	if i := strings.IndexAny(s, "-+"); i >= 0 {
		s = s[:i]
	}
	parts := strings.SplitN(s, ".", 4)
	for i := 0; i < 3 && i < len(parts); i++ {
		// strconv.Atoi returns 0 on error, which is the behaviour we
		// want: an unparseable segment sinks to zero and the tag
		// sorts below any well-formed one.
		n, _ := strconv.Atoi(parts[i])
		if n < 0 {
			n = 0
		}
		out[i] = n
	}
	return out
}
