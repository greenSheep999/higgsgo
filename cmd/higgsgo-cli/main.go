// higgsgo-cli — operations CLI.
//
// Subcommands:
//
//	list-accounts             print account pool summary
//	list-keys [-format]       list api keys (never leaks key_hash)
//	list-groups [-format]     list account groups
//	list-jobs [flags]         list jobs across the pool
//	show-usage [flags]        aggregate usage_events by model over a time window
//	disable-key <id>          mark an api key revoked
//	rotate-key <id>           mint a new secret, rotate the hash, print plaintext once
//	pause-key <id>            flip an api key to status=paused
//	resume-key <id>           flip a paused api key back to status=active
//	reset-usage <id>          zero the monthly_used counter for an api key
package main

import (
	"context"
	"crypto/rand"
	"crypto/sha256"
	"database/sql"
	"encoding/hex"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"io"
	"os"
	"strings"
	"time"

	"github.com/greensheep999/higgsgo/internal/adapters/storage/sqlite"
	"github.com/greensheep999/higgsgo/internal/config"
	"github.com/greensheep999/higgsgo/internal/domain"
	"github.com/greensheep999/higgsgo/internal/ports"
)

// apiKeySecretPrefix is the fixed shape used when minting a new plaintext
// secret. Kept in sync with internal/core/apikey.Prefix but duplicated here
// so the CLI does not pull in that package (and its rand-heavy Generate)
// simply to derive a prefix string.
const apiKeySecretPrefix = "sk-hg-"

func main() {
	if len(os.Args) < 2 {
		usage()
		os.Exit(2)
	}
	if err := dispatch(os.Args[1], os.Args[2:]); err != nil {
		fmt.Fprintf(os.Stderr, "higgsgo-cli: %v\n", err)
		os.Exit(1)
	}
}

// dispatch routes one subcommand invocation to its handler.
func dispatch(cmd string, args []string) error {
	switch cmd {
	case "list-accounts":
		return cmdListAccounts(args)
	case "list-keys":
		return cmdListKeys(args)
	case "list-groups":
		return cmdListGroups(args)
	case "list-jobs":
		return cmdListJobs(args)
	case "show-usage":
		return cmdShowUsage(args)
	case "disable-key":
		return cmdDisableKey(args)
	case "rotate-key":
		return cmdRotateKey(args)
	case "pause-key":
		return cmdPauseKey(args)
	case "resume-key":
		return cmdResumeKey(args)
	case "reset-usage":
		return cmdResetUsage(args)
	case "help", "-h", "--help":
		usage()
		return nil
	default:
		usage()
		return fmt.Errorf("unknown subcommand %q", cmd)
	}
}

func usage() {
	fmt.Fprintln(os.Stderr, "usage: higgsgo-cli <subcommand> [args]")
	fmt.Fprintln(os.Stderr, "  list-accounts [-config PATH]                     list account pool contents")
	fmt.Fprintln(os.Stderr, "  list-keys [-config PATH] [-format json|table]    list API keys (hash is never printed)")
	fmt.Fprintln(os.Stderr, "  list-groups [-config PATH] [-format json|table]  list account groups")
	fmt.Fprintln(os.Stderr, "  list-jobs [-config PATH] [-format ...] [-status S] [-account A] [-limit N]")
	fmt.Fprintln(os.Stderr, "  show-usage [-config PATH] [-format ...] [-hours N]")
	fmt.Fprintln(os.Stderr, "  disable-key [-config PATH] <api_key_id>          mark a key revoked")
	fmt.Fprintln(os.Stderr, "  rotate-key  [-config PATH] <api_key_id>          rotate a key and print the new secret once")
	fmt.Fprintln(os.Stderr, "  pause-key   [-config PATH] <api_key_id>          flip a key to status=paused")
	fmt.Fprintln(os.Stderr, "  resume-key  [-config PATH] <api_key_id>          flip a paused key back to status=active")
	fmt.Fprintln(os.Stderr, "  reset-usage [-config PATH] <api_key_id>          zero the monthly_used counter for a key")
}

