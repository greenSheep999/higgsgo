package jsonstatic

import (
	"encoding/json"
	"strings"
)

// refKind classifies the shape of a body-template catalogRef value. The
// registry loader dispatches on it to decide whether to look the ref up
// in the on-disk catalog map, use an inline literal set, or silently
// skip a ref the loader can't parse.
type refKind int

const (
	// refUnknown covers refs we cannot interpret — a missing catalog
	// file, a free-form annotation the resolver can't parse, or an
	// SPA-only artefact whose values only exist client-side. The
	// resolver skips these silently so a data-side annotation flourish
	// doesn't nuke enum enrichment for the surrounding template.
	refUnknown refKind = iota

	// refCatalog is a `catalogs/<file>.json` lookup, optionally paired
	// with a dot-path extractor (`... → item.some.field`) that walks
	// the parsed tree instead of using the flat id list.
	refCatalog

	// refLiteral is an inline `literal enum: a|b|c` (or `literal "x"`)
	// declaration — the allowed values live in the annotation itself
	// rather than an external catalog.
	refLiteral
)

// normalizeCatalogRef parses one catalogRef value from a body-template
// file and reports how the loader should resolve it.
//
// Recognised shapes, in the order they're tested:
//
//  1. `literal enum: a|b|c`  → refLiteral, literal = [a, b, c]
//  2. `literal "x"` or `literal "x" (…)`
//     → refLiteral, literal = ["x"]
//  3. `catalogs/foo.json → some.dot.path`  (also accepts ASCII `->`)
//     → refCatalog, path = "catalogs/foo.json", extractor = "some.dot.path"
//  4. `catalogs/foo.json (annotation)` — strip trailing whitespace/paren
//     → refCatalog, path = "catalogs/foo.json"
//  5. `catalogs/foo.json`  → refCatalog, path = same
//
// Anything else falls through as refUnknown so the caller can skip the
// ref without failing the whole registry reload. Callers are expected
// to trim whitespace on the input, but we do it defensively too.
func normalizeCatalogRef(raw string) (path string, extractor string, literal []string, kind refKind) {
	s := strings.TrimSpace(raw)
	if s == "" {
		return "", "", nil, refUnknown
	}

	// Literal enum: pipe-separated values after the "literal enum:" tag.
	if rest, ok := trimPrefixFold(s, "literal enum:"); ok {
		vals := splitLiteralValues(rest)
		if len(vals) == 0 {
			return "", "", nil, refUnknown
		}
		return "", "", vals, refLiteral
	}

	// Single literal: `literal "x"` — accept optional trailing annotation.
	if rest, ok := trimPrefixFold(s, "literal"); ok {
		v := extractQuoted(rest)
		if v == "" {
			return "", "", nil, refUnknown
		}
		return "", "", []string{v}, refLiteral
	}

	// A catalog reference must start with `catalogs/` — anything else
	// (e.g. `spa-only-presets.json → …`, `server/data/spa-only-presets.json`)
	// is an orphan we can't resolve on disk.
	if !strings.HasPrefix(s, "catalogs/") {
		return "", "", nil, refUnknown
	}

	// Split on the first arrow (U+2192 or ASCII `->`) — the LHS is the
	// catalog path, the RHS is the dot-path extractor.
	if lhs, rhs, ok := splitArrow(s); ok {
		p := stripAnnotation(lhs)
		e := strings.TrimSpace(rhs)
		if !strings.HasSuffix(p, ".json") {
			return "", "", nil, refUnknown
		}
		return p, e, nil, refCatalog
	}

	// Bare `catalogs/foo.json` or `catalogs/foo.json (annotation)`.
	p := stripAnnotation(s)
	if !strings.HasSuffix(p, ".json") {
		return "", "", nil, refUnknown
	}
	return p, "", nil, refCatalog
}

// trimPrefixFold is a case-insensitive TrimPrefix that also returns
// whether the prefix matched. Body-template authors have been
// inconsistent about capitalisation of "Literal" / "literal", and this
// avoids surprising skipped-ref bugs from a stray shift key.
func trimPrefixFold(s, prefix string) (string, bool) {
	if len(s) < len(prefix) {
		return s, false
	}
	if !strings.EqualFold(s[:len(prefix)], prefix) {
		return s, false
	}
	return s[len(prefix):], true
}

// splitArrow returns the LHS and RHS of the first arrow separator in s.
// Both U+2192 (`→`, the form the body-templates actually use) and ASCII
// `->` are accepted so future edits can use whichever renders better in
// the source file.
func splitArrow(s string) (lhs, rhs string, ok bool) {
	if i := strings.Index(s, "→"); i >= 0 {
		return s[:i], s[i+len("→"):], true
	}
	if i := strings.Index(s, "->"); i >= 0 {
		return s[:i], s[i+len("->"):], true
	}
	return "", "", false
}

