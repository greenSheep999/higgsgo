package register

import "time"

type Config struct {
	MaxConcurrent int
	PollInterval  time.Duration
	OTPTimeout    time.Duration
	RetryLimit    int
	Browser       BrowserConfig
}

type BrowserConfig struct {
	Type     string // "camoufox" | "cloak" | "mock"
	Headless bool
	PoolSize int
}

func DefaultConfig() Config {
	return Config{
		MaxConcurrent: 2,
		PollInterval:  5 * time.Second,
		OTPTimeout:    120 * time.Second,
		RetryLimit:    3,
		Browser: BrowserConfig{
			Type:     "camoufox",
			Headless: true,
			PoolSize: 2,
		},
	}
}