// openDB reads the config file (or its default location) and opens the
// sqlite handle it points at. Every subcommand except plain -h/--help
// funnels through here.
func openDB(configPath string) (*sqlite.DB, error) {
	cfg, err := config.Load(configPath)
	if err != nil {
		return nil, fmt.Errorf("load config: %w", err)
	}
	if cfg.Storage.Driver != "sqlite" {
		return nil, fmt.Errorf("only sqlite storage supported in CLI right now")
	}
	db, err := sqlite.Open(context.Background(), cfg.Storage.SQLite.Path)
	if err != nil {
		return nil, fmt.Errorf("open sqlite: %w", err)
	}
	return db, nil
}

// newFlagSet returns a flag.FlagSet that writes its help output to stderr
// and always carries -config so every subcommand shares the same idiom.
func newFlagSet(name string) (*flag.FlagSet, *string) {
	fs := flag.NewFlagSet(name, flag.ContinueOnError)
	fs.SetOutput(os.Stderr)
	cfgPath := fs.String("config", "configs/higgsgo.example.toml", "path to higgsgo config toml")
	return fs, cfgPath
}

// ---------- list-keys ----------

// apiKeyRow is the CLI-visible projection of an api_keys row.
// key_hash is intentionally omitted from the type so it can never be
// serialized, even if a future change adds new output formats.
type apiKeyRow struct {
	ID           string  `json:"id"`
	Name         string  `json:"name"`
	Status       string  `json:"status"`
	CPAPartnerID string  `json:"cpa_partner_id,omitempty"`
	MonthlyQuota int64   `json:"monthly_quota"`
	MonthlyUsed  int64   `json:"monthly_used"`
	MarkupPct    float64 `json:"markup_pct"`
	CreatedAt    string  `json:"created_at,omitempty"`
	LastUsedAt   string  `json:"last_used_at,omitempty"`
}

func cmdListKeys(args []string) error {
	fs, cfgPath := newFlagSet("list-keys")
	format := fs.String("format", "table", "output format: table|json")
	if err := fs.Parse(args); err != nil {
		return err
	}
	db, err := openDB(*cfgPath)
	if err != nil {
		return err
	}
	defer db.Close()
	return listKeysQuery(context.Background(), os.Stdout, db.DB, *format)
}

// listKeysQuery reads the api_keys table via raw SQL (skipping the
// APIKeyStore so we never even scan key_hash into memory in the CLI path)
// and writes the projection to out. The raw *sql.DB handle keeps this
// callable from tests without paying the migration cost twice.
func listKeysQuery(ctx context.Context, out io.Writer, db *sql.DB, format string) error {
	rows, err := db.QueryContext(ctx, `
		SELECT id, name, cpa_partner_id, status,
		       monthly_quota, monthly_used, markup_pct,
		       created_at, last_used_at
		FROM api_keys
		ORDER BY created_at DESC`)
	if err != nil {
		return fmt.Errorf("query api_keys: %w", err)
	}
	defer rows.Close()

	var keys []apiKeyRow
	for rows.Next() {
		var (
			r          apiKeyRow
			partner    sql.NullString
			createdAt  sql.NullString
			lastUsedAt sql.NullString
		)
		if err := rows.Scan(
			&r.ID, &r.Name, &partner, &r.Status,
			&r.MonthlyQuota, &r.MonthlyUsed, &r.MarkupPct,
			&createdAt, &lastUsedAt,
		); err != nil {
			return err
		}
		r.CPAPartnerID = partner.String
		r.CreatedAt = createdAt.String
		r.LastUsedAt = lastUsedAt.String
		keys = append(keys, r)
	}
	if err := rows.Err(); err != nil {
		return err
	}
	return writeRows(out, format, keys, func(w io.Writer) {
		fmt.Fprintf(w, "%-24s %-24s %-10s %-16s %10s %10s %6s\n",
			"id", "name", "status", "cpa_partner", "monthly_used", "monthly_quota", "markup")
		fmt.Fprintln(w, strings.Repeat("-", 108))
		for _, r := range keys {
			fmt.Fprintf(w, "%-24s %-24s %-10s %-16s %10d %10d %6.2f\n",
				r.ID, r.Name, r.Status, r.CPAPartnerID, r.MonthlyUsed, r.MonthlyQuota, r.MarkupPct)
		}
	})
}

