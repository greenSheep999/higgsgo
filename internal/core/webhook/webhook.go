// Package webhook posts job-terminal notifications to caller-supplied HTTP
// URLs. Consumers register a callback URL via the /v1 request body
// (`callback_url` field). When the pollworker (or sync proxy) observes a
// job reach a terminal state, it hands the Job to Dispatcher.Fire, which
// asynchronously POSTs a signed JSON payload.
//
// This is deliberately fire-and-forget: the caller of Fire returns
// immediately and delivery happens on background goroutines with bounded
// concurrency and per-URL retry.
package webhook

import (
	"bytes"
	"context"
	"crypto/hmac"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"net/http"
	"sync"
	"time"

	"github.com/greensheep999/higgsgo/internal/domain"
)

// Dispatcher delivers terminal-state notifications to caller webhooks.
//
// Zero-value Dispatcher is unusable; call New to construct one.
type Dispatcher struct {
	client    *http.Client
	logger    *slog.Logger
	signKey   string
	timeout   time.Duration
	maxRetry  int
	backoff   time.Duration
	sem       chan struct{}
	wg        sync.WaitGroup
	closeOnce sync.Once
	// accepting is closed by Close() to reject further Fire calls but
	// leave in-flight deliveries running.
	accepting chan struct{}
	// closed is only closed when the caller-supplied shutdown context
	// expires while deliveries are still running; it aborts backoff
	// waits so Close returns promptly.
	closed chan struct{}
}

// Config controls Dispatcher construction.
type Config struct {
	// SigningKey is a shared secret used to sign each payload with HMAC-SHA256
	// (header X-Higgsgo-Signature). Consumers verify by re-computing HMAC of
	// the raw body with the same key. Empty disables signing.
	SigningKey string

	// Timeout per outbound HTTP call. Default 10s.
	Timeout time.Duration

	// MaxRetry is the max number of delivery attempts per URL, including the
	// first. Default 4 (≈1s + 5s + 25s + 125s of backoff between them).
	MaxRetry int

	// InitialBackoff is the first retry gap; doubles each attempt.
	// Default 1s.
	InitialBackoff time.Duration

	// Concurrency caps how many URLs may be in-flight at once. Default 16.
	Concurrency int
}

// New builds a Dispatcher with default HTTP client and provided config.
// Use Close to drain in-flight deliveries before shutdown.
func New(logger *slog.Logger, cfg Config) *Dispatcher {
	if cfg.Timeout == 0 {
		cfg.Timeout = 10 * time.Second
	}
	if cfg.MaxRetry == 0 {
		cfg.MaxRetry = 4
	}
	if cfg.InitialBackoff == 0 {
		cfg.InitialBackoff = 1 * time.Second
	}
	if cfg.Concurrency == 0 {
		cfg.Concurrency = 16
	}
	return &Dispatcher{
		client:    &http.Client{Timeout: cfg.Timeout},
		logger:    logger,
		signKey:   cfg.SigningKey,
		timeout:   cfg.Timeout,
		maxRetry:  cfg.MaxRetry,
		backoff:   cfg.InitialBackoff,
		sem:       make(chan struct{}, cfg.Concurrency),
		accepting: make(chan struct{}),
		closed:    make(chan struct{}),
	}
}

// Payload is the JSON body posted to a caller webhook.
type Payload struct {
	Event      string    `json:"event"` // "job.completed" | "job.failed" | "job.timeout" | "job.refunded"
	JobID      string    `json:"job_id"`
	APIKeyID   string    `json:"api_key_id,omitempty"`
	Model      string    `json:"model"`
	Status     string    `json:"status"`
	ResultURL  string    `json:"result_url,omitempty"`
	Refunded   bool      `json:"refunded,omitempty"`
	Cost       int64     `json:"cost,omitempty"`
	LatencyMS  int64     `json:"latency_ms,omitempty"`
	FinishedAt time.Time `json:"finished_at"`
	CreatedAt  time.Time `json:"created_at"`
	Error      string    `json:"error,omitempty"`
}

