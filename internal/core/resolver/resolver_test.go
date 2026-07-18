package resolver

import "testing"

func globalDefaults() *GlobalConfig {
	return &GlobalConfig{
		MaxInFlightPerAccount: 5,
		ProxyURL:              "http://global-proxy:8080",
		DefaultRouteStrategy:  "round_robin",
		RateLimitRPS:          10,
		RateLimitBurst:        20,
	}
}

func TestResolve_AllZero_UsesGlobalDefaults(t *testing.T) {
	rc := Resolve(&KeyConfig{}, &GroupConfig{}, &AccountConfig{}, globalDefaults())

	if rc.MaxConcurrent != 5 {
		t.Errorf("MaxConcurrent = %d, want 5", rc.MaxConcurrent)
	}
	if rc.ProxyURL != "http://global-proxy:8080" {
		t.Errorf("ProxyURL = %q, want global proxy", rc.ProxyURL)
	}
	if rc.RouteStrategy != "round_robin" {
		t.Errorf("RouteStrategy = %q, want round_robin", rc.RouteStrategy)
	}
	if rc.MonthlyBudget != 0 {
		t.Errorf("MonthlyBudget = %d, want 0 (unlimited)", rc.MonthlyBudget)
	}
	if rc.AllowedModels != nil {
		t.Error("AllowedModels should be nil")
	}
	if rc.BlockedModels != nil {
		t.Error("BlockedModels should be nil")
	}
	if rc.RateLimitRPS != 10 {
		t.Errorf("RateLimitRPS = %d, want 10", rc.RateLimitRPS)
	}
	if rc.RateLimitBurst != 20 {
		t.Errorf("RateLimitBurst = %d, want 20", rc.RateLimitBurst)
	}
	if rc.MarkupPct != 0 {
		t.Errorf("MarkupPct = %d, want 0", rc.MarkupPct)
	}
}

func TestResolve_MostSpecificWins(t *testing.T) {
	key := &KeyConfig{
		MaxConcurrent:  2,
		RateLimitRPS:   100,
		RateLimitBurst: 200,
		MarkupPct:      50,
	}
	group := &GroupConfig{
		MaxConcurrentPerAccount: 3,
		RateLimitRPS:            50,
		RateLimitBurst:          100,
	}
	account := &AccountConfig{MaxConcurrent: 4}

	rc := Resolve(key, group, account, globalDefaults())

	if rc.MaxConcurrent != 2 {
		t.Errorf("MaxConcurrent = %d, want 2 (key)", rc.MaxConcurrent)
	}
	if rc.RateLimitRPS != 100 {
		t.Errorf("RateLimitRPS = %d, want 100 (key)", rc.RateLimitRPS)
	}
	if rc.RateLimitBurst != 200 {
		t.Errorf("RateLimitBurst = %d, want 200 (key)", rc.RateLimitBurst)
	}
	if rc.MarkupPct != 50 {
		t.Errorf("MarkupPct = %d, want 50 (key)", rc.MarkupPct)
	}
}

func TestResolve_MaxConcurrent_Cascade(t *testing.T) {
	// Key=0, Group.PerAccount=3 → should pick group
	rc := Resolve(
		&KeyConfig{},
		&GroupConfig{MaxConcurrentPerAccount: 3},
		&AccountConfig{MaxConcurrent: 4},
		globalDefaults(),
	)
	if rc.MaxConcurrent != 3 {
		t.Errorf("MaxConcurrent = %d, want 3 (group)", rc.MaxConcurrent)
	}

	// Key=0, Group=0, Account=4 → should pick account
	rc = Resolve(
		&KeyConfig{},
		&GroupConfig{},
		&AccountConfig{MaxConcurrent: 4},
		globalDefaults(),
	)
	if rc.MaxConcurrent != 4 {
		t.Errorf("MaxConcurrent = %d, want 4 (account)", rc.MaxConcurrent)
	}
}

func TestResolve_ProxyURL_AccountOverridesGlobal(t *testing.T) {
	rc := Resolve(
		&KeyConfig{},
		&GroupConfig{},
		&AccountConfig{BoundProxyURL: "socks5://sticky:1080"},
		globalDefaults(),
	)
	if rc.ProxyURL != "socks5://sticky:1080" {
		t.Errorf("ProxyURL = %q, want account proxy", rc.ProxyURL)
	}
}

func TestResolve_RouteStrategy_GroupOverridesGlobal(t *testing.T) {
	rc := Resolve(
		&KeyConfig{},
		&GroupConfig{RouteStrategy: "least_used"},
		&AccountConfig{},
		globalDefaults(),
	)
	if rc.RouteStrategy != "least_used" {
		t.Errorf("RouteStrategy = %q, want least_used", rc.RouteStrategy)
	}
}

