// Package camoufox's driver_node.go spawns and talks to the Node
// higgsgo-register-driver subprocess. Follows the klinggo pattern
// (klinggo/session/browser_client.go): one Go type owns the child
// process lifetime, exposes a Register method that speaks HTTP over
// 127.0.0.1:<port>, and cleans up the whole process group on Close.
//
// See docs/PLUGGABLE.md §0 for why this bridge exists (registration
// automation must live outside the public reverse-proxy binary) and
// ROADMAP §5.4 P4-3b for the delivery plan.
//
// The Node driver is at plugins/register/driver-node/index.mjs. It
// wraps the higgsfield-register project's registerAccount() so this
// Go side never has to reimplement Playwright + camoufox + DataDome
// + Graph OTP.
package camoufox

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"log/slog"
	"net"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strconv"
	"strings"
	"syscall"
	"time"

	register "github.com/greensheep999/higgsgo/plugins/register"
)

// NodeDriver implements register.Driver by spawning the Node
// higgsgo-register-driver subprocess and forwarding Register() calls
// to its POST /register endpoint.
//
// Concurrency: the underlying Node driver serializes on a single
// browser context, so Register() is not safe to call from multiple
// goroutines. Callers (register.Worker) already sequence per-account
// registrations through a semaphore — if a future deployment needs
// parallelism, spawn multiple NodeDriver instances on different
// ports rather than trying to make one driver reentrant.
type NodeDriver struct {
	base   string
	hc     *http.Client
	cmd    *exec.Cmd
	ownCmd bool
	log    *slog.Logger

	// mailboxConfig, when set, is forwarded in every /register call
	// so the Node driver can complete OTP verification without a
	// callback into Go. Zero-value fields tell the driver to skip
	// mailbox setup (registration will time out at OTP wait unless
	// the caller has some other OTP mechanism).
	mailboxConfig MailboxConfig
}

// NodeDriverOptions controls how New() reaches or spawns the driver.
type NodeDriverOptions struct {
	// DriverURL is the base URL of a running driver
	// (e.g. "http://127.0.0.1:8801"). When empty, New() spawns a
	// child process from ScriptPath (or the default plugins/
	// register/driver-node/index.mjs).
	DriverURL string

	// ScriptPath is the absolute path to index.mjs. When empty, New()
	// resolves it via runtime.Caller() → module root.
	ScriptPath string

	// NodeBin is the interpreter used to launch the driver. Empty
	// means "node" on PATH. Env HIGGSGO_NODE_BIN overrides.
	NodeBin string

	// Port is the port to run the local driver on. 0 uses 8801.
	Port int

	// StartupTimeout is how long to wait for /ready to report ready.
	// 0 uses 30s.
	StartupTimeout time.Duration

	// Headless controls the browser mode. Defaults to true.
	// Operators wanting a headed session for debugging pass false.
	Headless bool

	// MailboxConfig is forwarded on every /register so the Node
	// driver can pull OTPs via Microsoft Graph. When ClientID or
	// RefreshToken is empty the driver will fail registration at
	// the OTP wait step — set both for production.
	MailboxConfig MailboxConfig

	// Logger is used for lifecycle events. Nil is slog.Default().
	Logger *slog.Logger
}

// MailboxConfig is the Microsoft Graph OAuth2 credentials the Node
// driver uses to fetch OTPs. Kept here rather than in ports.go so the
// (Node-specific) shape doesn't leak into the shared plugin
// interface.
type MailboxConfig struct {
	ClientID     string
	RefreshToken string
}

