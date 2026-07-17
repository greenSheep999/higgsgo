package ports

import (
	"context"
	"time"
)

// MailboxProvider retrieves one-time passwords (OTPs) from an email inbox.
// higgsgo currently supports three flavors: Microsoft Graph (Outlook accounts
// with refresh_tokens), destiny-mmo (disposable inbox web viewer), and prompt
// (interactive stdin for development).
//
// A single higgsgo deployment usually wires up multiple providers behind a
// Chain adapter that routes based on the email domain.
type MailboxProvider interface {
	// FetchOTP blocks until the OTP arrives or ctx is canceled.
	// Returns the numeric code as a string (e.g., "482913").
	FetchOTP(ctx context.Context, req FetchOTPReq) (string, error)

	// ListInbox returns messages received after `since`. Used for password
	// reset flows and diagnostics.
	ListInbox(ctx context.Context, email string, since time.Time) ([]Email, error)

	// Supports reports whether this provider can handle the given email.
	// The Chain adapter uses this to route.
	Supports(email string) bool

	// Name identifies the provider for logs.
	Name() string
}

// FetchOTPReq carries the parameters required to fetch a single OTP.
type FetchOTPReq struct {
	Email       string
	Timeout     time.Duration
	Subject     string            // optional subject filter
	From        string            // optional sender filter
	Credentials map[string]string // provider-specific (refresh_token, password, ...)
}

// Email is a minimal representation of a fetched message.
type Email struct {
	MessageID string
	From      string
	Subject   string
	BodyText  string
	BodyHTML  string
	Received  time.Time
}