// ---------- list-groups ----------

// groupRow is the CLI-visible projection of an account_groups row.
type groupRow struct {
	ID               string `json:"id"`
	Name             string `json:"name"`
	Description      string `json:"description,omitempty"`
	OwnerType        string `json:"owner_type,omitempty"`
	OwnerID          string `json:"owner_id,omitempty"`
	Status           string `json:"status"`
	ConcurrencyLimit int    `json:"concurrency_limit"`
	MonthlyBudget    int64  `json:"monthly_credit_budget,omitempty"`
	MonthlyUsed      int64  `json:"monthly_credit_used"`
	RouteStrategy    string `json:"route_strategy,omitempty"`
}

func cmdListGroups(args []string) error {
	fs, cfgPath := newFlagSet("list-groups")
	format := fs.String("format", "table", "output format: table|json")
	if err := fs.Parse(args); err != nil {
		return err
	}
	db, err := openDB(*cfgPath)
	if err != nil {
		return err
	}
	defer db.Close()
	return listGroupsQuery(context.Background(), os.Stdout, db.DB, *format)
}

// listGroupsQuery reads account_groups directly via SQL so the CLI does
// not need to plumb a full GroupStore instance. Rows come back in the
// same order as GroupStore.List (name ASC) so operator scripts get a
// stable listing regardless of which code path fed them.
func listGroupsQuery(ctx context.Context, out io.Writer, db *sql.DB, format string) error {
	rows, err := db.QueryContext(ctx, `
		SELECT id, name, description,
		       max_concurrent_jobs,
		       monthly_credit_budget, monthly_credit_used,
		       route_strategy, owner_type, owner_id,
		       status
		FROM account_groups
		ORDER BY name ASC`)
	if err != nil {
		return fmt.Errorf("query account_groups: %w", err)
	}
	defer rows.Close()

	var groups []groupRow
	for rows.Next() {
		var (
			r           groupRow
			description sql.NullString
			maxConc     sql.NullInt64
			monthlyBudg sql.NullInt64
			ownerID     sql.NullString
		)
		if err := rows.Scan(
			&r.ID, &r.Name, &description,
			&maxConc,
			&monthlyBudg, &r.MonthlyUsed,
			&r.RouteStrategy, &r.OwnerType, &ownerID,
			&r.Status,
		); err != nil {
			return err
		}
		r.Description = description.String
		r.ConcurrencyLimit = int(maxConc.Int64)
		r.MonthlyBudget = monthlyBudg.Int64
		r.OwnerID = ownerID.String
		groups = append(groups, r)
	}
	if err := rows.Err(); err != nil {
		return err
	}
	return writeRows(out, format, groups, func(w io.Writer) {
		fmt.Fprintf(w, "%-20s %-20s %-10s %-14s %8s %12s %12s\n",
			"id", "name", "status", "owner_type", "conc", "budget", "used")
		fmt.Fprintln(w, strings.Repeat("-", 104))
		for _, r := range groups {
			fmt.Fprintf(w, "%-20s %-20s %-10s %-14s %8d %12d %12d\n",
				r.ID, r.Name, r.Status, r.OwnerType, r.ConcurrencyLimit, r.MonthlyBudget, r.MonthlyUsed)
		}
	})
}

// ---------- list-jobs ----------

