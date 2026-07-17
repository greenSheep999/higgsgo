package domain

import "time"

// AuditEvent is a single admin write-op record. One row is emitted per
// mutating HTTP request against the /admin/* surface (POST/PUT/PATCH/
// DELETE); GET/HEAD/OPTIONS are ignored to keep the audit signal high.
//
// The row shape mirrors the audit_events table added in migration 007.
// See internal/api/middleware/audit.go for how each field is derived
// from the incoming request.
type AuditEvent struct {
	// ID is a prefixed random id (idgen.NewID("audit")).
	ID string

	// TS is when the request finished, in UTC.
	TS time.Time

	// Actor identifies the bearer token used to authenticate the
	// request, truncated to 8 characters. Empty tokens surface as
	// "anonymous" — the middleware never persists a full bearer.
	Actor string

	// Method is the HTTP verb (POST / PUT / PATCH / DELETE).
	Method string

	// Path is the request URL path exactly as observed on the wire.
	Path string

	// Route is the chi RoutePattern (e.g. "/admin/keys/{id}"). Stable
	// under path-param variation so operators can group audit rows by
	// endpoint without regexing over Path.
	Route string

	// Status is the HTTP status code the handler eventually returned.
	Status int

	// ResourceType is derived from Route via a small lookup table:
	// apikey / account / group / job / partner. Empty when the route
	// is not in the lookup — the middleware still records the row.
	ResourceType string

	// ResourceID is the chi.URLParam value pulled from the matched
	// route (typically {id} or {partner_id}). Empty when the route
	// has no id in it (e.g. POST /admin/keys).
	ResourceID string

	// BodyHash is the SHA-256 hex of the request body. The body
	// itself is never stored — new keys, group configs, and other
	// admin payloads contain secrets we do not want to persist.
	BodyHash string

	// ErrorDetail is reserved for future non-2xx annotations; today
	// it is always empty.
	ErrorDetail string
}
