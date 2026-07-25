package main

import (
	"fmt"
	"sort"
	"time"
)

// ProviderConfig is the user-facing configuration for a single upstream provider.
// It is read from YAML or derived from legacy single-provider config.
type ProviderConfig struct {
	Name     string   `yaml:"name"`
	BaseURL  string   `yaml:"base_url"`
	APIKeys  []string `yaml:"api_keys"`
	Priority int      `yaml:"priority"` // lower = tried first
}

// Provider pairs a user-facing config with a KeyManager for its API keys.
type Provider struct {
	Config ProviderConfig
	Keys   *KeyManager
}

// ProviderRouter manages multiple providers, each with its own key pool.
// Providers are tried in priority order. When all keys in provider N are
// exhausted, the router falls through to provider N+1.
type ProviderRouter struct {
	providers []*Provider // sorted by priority, stable
}

// NewProviderRouter creates a router from provider configs and an optional
// global cooldown duration applied to every provider's KeyManager.
func NewProviderRouter(configs []ProviderConfig, cooldown time.Duration) (*ProviderRouter, error) {
	if len(configs) == 0 {
		return nil, fmt.Errorf("at least one provider is required")
	}
	providers := make([]*Provider, 0, len(configs))
	for _, cfg := range configs {
		if len(cfg.APIKeys) == 0 {
			return nil, fmt.Errorf("provider %q: at least one api_key is required", cfg.Name)
		}
		if cfg.BaseURL == "" {
			return nil, fmt.Errorf("provider %q: base_url is required", cfg.Name)
		}
		providers = append(providers, &Provider{
			Config: cfg,
			Keys:   NewKeyManager(cfg.APIKeys, cooldown),
		})
	}
	// Sort stable by priority; break ties by original slice order.
	sort.SliceStable(providers, func(i, j int) bool {
		return providers[i].Config.Priority < providers[j].Config.Priority
	})
	return &ProviderRouter{providers: providers}, nil
}

// Current returns the first provider that has an eligible key, along with that
// key's index and value. ok is false when every provider is fully exhausted.
func (r *ProviderRouter) Current() (prov *Provider, keyIdx int, key string, ok bool) {
	for _, p := range r.providers {
		idx, k, hasKey := p.Keys.Current()
		if hasKey {
			return p, idx, k, true
		}
	}
	return nil, 0, "", false
}

// MarkExhausted marks the given key index as exhausted on the specified provider.
func (r *ProviderRouter) MarkExhausted(prov *Provider, keyIdx int) {
	prov.Keys.MarkExhausted(keyIdx)
}

// MarkAvailable marks the given key index as available on the specified provider.
func (r *ProviderRouter) MarkAvailable(prov *Provider, keyIdx int) {
	prov.Keys.MarkAvailable(keyIdx)
}

// AllExhausted returns true when every key in every provider is exhausted.
func (r *ProviderRouter) AllExhausted() bool {
	for _, p := range r.providers {
		if !p.Keys.AllExhausted() {
			return false
		}
	}
	return len(r.providers) > 0
}

// RetryAfterSeconds returns the soonest cooldown expiry across all providers.
func (r *ProviderRouter) RetryAfterSeconds() (int, bool) {
	var soonest int
	found := false
	for _, p := range r.providers {
		if secs, ok := p.Keys.RetryAfterSeconds(); ok {
			if !found || secs < soonest {
				soonest = secs
				found = true
			}
		}
	}
	return soonest, found
}

// NumProviders returns the number of providers.
func (r *ProviderRouter) NumProviders() int {
	return len(r.providers)
}

// ProviderStatus shows one provider's key pool status.
type ProviderStatus struct {
	Name    string          `json:"name"`
	BaseURL string          `json:"base_url"`
	Active  bool            `json:"active"` // has at least one eligible key
	Keys    []PerKeyStatus  `json:"keys"`
}

// MultiProviderStatus is the admin status response when the router has
// multiple providers.
type MultiProviderStatus struct {
	Providers                  []ProviderStatus `json:"providers"`
	RetryExhaustedAfterSeconds int              `json:"retry_exhausted_after_seconds"`
	Note                       string           `json:"note"`
}

// MultiStatus returns per-provider status for admin endpoints.
func (r *ProviderRouter) MultiStatus() MultiProviderStatus {
	ps := make([]ProviderStatus, 0, len(r.providers))
	for _, p := range r.providers {
		st := p.Keys.Status()
		active := false
		for _, k := range st.Keys {
			if k.Eligible {
				active = true
				break
			}
		}
		ps = append(ps, ProviderStatus{
			Name:    p.Config.Name,
			BaseURL: p.Config.BaseURL,
			Active:  active,
			Keys:    st.Keys,
		})
	}
	return MultiProviderStatus{
		Providers:                  ps,
		RetryExhaustedAfterSeconds: int(r.providers[0].Keys.cooldown / time.Second),
		Note:                       "unknown means the key has not yet been validated or used since startup; an exhausted key becomes eligible for an automatic retry once retry_exhausted_after_seconds has elapsed since last_429_time (0 disables the cooldown); remaining usage is unavailable from opencode-go API.",
	}
}
