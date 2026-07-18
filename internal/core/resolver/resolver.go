package resolver

import "regexp"

type KeyConfig struct {
	MonthlyQuota   int64
	MonthlyUsed    int64
	MarkupPct      int
	MaxConcurrent  int
	RateLimitRPS   int
	RateLimitBurst int
	GroupID        string
}

type GroupConfig struct {
	MaxConcurrentJobs       int
	MaxConcurrentPerAccount int
	MonthlyCreditBudget     int64
	MonthlyCreditUsed       int64
	RouteStrategy           string
	AllowedModelsRegex      string
	BlockedModelsRegex      string
	RateLimitRPS            int
	RateLimitBurst          int
}

type AccountConfig struct {
	MaxConcurrent int
	BoundProxyURL string
	Priority      int
}

type GlobalConfig struct {
	MaxInFlightPerAccount int
	ProxyURL              string
	DefaultRouteStrategy  string
	RateLimitRPS          int
	RateLimitBurst        int
}

// Resolve computes the effective configuration for a request by cascading
// settings from most specific (API Key) to least specific (Global).
// Rule: first non-zero/non-empty value wins (most specific layer takes priority).
func Resolve(key *KeyConfig, group *GroupConfig, account *AccountConfig, global *GlobalConfig) ResolvedConfig {
	var rc ResolvedConfig

	// MaxConcurrent: Key > Group.PerAccount > Account > Global
	rc.MaxConcurrent = firstNonZero(
		key.MaxConcurrent,
		group.MaxConcurrentPerAccount,
		account.MaxConcurrent,
		global.MaxInFlightPerAccount,
	)

	// ProxyURL: Account > Global
	rc.ProxyURL = firstNonEmpty(account.BoundProxyURL, global.ProxyURL)

	// RouteStrategy: Group > Global
	rc.RouteStrategy = firstNonEmpty(group.RouteStrategy, global.DefaultRouteStrategy)
	if rc.RouteStrategy == "" {
		rc.RouteStrategy = "round_robin"
	}

	// MonthlyBudget: Key quota takes priority; fallback to Group budget.
	if key.MonthlyQuota > 0 {
		rc.MonthlyBudget = key.MonthlyQuota
		rc.MonthlyUsed = key.MonthlyUsed
	} else if group.MonthlyCreditBudget > 0 {
		rc.MonthlyBudget = group.MonthlyCreditBudget
		rc.MonthlyUsed = group.MonthlyCreditUsed
	}

	// AllowedModels / BlockedModels: Group level only.
	rc.AllowedModels = compileRegex(group.AllowedModelsRegex)
	rc.BlockedModels = compileRegex(group.BlockedModelsRegex)

	// RateLimitRPS: Key > Group > Global
	rc.RateLimitRPS = firstNonZero(key.RateLimitRPS, group.RateLimitRPS, global.RateLimitRPS)
	rc.RateLimitBurst = firstNonZero(key.RateLimitBurst, group.RateLimitBurst, global.RateLimitBurst)

	// MarkupPct: Key level only.
	rc.MarkupPct = key.MarkupPct

	return rc
}

func firstNonZero(vals ...int) int {
	for _, v := range vals {
		if v != 0 {
			return v
		}
	}
	return 0
}

func firstNonEmpty(vals ...string) string {
	for _, v := range vals {
		if v != "" {
			return v
		}
	}
	return ""
}

func compileRegex(pattern string) *regexp.Regexp {
	if pattern == "" {
		return nil
	}
	re, err := regexp.Compile(pattern)
	if err != nil {
		return nil
	}
	return re
}
