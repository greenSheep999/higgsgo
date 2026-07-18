package register

import (
	"context"
	"fmt"
	"log/slog"
	"time"
)

const higgsFieldURL = "https://higgsfield.ai"

// Flow drives a single registration through the state machine.
type Flow struct {
	browser BrowserAutomator
	mailbox MailboxProvider
	captcha CaptchaSolver
	store   RegistrationStore
	cfg     Config
	log     *slog.Logger
}

func NewFlow(browser BrowserAutomator, mailbox MailboxProvider, captcha CaptchaSolver, store RegistrationStore, cfg Config, log *slog.Logger) *Flow {
	return &Flow{
		browser: browser,
		mailbox: mailbox,
		captcha: captcha,
		store:   store,
		cfg:     cfg,
		log:     log,
	}
}

// Execute runs the full registration flow for a single registration.
func (f *Flow) Execute(ctx context.Context, reg *Registration) error {
	if err := f.store.MarkRunning(ctx, reg.ID); err != nil {
		return fmt.Errorf("mark running: %w", err)
	}

	result, err := f.run(ctx, reg)
	if err != nil {
		_ = f.store.MarkFailed(ctx, reg.ID, err.Error())
		return err
	}

	if err := f.store.MarkCompleted(ctx, reg.ID, result); err != nil {
		return fmt.Errorf("mark completed: %w", err)
	}
	return nil
}

func (f *Flow) run(ctx context.Context, reg *Registration) (CompletedResult, error) {
	opts := LaunchOpts{
		ProxyURL: reg.ProxyURL,
		Headless: f.cfg.Browser.Headless,
	}

	session, err := f.browser.Launch(ctx, opts)
	if err != nil {
		return CompletedResult{}, fmt.Errorf("launch browser: %w", err)
	}
	defer session.Close()

	// Navigate to sign-up page
	if err := session.Goto(ctx, higgsFieldURL+"/sign-up"); err != nil {
		return CompletedResult{}, fmt.Errorf("navigate: %w", err)
	}

	// Click "Continue with Email"
	if err := session.Click(ctx, `button[data-action="email"]`); err != nil {
		return CompletedResult{}, fmt.Errorf("click email button: %w", err)
	}

	// Fill email
	if err := session.Fill(ctx, `input[name="email"]`, reg.Email); err != nil {
		return CompletedResult{}, fmt.Errorf("fill email: %w", err)
	}

	// Fill password
	if err := session.Fill(ctx, `input[name="password"]`, reg.Password); err != nil {
		return CompletedResult{}, fmt.Errorf("fill password: %w", err)
	}

	// Submit
	if err := session.Click(ctx, `button[type="submit"]`); err != nil {
		return CompletedResult{}, fmt.Errorf("click submit: %w", err)
	}

	// Wait for OTP challenge
	if err := session.WaitFor(ctx, `input[name="code"]`, 30*time.Second); err != nil {
		return CompletedResult{}, fmt.Errorf("wait for otp input: %w", err)
	}

	if err := f.store.MarkOTPWait(ctx, reg.ID); err != nil {
		return CompletedResult{}, fmt.Errorf("mark otp_wait: %w", err)
	}

	// Fetch OTP from mailbox
	otpStart := time.Now()
	otp, err := f.waitForOTP(ctx, reg.Email, otpStart)
	if err != nil {
		return CompletedResult{}, fmt.Errorf("fetch otp: %w", err)
	}

	// Fill OTP
	if err := session.Fill(ctx, `input[name="code"]`, otp.Code); err != nil {
		return CompletedResult{}, fmt.Errorf("fill otp: %w", err)
	}

	if err := session.Click(ctx, `button[type="submit"]`); err != nil {
		return CompletedResult{}, fmt.Errorf("submit otp: %w", err)
	}

	// Wait for successful redirect
	if err := session.WaitFor(ctx, `[data-testid="dashboard"]`, 30*time.Second); err != nil {
		return CompletedResult{}, fmt.Errorf("wait for dashboard: %w", err)
	}

	// Harvest credentials
	return f.harvest(ctx, session)
}

func (f *Flow) waitForOTP(ctx context.Context, email string, since time.Time) (OTPResult, error) {
	deadline := time.After(f.cfg.OTPTimeout)
	ticker := time.NewTicker(3 * time.Second)
	defer ticker.Stop()

	for {
		select {
		case <-ctx.Done():
			return OTPResult{}, ctx.Err()
		case <-deadline:
			return OTPResult{}, fmt.Errorf("otp timeout after %s", f.cfg.OTPTimeout)
		case <-ticker.C:
			result, err := f.mailbox.FetchOTP(ctx, email, since)
			if err != nil {
				f.log.Debug("otp fetch attempt failed", "email", email, "err", err)
				continue
			}
			if result.Code != "" {
				return result, nil
			}
		}
	}
}

func (f *Flow) harvest(ctx context.Context, session BrowserSession) (CompletedResult, error) {
	cookies, err := session.Cookies(ctx)
	if err != nil {
		return CompletedResult{}, fmt.Errorf("get cookies: %w", err)
	}

	ua, err := session.UserAgent(ctx)
	if err != nil {
		return CompletedResult{}, fmt.Errorf("get user agent: %w", err)
	}

	sessionToken := ""
	for _, c := range cookies {
		if c.Name == "__session" {
			sessionToken = c.Value
			break
		}
	}

	userID, _ := session.EvalJS(ctx, `window.__clerk?.user?.id || ""`)
	dataDome, _ := session.EvalJS(ctx, `document.cookie.match(/datadome=([^;]+)/)?.[1] || ""`)

	return CompletedResult{
		UserID:     userID,
		SessionID:  sessionToken,
		Cookies:    cookies,
		UserAgent:  ua,
		DataDomeID: dataDome,
	}, nil
}
