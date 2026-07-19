package register

import "time"

type RegistrationStatus string

const (
	StatusPending   RegistrationStatus = "pending"
	StatusRunning   RegistrationStatus = "running"
	StatusOTPWait   RegistrationStatus = "otp_wait"
	StatusVerifying RegistrationStatus = "verifying"
	StatusCompleted RegistrationStatus = "completed"
	StatusFailed    RegistrationStatus = "failed"
)

type Registration struct {
	ID          string
	Email       string
	Password    string
	Status      RegistrationStatus
	OAuthSource string
	ProxyURL    string
	Error       string
	AccountID   string
	CreatedAt   time.Time
	UpdatedAt   time.Time
	// MailboxClientID + MailboxRefreshToken pass through from the
	// main module's RegistrationStore so Flow.run can build a
	// Driver.RegisterRequest carrying per-row Graph OAuth2
	// credentials. See docs/ROADMAP.md §5.4 P4-3d.
	MailboxClientID     string
	MailboxRefreshToken string
}

type EnqueueRequest struct {
	Email       string
	Password    string
	OAuthSource string
	ProxyURL    string
}

type CompletedResult struct {
	AccountID  string
	UserID     string
	SessionID  string
	Cookies    []Cookie
	UserAgent  string
	DataDomeID string
	PlanType   string
	Credits    float64
}

type LaunchOpts struct {
	ProxyURL   string
	UserAgent  string
	Headless   bool
	Profile    string
	ExtraFlags []string
}

type Cookie struct {
	Name     string
	Value    string
	Domain   string
	Path     string
	Expires  time.Time
	Secure   bool
	HTTPOnly bool
}

type OTPResult struct {
	Code      string
	ExpiresAt time.Time
}

type ListFilter struct {
	Status *RegistrationStatus
	Limit  int
	Offset int
}