// New spawns (or connects to) the Node driver and blocks until /ready
// reports ready or the startup timeout expires. Returns an error on
// spawn failure or timeout; callers should propagate — the boot path
// treats registrar wiring as best-effort but the operator-facing
// "spawn failed" error is worth surfacing.
func New(ctx context.Context, opts NodeDriverOptions) (*NodeDriver, error) {
	if opts.Logger == nil {
		opts.Logger = slog.Default()
	}
	if opts.StartupTimeout == 0 {
		opts.StartupTimeout = 30 * time.Second
	}

	base := strings.TrimRight(opts.DriverURL, "/")
	var cmd *exec.Cmd
	if base == "" {
		port := opts.Port
		if port == 0 {
			port = 8801
		}
		// Bind check: refuse to spawn if something already holds
		// the port. Prevents the confusing "second driver crashed
		// with EADDRINUSE while first was fine" mode.
		if ln, err := net.Listen("tcp", fmt.Sprintf("127.0.0.1:%d", port)); err != nil {
			return nil, fmt.Errorf("driver port %d unavailable: %w", port, err)
		} else {
			_ = ln.Close()
		}

		nodeBin := opts.NodeBin
		if nodeBin == "" {
			nodeBin = os.Getenv("HIGGSGO_NODE_BIN")
		}
		if nodeBin == "" {
			nodeBin = "node"
		}
		script := opts.ScriptPath
		if script == "" {
			s, err := defaultScriptPath()
			if err != nil {
				return nil, err
			}
			script = s
		}
		nodeArgs := []string{script, "--port", strconv.Itoa(port)}
		if opts.Headless {
			nodeArgs = append(nodeArgs, "--headless")
		} else {
			nodeArgs = append(nodeArgs, "--headed")
		}
		cmd = exec.Command(nodeBin, nodeArgs...)
		// stdio streams get prefixed and forwarded so operators see
		// the Node logs alongside the Go logs. Errors on the child
		// surface immediately instead of getting lost.
		cmd.Stderr = prefixWriter{prefix: "[node-driver]"}
		cmd.Stdout = prefixWriter{prefix: "[node-driver]"}
		// Setpgid puts the child in its own process group so Close()
		// can kill the whole tree (node → Chromium/Firefox) with a
		// single kill(-pgid). Without this, the browser sub-processes
		// leak past a Go-side shutdown.
		cmd.SysProcAttr = &syscall.SysProcAttr{Setpgid: true}
		if err := cmd.Start(); err != nil {
			return nil, fmt.Errorf("spawn node driver: %w", err)
		}
		base = fmt.Sprintf("http://127.0.0.1:%d", port)
		opts.Logger.Info("node driver spawned",
			slog.String("script", script),
			slog.Int("port", port),
			slog.Int("pid", cmd.Process.Pid))
	}

	d := &NodeDriver{
		base:          base,
		hc:            &http.Client{Timeout: 5 * time.Minute},
		cmd:           cmd,
		ownCmd:        cmd != nil,
		log:           opts.Logger,
		mailboxConfig: opts.MailboxConfig,
	}

	// Wait for /ready. A short poll — the child is a Node process
	// that binds a socket in <200ms typically, but the first-run
	// import can take longer if flow.mjs pulls a big graph.
	deadline := time.Now().Add(opts.StartupTimeout)
	for time.Now().Before(deadline) {
		select {
		case <-ctx.Done():
			d.Close()
			return nil, ctx.Err()
		default:
		}
		if d.isReady(ctx) {
			opts.Logger.Info("node driver ready", slog.String("base", base))
			return d, nil
		}
		time.Sleep(500 * time.Millisecond)
	}
	d.Close()
	return nil, fmt.Errorf("node driver at %s did not become ready within %s", base, opts.StartupTimeout)
}

// Compile-time assertion so the register.Driver contract stays in
// sync with the plugin.
var _ register.Driver = (*NodeDriver)(nil)

// Name identifies the driver in logs / metrics.
func (d *NodeDriver) Name() string { return "camoufox-node" }

// Register runs one signup end-to-end via the Node driver. Returns
// a CompletedResult on success. Any non-2xx or protocol failure
// bubbles up as a Go error; a 200 with ok=false in the body maps to
// an error carrying the driver's reason.
func (d *NodeDriver) Register(ctx context.Context, req register.RegisterRequest) (register.CompletedResult, error) {
	payload := map[string]any{
		"email":        req.Email,
		"password":     req.Password,
		"oauth_source": req.OAuthSource,
		"proxy_url":    req.ProxyURL,
	}
	if d.mailboxConfig.ClientID != "" && d.mailboxConfig.RefreshToken != "" {
		payload["mailbox_config"] = map[string]string{
			"client_id":     d.mailboxConfig.ClientID,
			"refresh_token": d.mailboxConfig.RefreshToken,
		}
	}
	body, err := json.Marshal(payload)
	if err != nil {
		return register.CompletedResult{}, fmt.Errorf("driver: marshal request: %w", err)
	}
	httpReq, err := http.NewRequestWithContext(ctx, http.MethodPost, d.base+"/register", bytes.NewReader(body))
	if err != nil {
		return register.CompletedResult{}, fmt.Errorf("driver: build request: %w", err)
	}
	httpReq.Header.Set("Content-Type", "application/json")

	resp, err := d.hc.Do(httpReq)
	if err != nil {
		return register.CompletedResult{}, fmt.Errorf("driver: request: %w", err)
	}
	defer resp.Body.Close()
	raw, _ := io.ReadAll(resp.Body)

	// 503 driver_unavailable is the "flow.mjs not resolvable" path.
	// Distinct error so operators know to check the sibling repo
	// checkout rather than think the flow itself failed.
	if resp.StatusCode == http.StatusServiceUnavailable {
		return register.CompletedResult{}, fmt.Errorf("driver unavailable (%d): %s", resp.StatusCode, snip(raw))
	}
	if resp.StatusCode != http.StatusOK {
		return register.CompletedResult{}, fmt.Errorf("driver http %d: %s", resp.StatusCode, snip(raw))
	}

	var envelope struct {
		OK     bool           `json:"ok"`
		Error  string         `json:"error,omitempty"`
		Logs   []string       `json:"logs,omitempty"`
		Result map[string]any `json:"result,omitempty"`
	}
	if err := json.Unmarshal(raw, &envelope); err != nil {
		return register.CompletedResult{}, fmt.Errorf("driver: decode envelope: %w: %s", err, snip(raw))
	}
	if !envelope.OK {
		return register.CompletedResult{}, fmt.Errorf("driver flow: %s", envelope.Error)
	}
	return mapDriverResult(envelope.Result), nil
}