// jobRow is the CLI-visible projection of a jobs row.
type jobRow struct {
	ID         string `json:"id"`
	AccountID  string `json:"account_id"`
	APIKeyID   string `json:"api_key_id,omitempty"`
	ModelAlias string `json:"model_alias"`
	JST        string `json:"jst"`
	Status     string `json:"status"`
	RequestTS  string `json:"request_ts"`
	LatencyMS  int64  `json:"latency_ms,omitempty"`
	Charged    int64  `json:"charged_credits_h,omitempty"`
}

func cmdListJobs(args []string) error {
	fs, cfgPath := newFlagSet("list-jobs")
	format := fs.String("format", "table", "output format: table|json")
	status := fs.String("status", "", "filter by job status (e.g. completed, failed)")
	account := fs.String("account", "", "filter by account_id")
	limit := fs.Int("limit", 50, "maximum rows to return")
	if err := fs.Parse(args); err != nil {
		return err
	}
	db, err := openDB(*cfgPath)
	if err != nil {
		return err
	}
	defer db.Close()
	store := sqlite.NewJobStore(db)
	return listJobsQuery(context.Background(), os.Stdout, store, *format, *status, *account, *limit)
}

// listJobsQuery composes a ports.JobFilter from the CLI flags and delegates
// to JobStore.ListAll so the exact filter semantics stay in one place.
func listJobsQuery(ctx context.Context, out io.Writer, store ports.JobStore, format, status, account string, limit int) error {
	filter := ports.JobFilter{
		Status:    domain.JobStatus(status),
		AccountID: account,
		Limit:     limit,
	}
	jobs, err := store.ListAll(ctx, filter)
	if err != nil {
		return fmt.Errorf("list jobs: %w", err)
	}
	rows := make([]jobRow, 0, len(jobs))
	for _, j := range jobs {
		rows = append(rows, jobRow{
			ID:         j.ID,
			AccountID:  j.AccountID,
			APIKeyID:   j.APIKeyID,
			ModelAlias: j.ModelAlias,
			JST:        j.JST,
			Status:     string(j.Status),
			RequestTS:  j.RequestTS.Format(time.RFC3339),
			LatencyMS:  j.LatencyMS,
			Charged:    j.ChargedCreditsHundredths,
		})
	}
	return writeRows(out, format, rows, func(w io.Writer) {
		fmt.Fprintf(w, "%-36s %-24s %-18s %-12s %-20s %8s\n",
			"id", "account", "model", "status", "request_ts", "lat_ms")
		fmt.Fprintln(w, strings.Repeat("-", 124))
		for _, r := range rows {
			fmt.Fprintf(w, "%-36s %-24s %-18s %-12s %-20s %8d\n",
				r.ID, r.AccountID, r.ModelAlias, r.Status, r.RequestTS, r.LatencyMS)
		}
	})
}

// ---------- show-usage ----------

// usageRow is one aggregate row grouped by model_alias.
type usageRow struct {
	Model      string `json:"model"`
	Count      int64  `json:"count"`
	Completed  int64  `json:"completed"`
	Failed     int64  `json:"failed"`
	TotalActH  int64  `json:"total_actual_h"`
	TotalCharg int64  `json:"total_charged_h"`
	AvgLatMS   int64  `json:"avg_latency_ms"`
}

func cmdShowUsage(args []string) error {
	fs, cfgPath := newFlagSet("show-usage")
	format := fs.String("format", "table", "output format: table|json")
	hours := fs.Int("hours", 24, "time window ending now, in hours")
	if err := fs.Parse(args); err != nil {
		return err
	}
	db, err := openDB(*cfgPath)
	if err != nil {
		return err
	}
	defer db.Close()
	store := sqlite.NewUsageEventStore(db)
	return showUsageQuery(context.Background(), os.Stdout, store, *format, *hours, time.Now().UTC())
}

