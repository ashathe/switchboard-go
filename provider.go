package main

import (
	"fmt"
	"log"
	"sort"
	"time"
)

// ProviderConfig is the user-facing configuration for a single upstream provider.
// It is read from YAML or derived from legacy single-provider config.
type ProviderConfig struct {
	Name     string   `yaml:"name"`
	BaseURL  string   `yaml:"base_url"`
	APIKeys  []string `yaml:"api_keys"`
	Priority int      `yaml:"priority"`   // lower = tried first
	AuthType string   `yaml:"auth_type"`  // "" or "api_key" (default), "oauth" for ChatGPT Plus
}

// Provider pairs a user-facing config with a KeyManager for its API keys.
// When AuthType is "oauth", Keys is nil and OAuth holds the token store.
type Provider struct {
	Config ProviderConfig
	Keys   *KeyManager
	OAuth  *OAuthTokenStore // non-nil only for AuthType == "oauth"
}

// IsOAuth returns true when this provider uses OAuth instead of API keys.
func (p *Provider) IsOAuth() bool { return p.Config.AuthType == "oauth" && p.OAuth != nil }

// HasCapacity returns true when the provider can serve requests right now
// (has an eligible key or a valid OAuth token).
func (p *Provider) HasCapacity() bool {
	if p.IsOAuth() {
		return p.OAuth.HasValidToken()
	}
	_, _, ok := p.Keys.Current()
	return ok
}

// ProviderRouter manages multiple providers, each with its own key pool.
// Providers are tried in priority order. When all keys in provider N are
// exhausted, the router falls through to provider N+1.
type ProviderRouter struct {
	providers []*Provider // sorted by priority, stable
}

// NewProviderRouter creates a router from provider configs and an optional
// global cooldown duration applied to every non-OAuth provider's KeyManager.
func NewProviderRouter(configs []ProviderConfig, cooldown time.Duration) (*ProviderRouter, error) {
	if len(configs) == 0 {
		return nil, fmt.Errorf("at least one provider is required")
	}
	providers := make([]*Provider, 0, len(configs))
	for _, cfg := range configs {
		p := &Provider{Config: cfg}
		if cfg.AuthType == "oauth" {
			store, err := NewOAuthTokenStore(cfg.Name)
			if err != nil {
				return nil, fmt.Errorf("provider %q: oauth: %w", cfg.Name, err)
			}
			if !store.HasValidToken() {
				log.Printf("provider %q: no valid oauth token; run 'switchboard-go oauth-login %s' to authenticate", cfg.Name, cfg.Name)
			}
			p.OAuth = store
		} else {
			if len(cfg.APIKeys) == 0 {
				return nil, fmt.Errorf("provider %q: at least one api_key is required (or set auth_type: oauth)", cfg.Name)
			}
			if cfg.BaseURL == "" {
				return nil, fmt.Errorf("provider %q: base_url is required", cfg.Name)
			}
			p.Keys = NewKeyManager(cfg.APIKeys, cooldown)
		}
		providers = append(providers, p)
	}
	// Sort stable by priority; break ties by original slice order.
	sort.SliceStable(providers, func(i, j int) bool {
		return providers[i].Config.Priority < providers[j].Config.Priority
	})
	return &ProviderRouter{providers: providers}, nil
}

// Current returns the first provider that has an eligible key or valid OAuth token.
// ok is false when every provider is fully exhausted or has no valid credentials.
func (r *ProviderRouter) Current() (prov *Provider, keyIdx int, key string, ok bool) {
	for _, p := range r.providers {
		if p.IsOAuth() {
			if p.OAuth.HasValidToken() {
				return p, 0, "", true // OAuth providers don't use key indices
			}
			continue
		}
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

// AllExhausted returns true when every provider is fully exhausted or has no
// valid credentials.
func (r *ProviderRouter) AllExhausted() bool {
	for _, p := range r.providers {
		if p.IsOAuth() {
			if p.OAuth.HasValidToken() {
				return false
			}
			continue
		}
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

// singleKeyStatus builds a minimal StatusResponse for an OAuth provider's
// single "key" so SMTP notifications can use the same format.
func (r *ProviderRouter) singleKeyStatus(name, state string) StatusResponse {
	return StatusResponse{
		CurrentKeyIndex: 0,
		Keys: []PerKeyStatus{{
			Index:    0,
			State:    state,
			Current:  true,
			Eligible: state == "available",
		}},
		RetryExhaustedAfterSeconds: 0,
		Note:                       "oauth provider " + name,
	}
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
		if p.IsOAuth() {
			active := p.OAuth.HasValidToken()
			keys := []PerKeyStatus{{
				Index:      0,
				State:      oauthState(active),
				Current:    true,
				Eligible:   active,
			}}
			ps = append(ps, ProviderStatus{
				Name:    p.Config.Name,
				BaseURL: openAICodexAPIEndpoint,
				Active:  active,
				Keys:    keys,
			})
			continue
		}
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
	cooldown := time.Duration(0)
	if len(r.providers) > 0 && r.providers[0].Keys != nil {
		cooldown = r.providers[0].Keys.cooldown
	}
	return MultiProviderStatus{
		Providers:                  ps,
		RetryExhaustedAfterSeconds: int(cooldown / time.Second),
		Note:                       "unknown means the key has not yet been validated or used since startup; an exhausted key becomes eligible for an automatic retry once retry_exhausted_after_seconds has elapsed since last_429_time (0 disables the cooldown); remaining usage is unavailable from opencode-go API.",
	}
}

func oauthState(active bool) string {
	if active {
		return "available"
	}
	return "exhausted"
}
