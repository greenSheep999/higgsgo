package resolver

import "regexp"

type ResolvedConfig struct {
	MaxConcurrent  int
	ProxyURL       string
	RouteStrategy  string
	MonthlyBudget  int64
	MonthlyUsed    int64
	AllowedModels  *regexp.Regexp
	BlockedModels  *regexp.Regexp
	RateLimitRPS   int
	RateLimitBurst int
	MarkupPct      int
}