// showUsageQuery groups usage_events by model_alias over the [now-hours, now)
// window and writes the result. `now` is a parameter so tests can pin the
// window against known event timestamps.
func showUsageQuery(ctx context.Context, out io.Writer, store ports.UsageEventStore, format string, hours int, now time.Time) error {
	if hours <= 0 {
		hours = 24
	}
	since := now.Add(-time.Duration(hours) * time.Hour)
	agg, err := store.Aggregate(ctx, ports.UsageAggQuery{
		Since:   since,
		Until:   now,
		GroupBy: []string{"model_alias"},
	})
	if err != nil {
		return fmt.Errorf("aggregate usage: %w", err)
	}
	rows := make([]usageRow, 0, len(agg))
	for _, r := range agg {
		rows = append(rows, usageRow{
			Model:      r.Keys["model_alias"],
			Count:      r.RequestCount,
			Completed:  r.CompletedCount,
			Failed:     r.FailedCount,
			TotalActH:  r.TotalCreditsHundredths,
			TotalCharg: r.ChargedCreditsHundredths,
			AvgLatMS:   r.AvgLatencyMS,
		})
	}
	return writeRows(out, format, rows, func(w io.Writer) {
		fmt.Fprintf(w, "%-24s %8s %8s %8s %12s %12s %10s\n",
			"model", "count", "compl", "failed", "actual_h", "charged_h", "avg_lat")
		fmt.Fprintln(w, strings.Repeat("-", 96))
		for _, r := range rows {
			fmt.Fprintf(w, "%-24s %8d %8d %8d %12d %12d %10d\n",
				r.Model, r.Count, r.Completed, r.Failed, r.TotalActH, r.TotalCharg, r.AvgLatMS)
		}
	})
}

// ---------- disable-key ----------

func cmdDisableKey(args []string) error {
	fs, cfgPath := newFlagSet("disable-key")
	if err := fs.Parse(args); err != nil {
		return err
	}
	if fs.NArg() < 1 {
		return errors.New("disable-key: missing <api_key_id>")
	}
	db, err := openDB(*cfgPath)
	if err != nil {
		return err
	}
	defer db.Close()
	store := sqlite.NewAPIKeyStore(db)
	return disableKeyExec(context.Background(), os.Stdout, store, fs.Arg(0))
}

// disableKeyExec revokes the given api key via APIKeyStore.Revoke and prints
// a one-line confirmation. domain.ErrAPIKeyNotFound is surfaced verbatim so
// callers (and tests) can distinguish "no such key" from other failures.
func disableKeyExec(ctx context.Context, out io.Writer, store ports.APIKeyStore, id string) error {
	if err := store.Revoke(ctx, id); err != nil {
		return err
	}
	fmt.Fprintf(out, "revoked api key %s\n", id)
	return nil
}

// ---------- rotate-key ----------

// rotateKeyResult is emitted on stdout after a successful rotation. The
// plaintext new_secret is shown here exactly once; callers must capture it
// immediately because only the hash is persisted.
type rotateKeyResult struct {
	ID        string `json:"id"`
	KeyPrefix string `json:"key_prefix"`
	NewSecret string `json:"new_secret"`
}

func cmdRotateKey(args []string) error {
	fs, cfgPath := newFlagSet("rotate-key")
	if err := fs.Parse(args); err != nil {
		return err
	}
	if fs.NArg() < 1 {
		return errors.New("rotate-key: missing <api_key_id>")
	}
	db, err := openDB(*cfgPath)
	if err != nil {
		return err
	}
	defer db.Close()
	return rotateKeyExec(context.Background(), os.Stdout, db.DB, fs.Arg(0))
}