// Close terminates the child (if we spawned it) and reaps its
// process group. Best-effort graceful shutdown via POST /shutdown,
// then SIGKILL if the child is still alive after 3s.
func (d *NodeDriver) Close() {
	if !d.ownCmd || d.cmd == nil {
		return
	}
	req, _ := http.NewRequest(http.MethodPost, d.base+"/shutdown", bytes.NewReader([]byte(`{}`)))
	req.Header.Set("Content-Type", "application/json")
	if resp, err := d.hc.Do(req); err == nil && resp != nil {
		resp.Body.Close()
	}
	if d.cmd.Process != nil {
		if pgid, err := syscall.Getpgid(d.cmd.Process.Pid); err == nil {
			_ = syscall.Kill(-pgid, syscall.SIGKILL)
		} else {
			_ = d.cmd.Process.Kill()
		}
	}
	done := make(chan struct{})
	go func() {
		_ = d.cmd.Wait()
		close(done)
	}()
	select {
	case <-done:
	case <-time.After(3 * time.Second):
	}
}

func (d *NodeDriver) isReady(ctx context.Context) bool {
	req, _ := http.NewRequestWithContext(ctx, http.MethodGet, d.base+"/ready", nil)
	resp, err := d.hc.Do(req)
	if err != nil {
		return false
	}
	defer resp.Body.Close()
	if resp.StatusCode != 200 {
		return false
	}
	var out struct {
		Ready bool `json:"ready"`
	}
	_ = json.NewDecoder(resp.Body).Decode(&out)
	return out.Ready
}

// mapDriverResult converts the Node driver's untyped result payload
// into a register.CompletedResult. Every field is best-effort — a
// missing field lands as the zero value on the Go side rather than
// failing the whole registration.
func mapDriverResult(r map[string]any) register.CompletedResult {
	out := register.CompletedResult{
		AccountID:  strFromAny(r["account_id"]),
		UserID:     strFromAny(r["user_id"]),
		SessionID:  strFromAny(r["session_id"]),
		UserAgent:  strFromAny(r["user_agent"]),
		DataDomeID: strFromAny(r["datadome_id"]),
		PlanType:   strFromAny(r["plan_type"]),
		Credits:    floatFromAny(r["credits"]),
	}
	if cookiesRaw, ok := r["cookies"].([]any); ok {
		out.Cookies = make([]register.Cookie, 0, len(cookiesRaw))
		for _, c := range cookiesRaw {
			cm, ok := c.(map[string]any)
			if !ok {
				continue
			}
			out.Cookies = append(out.Cookies, register.Cookie{
				Name:     strFromAny(cm["name"]),
				Value:    strFromAny(cm["value"]),
				Domain:   strFromAny(cm["domain"]),
				Path:     strFromAny(cm["path"]),
				Secure:   boolFromAny(cm["secure"]),
				HTTPOnly: boolFromAny(cm["httpOnly"]),
			})
		}
	}
	return out
}

func strFromAny(v any) string {
	if s, ok := v.(string); ok {
		return s
	}
	return ""
}
func boolFromAny(v any) bool {
	if b, ok := v.(bool); ok {
		return b
	}
	return false
}
func floatFromAny(v any) float64 {
	if f, ok := v.(float64); ok {
		return f
	}
	return 0
}

// defaultScriptPath resolves plugins/register/driver-node/index.mjs
// relative to this source file. Robust regardless of cwd.
func defaultScriptPath() (string, error) {
	if env := os.Getenv("HIGGSGO_REGISTER_DRIVER_SCRIPT"); env != "" {
		if _, err := os.Stat(env); err == nil {
			return env, nil
		}
		return "", fmt.Errorf("HIGGSGO_REGISTER_DRIVER_SCRIPT=%q does not exist", env)
	}
	_, file, _, ok := runtime.Caller(0)
	if !ok {
		return "", fmt.Errorf("cannot resolve driver script path (runtime.Caller failed)")
	}
	// .../plugins/register/adapters/camoufox/driver_node.go
	//                         ^                    ^
	// go up to plugins/register/, into driver-node/index.mjs.
	root := filepath.Dir(filepath.Dir(filepath.Dir(file)))
	candidate := filepath.Join(root, "driver-node", "index.mjs")
	if _, err := os.Stat(candidate); err == nil {
		return candidate, nil
	}
	return "", fmt.Errorf("driver script not found at %s (set HIGGSGO_REGISTER_DRIVER_SCRIPT)", candidate)
}

// snip truncates a byte slice for use in error messages so a giant
// HTML error response doesn't blow the log.
func snip(b []byte) string {
	const max = 200
	if len(b) <= max {
		return string(b)
	}
	return string(b[:max]) + "…"
}

// prefixWriter tags every line the child writes so operator logs
// stay parseable.
type prefixWriter struct{ prefix string }

func (w prefixWriter) Write(p []byte) (int, error) {
	// Best-effort: don't split lines here — Node's console.error
	// already writes one line per call. Just prepend the prefix.
	fmt.Fprintf(os.Stderr, "%s %s", w.prefix, p)
	return len(p), nil
}