// Fire schedules delivery of a job's terminal state to the given URL.
// Non-blocking: returns immediately.
func (d *Dispatcher) Fire(url string, job *domain.Job) {
	if url == "" || job == nil {
		return
	}
	select {
	case <-d.accepting:
		return
	default:
	}
	payload := payloadFromJob(job)
	d.wg.Add(1)
	go func() {
		defer d.wg.Done()
		// Acquire an in-flight slot. We intentionally do NOT race this
		// against d.closed — once a caller has handed us a job, they
		// expect at least one delivery attempt. Backoff between retries
		// still short-circuits when closed to keep shutdown snappy.
		d.sem <- struct{}{}
		defer func() { <-d.sem }()
		d.deliver(url, payload)
	}()
}

// Close stops accepting new Fires and waits for in-flight deliveries to
// finish. When the shutdown context expires, backoff loops abort early
// so shutdown remains bounded.
func (d *Dispatcher) Close(shutdown context.Context) {
	d.closeOnce.Do(func() { close(d.accepting) })
	done := make(chan struct{})
	go func() { d.wg.Wait(); close(done) }()
	select {
	case <-done:
	case <-shutdown.Done():
		// Signal in-flight deliveries to abort remaining retries.
		close(d.closed)
	}
}

func (d *Dispatcher) deliver(url string, p Payload) {
	body, err := json.Marshal(p)
	if err != nil {
		d.logger.Warn("webhook marshal", slog.String("err", err.Error()))
		return
	}
	backoff := d.backoff
	for attempt := 1; attempt <= d.maxRetry; attempt++ {
		if err := d.postOnce(url, body); err == nil {
			d.logger.Info("webhook delivered",
				slog.String("url", url),
				slog.String("job_id", p.JobID),
				slog.Int("attempt", attempt))
			return
		} else {
			d.logger.Warn("webhook attempt failed",
				slog.String("url", url),
				slog.String("job_id", p.JobID),
				slog.Int("attempt", attempt),
				slog.String("err", err.Error()))
		}
		if attempt == d.maxRetry {
			break
		}
		select {
		case <-time.After(backoff):
			backoff *= 5
		case <-d.closed:
			return
		}
	}
}

func (d *Dispatcher) postOnce(url string, body []byte) error {
	ctx, cancel := context.WithTimeout(context.Background(), d.timeout)
	defer cancel()
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, url, bytes.NewReader(body))
	if err != nil {
		return err
	}
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("User-Agent", "higgsgo-webhook/1")
	if d.signKey != "" {
		req.Header.Set("X-Higgsgo-Signature", "sha256="+sign(body, d.signKey))
	}
	resp, err := d.client.Do(req)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	if resp.StatusCode >= 200 && resp.StatusCode < 300 {
		return nil
	}
	return fmt.Errorf("webhook HTTP %d", resp.StatusCode)
}

// payloadFromJob converts a domain.Job to a wire Payload.
func payloadFromJob(j *domain.Job) Payload {
	event := "job.completed"
	switch j.Status {
	case domain.JobFailed:
		event = "job.failed"
	case domain.JobTimeout:
		event = "job.timeout"
	case domain.JobRefunded:
		event = "job.refunded"
	}
	p := Payload{
		Event:      event,
		JobID:      j.ID,
		APIKeyID:   j.APIKeyID,
		Model:      j.ModelAlias,
		Status:     string(j.Status),
		ResultURL:  j.ResultURL,
		Refunded:   j.Refunded,
		Cost:       j.UpstreamCost,
		LatencyMS:  j.LatencyMS,
		FinishedAt: j.FinishedAt,
		CreatedAt:  j.RequestTS,
	}
	if j.ErrorDetail != "" {
		p.Error = j.ErrorDetail
	}
	return p
}

// sign returns the hex-encoded HMAC-SHA256 of body under key.
func sign(body []byte, key string) string {
	mac := hmac.New(sha256.New, []byte(key))
	_, _ = mac.Write(body)
	return hex.EncodeToString(mac.Sum(nil))
}

// ErrNoURL is returned by helpers when a request lacks callback_url.
var ErrNoURL = errors.New("webhook: no callback_url")