// rotateKeyExec mints a fresh (plaintext, hash) pair for the api key with
// the given id, writes the new hash into the row, and emits the plaintext
// on stdout as JSON. Rotating a nonexistent id must return an error so the
// admin gets an explicit signal instead of a silent no-op.
func rotateKeyExec(ctx context.Context, out io.Writer, db *sql.DB, id string) error {
	// Confirm the row exists before minting so we don't waste a fresh
	// secret on a typo. Also lets us return domain.ErrAPIKeyNotFound
	// cleanly.
	var existing string
	if err := db.QueryRowContext(ctx, `SELECT id FROM api_keys WHERE id = ?`, id).Scan(&existing); err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			return domain.ErrAPIKeyNotFound
		}
		return fmt.Errorf("lookup api key %s: %w", id, err)
	}

	plaintext, hash, err := generateAPIKeyPair()
	if err != nil {
		return fmt.Errorf("mint api key: %w", err)
	}

	res, err := db.ExecContext(ctx,
		`UPDATE api_keys SET key_hash = ?, last_used_at = NULL WHERE id = ?`,
		hash, id)
	if err != nil {
		return fmt.Errorf("rotate api key %s: %w", id, err)
	}
	n, _ := res.RowsAffected()
	if n == 0 {
		// A concurrent delete between the check and the update.
		return domain.ErrAPIKeyNotFound
	}

	payload := rotateKeyResult{
		ID:        id,
		KeyPrefix: apiKeySecretPrefix,
		NewSecret: plaintext,
	}
	enc := json.NewEncoder(out)
	enc.SetIndent("", "  ")
	return enc.Encode(&payload)
}

// ---------- pause-key ----------

// pauseKeyResult is the JSON payload written on a successful pause. Keeping
// status in the payload (rather than a free-form message) mirrors the shape
// of resumeKeyResult / resetUsageResult so tooling can parse all three the
// same way.
type pauseKeyResult struct {
	ID     string `json:"id"`
	Status string `json:"status"`
}

func cmdPauseKey(args []string) error {
	fs, cfgPath := newFlagSet("pause-key")
	if err := fs.Parse(args); err != nil {
		return err
	}
	if fs.NArg() < 1 {
		return errors.New("pause-key: missing <api_key_id>")
	}
	db, err := openDB(*cfgPath)
	if err != nil {
		return err
	}
	defer db.Close()
	store := sqlite.NewAPIKeyStore(db)
	return pauseKeyExec(context.Background(), os.Stdout, store, fs.Arg(0))
}

// pauseKeyExec flips the api key with the given id to status=paused via
// APIKeyStore.Pause. domain.ErrAPIKeyRevoked / domain.ErrAPIKeyNotFound are
// returned verbatim so main() (and tests) can distinguish the two.
func pauseKeyExec(ctx context.Context, out io.Writer, store ports.APIKeyStore, id string) error {
	if err := store.Pause(ctx, id); err != nil {
		return err
	}
	enc := json.NewEncoder(out)
	enc.SetIndent("", "  ")
	return enc.Encode(&pauseKeyResult{ID: id, Status: "paused"})
}

// ---------- resume-key ----------

type resumeKeyResult struct {
	ID     string `json:"id"`
	Status string `json:"status"`
}

func cmdResumeKey(args []string) error {
	fs, cfgPath := newFlagSet("resume-key")
	if err := fs.Parse(args); err != nil {
		return err
	}
	if fs.NArg() < 1 {
		return errors.New("resume-key: missing <api_key_id>")
	}
	db, err := openDB(*cfgPath)
	if err != nil {
		return err
	}
	defer db.Close()
	store := sqlite.NewAPIKeyStore(db)
	return resumeKeyExec(context.Background(), os.Stdout, store, fs.Arg(0))
}

// resumeKeyExec flips the api key with the given id back to status=active
// via APIKeyStore.Resume. Attempting to resume a revoked key surfaces
// domain.ErrAPIKeyRevoked so the caller sees the terminal-state signal.
func resumeKeyExec(ctx context.Context, out io.Writer, store ports.APIKeyStore, id string) error {
	if err := store.Resume(ctx, id); err != nil {
		return err
	}
	enc := json.NewEncoder(out)
	enc.SetIndent("", "  ")
	return enc.Encode(&resumeKeyResult{ID: id, Status: "active"})
}

// ---------- reset-usage ----------