func TestResolve_RouteStrategy_FallbackDefault(t *testing.T) {
	rc := Resolve(
		&KeyConfig{},
		&GroupConfig{},
		&AccountConfig{},
		&GlobalConfig{},
	)
	if rc.RouteStrategy != "round_robin" {
		t.Errorf("RouteStrategy = %q, want round_robin (hardcoded default)", rc.RouteStrategy)
	}
}

func TestResolve_MonthlyBudget_KeyLevel(t *testing.T) {
	rc := Resolve(
		&KeyConfig{MonthlyQuota: 50000, MonthlyUsed: 12000},
		&GroupConfig{MonthlyCreditBudget: 999999, MonthlyCreditUsed: 500000},
		&AccountConfig{},
		globalDefaults(),
	)
	if rc.MonthlyBudget != 50000 {
		t.Errorf("MonthlyBudget = %d, want 50000 (key)", rc.MonthlyBudget)
	}
	if rc.MonthlyUsed != 12000 {
		t.Errorf("MonthlyUsed = %d, want 12000 (key)", rc.MonthlyUsed)
	}
}

func TestResolve_MonthlyBudget_GroupFallback(t *testing.T) {
	rc := Resolve(
		&KeyConfig{MonthlyQuota: 0},
		&GroupConfig{MonthlyCreditBudget: 100000, MonthlyCreditUsed: 30000},
		&AccountConfig{},
		globalDefaults(),
	)
	if rc.MonthlyBudget != 100000 {
		t.Errorf("MonthlyBudget = %d, want 100000 (group)", rc.MonthlyBudget)
	}
	if rc.MonthlyUsed != 30000 {
		t.Errorf("MonthlyUsed = %d, want 30000 (group)", rc.MonthlyUsed)
	}
}

func TestResolve_MonthlyBudget_NeitherSet(t *testing.T) {
	rc := Resolve(&KeyConfig{}, &GroupConfig{}, &AccountConfig{}, globalDefaults())
	if rc.MonthlyBudget != 0 {
		t.Errorf("MonthlyBudget = %d, want 0 (unlimited)", rc.MonthlyBudget)
	}
}

func TestResolve_Regex_Valid(t *testing.T) {
	rc := Resolve(
		&KeyConfig{},
		&GroupConfig{
			AllowedModelsRegex: "^(wan21|kling).*",
			BlockedModelsRegex: "^veo3.*",
		},
		&AccountConfig{},
		globalDefaults(),
	)
	if rc.AllowedModels == nil {
		t.Fatal("AllowedModels should not be nil")
	}
	if !rc.AllowedModels.MatchString("wan21_t2v") {
		t.Error("AllowedModels should match wan21_t2v")
	}
	if rc.AllowedModels.MatchString("hailuo_t2v") {
		t.Error("AllowedModels should not match hailuo_t2v")
	}
	if rc.BlockedModels == nil {
		t.Fatal("BlockedModels should not be nil")
	}
	if !rc.BlockedModels.MatchString("veo3_standard") {
		t.Error("BlockedModels should match veo3_standard")
	}
}

func TestResolve_Regex_Invalid(t *testing.T) {
	rc := Resolve(
		&KeyConfig{},
		&GroupConfig{
			AllowedModelsRegex: "[invalid",
			BlockedModelsRegex: "(?P<bad",
		},
		&AccountConfig{},
		globalDefaults(),
	)
	if rc.AllowedModels != nil {
		t.Error("AllowedModels should be nil for invalid regex")
	}
	if rc.BlockedModels != nil {
		t.Error("BlockedModels should be nil for invalid regex")
	}
}

func TestResolve_Regex_Empty(t *testing.T) {
	rc := Resolve(
		&KeyConfig{},
		&GroupConfig{AllowedModelsRegex: "", BlockedModelsRegex: ""},
		&AccountConfig{},
		globalDefaults(),
	)
	if rc.AllowedModels != nil {
		t.Error("AllowedModels should be nil for empty pattern")
	}
	if rc.BlockedModels != nil {
		t.Error("BlockedModels should be nil for empty pattern")
	}
}

func TestResolve_RateLimit_GroupOverridesGlobal(t *testing.T) {
	rc := Resolve(
		&KeyConfig{},
		&GroupConfig{RateLimitRPS: 30, RateLimitBurst: 60},
		&AccountConfig{},
		globalDefaults(),
	)
	if rc.RateLimitRPS != 30 {
		t.Errorf("RateLimitRPS = %d, want 30 (group)", rc.RateLimitRPS)
	}
	if rc.RateLimitBurst != 60 {
		t.Errorf("RateLimitBurst = %d, want 60 (group)", rc.RateLimitBurst)
	}
}