// stripAnnotation trims trailing free-form context that follows the
// catalog path: `catalogs/foo.json (must be user-created)` collapses to
// `catalogs/foo.json`. Any run of whitespace or an opening paren marks
// the end of the machine-readable path.
func stripAnnotation(s string) string {
	s = strings.TrimSpace(s)
	if i := strings.IndexAny(s, " \t("); i >= 0 {
		s = s[:i]
	}
	return strings.TrimSpace(s)
}

// splitLiteralValues parses the RHS of `literal enum: a|b|c` — a
// pipe-separated list of raw tokens (optionally quoted). Empty entries
// are dropped so `a||b` doesn't produce a phantom "" enum value.
func splitLiteralValues(rest string) []string {
	// Some annotations put trailing prose after the enum, e.g.
	// `literal enum: a|b (comment)`. Strip anything past a parenthesis
	// so it doesn't slip into the last value.
	if i := strings.Index(rest, "("); i >= 0 {
		rest = rest[:i]
	}
	parts := strings.Split(rest, "|")
	out := make([]string, 0, len(parts))
	for _, p := range parts {
		v := strings.TrimSpace(p)
		v = strings.Trim(v, `"'`)
		if v == "" {
			continue
		}
		out = append(out, v)
	}
	return out
}

// extractQuoted returns the first quoted (single or double) substring
// in s, or the trimmed s itself if no quotes are present. Used for the
// single-value `literal "x"` shape.
func extractQuoted(s string) string {
	s = strings.TrimSpace(s)
	for _, q := range []string{`"`, `'`} {
		if a := strings.Index(s, q); a >= 0 {
			if b := strings.Index(s[a+1:], q); b >= 0 {
				return s[a+1 : a+1+b]
			}
		}
	}
	// No quotes — take the first whitespace-delimited token so a bare
	// `literal skin-enhancer` still resolves cleanly.
	if i := strings.IndexAny(s, " \t("); i >= 0 {
		s = s[:i]
	}
	return strings.TrimSpace(s)
}

// extractFromCatalogTree walks the parsed catalog document following a
// dot-separated path and returns every string it finds at the leaves.
// The path uses one convention drawn from the real data:
//
//	item.<field>[.<field>...]
//
// The literal `item` segment means "iterate the `items` array on the
// current node". This mirrors how the annotations read in
// data/reference/body-templates — e.g. `item.camera.id` = "for each
// element of items, read .camera.id".
//
// Segments other than `item` are treated as JSON object keys. Missing
// keys / wrong types cause the walker to skip that branch silently so a
// partially-populated catalog still produces the ids it can, rather
// than aborting the whole enum-enrichment pass.
func extractFromCatalogTree(tree json.RawMessage, path string) []string {
	path = strings.TrimSpace(path)
	if len(tree) == 0 || path == "" {
		return nil
	}
	segments := strings.Split(path, ".")

	// Decode lazily into interface{} so we can walk both arrays and
	// objects without an intermediate schema. The trees are small
	// (dozens of items per catalog) so the allocation overhead is fine.
	var root any
	if err := json.Unmarshal(tree, &root); err != nil {
		return nil
	}

	// Prime the walk: if the first segment is `item`, the resolver
	// expects to descend into the top-level `items` array first.
	nodes := []any{root}
	if segments[0] == "item" {
		nodes = expandItems(nodes)
		segments = segments[1:]
	}

	for _, seg := range segments {
		if seg == "" {
			continue
		}
		if seg == "item" {
			// A second `item` inside the path means another array
			// descent — support it for future nested catalogs.
			nodes = expandItems(nodes)
			continue
		}
		next := make([]any, 0, len(nodes))
		for _, n := range nodes {
			switch v := n.(type) {
			case map[string]any:
				if child, ok := v[seg]; ok {
					next = append(next, child)
				}
			case []any:
				// Silent array traversal: apply the key to every
				// element. Handles `foo.bar` where foo is an array
				// without an explicit `item.` prefix.
				for _, e := range v {
					if m, ok := e.(map[string]any); ok {
						if child, ok := m[seg]; ok {
							next = append(next, child)
						}
					}
				}
			}
		}
		nodes = next
	}

	out := make([]string, 0, len(nodes))
	for _, n := range nodes {
		if s, ok := n.(string); ok && s != "" {
			out = append(out, s)
		}
	}
	return out
}

// expandItems replaces every node with the contents of its `items`
// array (when present) or leaves it alone if the node is already an
// array. Non-matching nodes drop out — the resolver treats them as
// dead ends rather than an error, matching the "best-effort enum
// enrichment" contract described on the caller side.
func expandItems(nodes []any) []any {
	out := make([]any, 0, len(nodes))
	for _, n := range nodes {
		switch v := n.(type) {
		case map[string]any:
			if items, ok := v["items"]; ok {
				if arr, ok := items.([]any); ok {
					out = append(out, arr...)
				}
			}
		case []any:
			out = append(out, v...)
		}
	}
	return out
}
