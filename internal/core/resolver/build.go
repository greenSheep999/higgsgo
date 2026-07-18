package resolver

import (
	"github.com/greensheep999/higgsgo/internal/config"
	"github.com/greensheep999/higgsgo/internal/domain"
)

// FromDomain builds the input configs from domain types.
// This is the bridge between domain structs and the resolver's input types.
func FromDomain(key *domain.APIKey, group *domain.Group, account *domain.Account, cfg *config.Config) (KeyConfig, GroupConfig, AccountConfig, GlobalConfig) {
	kc := KeyConfig{}
	if key != nil {
		kc.MonthlyQuota = key.MonthlyQuota
		kc.MonthlyUsed = key.MonthlyUsed
		kc.MarkupPct = int(key.MarkupPct * 100) // domain stores as float multiplier (1.5 = 150%)
		kc.GroupID = key.GroupID
		// TODO: wire Key.MaxConcurrent, Key.RateLimitRPS, Key.RateLimitBurst
		// once those fields are added to the domain.APIKey struct.
	}

	gc := GroupConfig{}
	if group != nil {
		gc.MaxConcurrentJobs = group.MaxConcurrentJobs
		gc.MaxConcurrentPerAccount = group.MaxConcurrentPerAccount
		gc.MonthlyCreditBudget = group.MonthlyCreditBudget
		gc.MonthlyCreditUsed = group.MonthlyCreditUsed
		gc.RouteStrategy = string(group.RouteStrategy)
		gc.AllowedModelsRegex = group.AllowedModelsRegex
		gc.BlockedModelsRegex = group.BlockedModelsRegex
		// TODO: wire Group.RateLimitRPS, Group.RateLimitBurst
		// once those fields are added to the domain.Group struct.
	}

	ac := AccountConfig{}
	if account != nil {
		ac.MaxConcurrent = account.MaxConcurrent
		ac.BoundProxyURL = account.BoundProxyURL
		ac.Priority = account.Priority
	}

	glc := GlobalConfig{}
	if cfg != nil {
		glc.MaxInFlightPerAccount = cfg.Pool.MaxInFlightPerAccount
		glc.ProxyURL = proxyURL(cfg)
		glc.DefaultRouteStrategy = "round_robin"
		glc.RateLimitRPS = int(cfg.Server.RateLimit.RPS)
		glc.RateLimitBurst = cfg.Server.RateLimit.Burst
	}

	return kc, gc, ac, glc
}

func proxyURL(cfg *config.Config) string {
	if len(cfg.Proxy.Static.URLs) > 0 {
		return cfg.Proxy.Static.URLs[0]
	}
	return ""
}