type resetUsageResult struct {
	ID          string `json:"id"`
	MonthlyUsed int64  `json:"monthly_used"`
}

func cmdResetUsage(args []string) error {
	fs, cfgPath := newFlagSet("reset-usage")
	if err := fs.Parse(args); err != nil {
		return err
	}
	if fs.NArg() < 1 {
		return errors.New("reset-usage: missing <api_key_id>")
	}
	db, err := openDB(*cfgPath)
	if err != nil {
		return err
	}
	defer db.Close()
	store := sqlite.NewAPIKeyStore(db)
	return resetUsageExec(context.Background(), os.Stdout, store, fs.Arg(0))
}

// resetUsageExec zeros the monthly_used counter for the given api key via
// APIKeyStore.ResetMonthlyUsage. The response echoes monthly_used=0 so a
// consumer can confirm the post-reset value without a follow-up Get.
func resetUsageExec(ctx context.Context, out io.Writer, store ports.APIKeyStore, id string) error {
	if err := store.ResetMonthlyUsage(ctx, id); err != nil {
		return err
	}
	enc := json.NewEncoder(out)
	enc.SetIndent("", "  ")
	return enc.Encode(&resetUsageResult{ID: id, MonthlyUsed: 0})
}

// generateAPIKeyPair mirrors internal/core/apikey.Generate without pulling
// that package into the CLI binary. Format is sk-hg-<40 hex chars>; only
// the SHA-256 hex digest is ever persisted.
func generateAPIKeyPair() (plaintext, hash string, err error) {
	var buf [20]byte
	if _, err := rand.Read(buf[:]); err != nil {
		return "", "", err
	}
	plaintext = apiKeySecretPrefix + hex.EncodeToString(buf[:])
	sum := sha256.Sum256([]byte(plaintext))
	hash = hex.EncodeToString(sum[:])
	return plaintext, hash, nil
}

// ---------- output helpers ----------

// writeRows renders the given rows either as pretty JSON or by invoking
// tableFn (which is expected to write a fixed-width table to out). Kept
// generic across all the list-* subcommands so their handler bodies stay
// short and their output formats stay in lockstep.
func writeRows(out io.Writer, format string, rows any, tableFn func(io.Writer)) error {
	f := strings.ToLower(strings.TrimSpace(format))
	if f == "" {
		f = "table"
	}
	if f == "json" {
		enc := json.NewEncoder(out)
		enc.SetIndent("", "  ")
		return enc.Encode(rows)
	}
	tableFn(out)
	return nil
}

// ---------- list-accounts (existing) ----------

func cmdListAccounts(args []string) error {
	fs, cfgPath := newFlagSet("list-accounts")
	if err := fs.Parse(args); err != nil {
		return err
	}
	db, err := openDB(*cfgPath)
	if err != nil {
		return err
	}
	defer db.Close()

	ctx := context.Background()
	rows, err := db.QueryContext(ctx, `
		SELECT id, email, plan_type, status, subscription_balance, in_flight_jobs, has_unlim, has_flex_unlim
		FROM accounts
		ORDER BY plan_type, subscription_balance DESC`)
	if err != nil {
		return err
	}
	defer rows.Close()

	fmt.Printf("%-40s %-40s %-9s %-10s %10s %8s %5s %5s\n",
		"id", "email", "plan", "status", "sub_bal", "in_flt", "unlm", "flex")
	fmt.Println("--------------------------------------------------------------------------------------------------------------------------")
	for rows.Next() {
		var (
			id, email, plan, status string
			subBal                  int64
			inFlt                   int
			hasUnlim, hasFlex       int
		)
		if err := rows.Scan(&id, &email, &plan, &status, &subBal, &inFlt, &hasUnlim, &hasFlex); err != nil {
			return err
		}
		fmt.Printf("%-40s %-40s %-9s %-10s %10d %8d %5d %5d\n", id, email, plan, status, subBal, inFlt, hasUnlim, hasFlex)
	}
	return rows.Err()
}
