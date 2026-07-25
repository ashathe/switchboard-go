package main

import (
	"context"
	"crypto/rand"
	"crypto/sha256"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"log"
	"net"
	"net/http"
	"net/url"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"sync"
	"time"
)

const (
	openAIClientID         = "app_EMoamEEZ73f0CkXaXp7hrann"
	openAIIssuer           = "https://auth.openai.com"
	openAICodexAPIEndpoint = "https://chatgpt.com/backend-api/codex/responses"
	oauthCallbackPort      = 1455
	oauthTimeout           = 5 * time.Minute
)

// OAuthTokens is the persisted OAuth credential for a provider.
type OAuthTokens struct {
	Provider     string `json:"provider"`
	AccessToken  string `json:"access_token"`
	RefreshToken string `json:"refresh_token"`
	ExpiresAt    int64  `json:"expires_at"` // unix seconds
	Exhausted    bool   `json:"exhausted"`  // quota-exhausted, needs admin reset or re-auth
}

// OAuthTokenStore manages a single OAuth provider's tokens
// with automatic refresh and file persistence.
type OAuthTokenStore struct {
	provider string
	path     string

	mu           sync.Mutex
	tokens       *OAuthTokens
	refreshQueue *sync.Mutex // prevents concurrent refresh storms
	notified     bool        // whether an exhaustion notification was already sent
}

// NewOAuthTokenStore creates a store that persists tokens to disk.
func NewOAuthTokenStore(providerName string) (*OAuthTokenStore, error) {
	home, _ := os.UserHomeDir()
	dir := filepath.Join(home, ".config", "switchboard-go")
	_ = os.MkdirAll(dir, 0o700)
	path := filepath.Join(dir, providerName+"-oauth.json")

	s := &OAuthTokenStore{
		provider:     providerName,
		path:         path,
		refreshQueue: &sync.Mutex{},
	}
	if data, err := os.ReadFile(path); err == nil {
		var t OAuthTokens
		if json.Unmarshal(data, &t) == nil && t.AccessToken != "" {
			s.tokens = &t
		}
	}
	return s, nil
}

// HasValidToken reports whether a usable access token is available (not expired
// and not quota-exhausted). Re-reads from disk to stay in sync with admin ops.
func (s *OAuthTokenStore) HasValidToken() bool {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.reloadLocked()
	return s.tokens != nil && s.tokens.AccessToken != "" &&
		!s.tokens.Exhausted &&
		time.Now().Unix() < s.tokens.ExpiresAt-30
}

// IsExhausted reports whether the token was quota-exhausted and needs a reset.
func (s *OAuthTokenStore) IsExhausted() bool {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.reloadLocked()
	return s.tokens != nil && s.tokens.Exhausted
}

// MarkExhausted flags the token as quota-exhausted. It does NOT clear the
// refresh token so the provider can be reset via admin or auto-retry.
func (s *OAuthTokenStore) MarkExhausted() error {
	s.mu.Lock()
	if s.tokens == nil {
		s.mu.Unlock()
		return nil
	}
	s.tokens.Exhausted = true
	err := s.persistLocked()
	s.mu.Unlock()
	return err
}

// Reset un-exhausts the token so it becomes eligible again.
func (s *OAuthTokenStore) Reset() error {
	s.mu.Lock()
	if s.tokens == nil {
		s.mu.Unlock()
		return nil
	}
	s.tokens.Exhausted = false
	s.notified = false // re-arm notification for next exhaustion
	err := s.persistLocked()
	s.mu.Unlock()
	return err
}

// ShouldNotifyExhausted returns true exactly once per exhaustion cycle.
func (s *OAuthTokenStore) ShouldNotifyExhausted() bool {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.notified {
		return false
	}
	if s.tokens == nil || !s.tokens.Exhausted {
		return false
	}
	s.notified = true
	return true
}

// AccessToken returns the current access token, refreshing if needed.
func (s *OAuthTokenStore) AccessToken(ctx context.Context) (string, error) {
	s.mu.Lock()
	s.reloadLocked()
	if s.tokens == nil || s.tokens.AccessToken == "" {
		s.mu.Unlock()
		return "", fmt.Errorf("no oauth token available — run 'switchboard-go oauth-login %s'", s.provider)
	}
	if s.tokens.Exhausted {
		s.mu.Unlock()
		return "", fmt.Errorf("oauth token quota-exhausted — use admin reset or re-run oauth-login")
	}
	if time.Now().Unix() < s.tokens.ExpiresAt-30 {
		tok := s.tokens.AccessToken
		s.mu.Unlock()
		return tok, nil
	}
	refresh := s.tokens.RefreshToken
	s.mu.Unlock()

	// Serialize refreshes so concurrent requests don't race.
	s.refreshQueue.Lock()
	defer s.refreshQueue.Unlock()

	// Double-check after acquiring the refresh lock.
	s.mu.Lock()
	s.reloadLocked()
	if s.tokens != nil && !s.tokens.Exhausted && time.Now().Unix() < s.tokens.ExpiresAt-30 {
		tok := s.tokens.AccessToken
		s.mu.Unlock()
		return tok, nil
	}
	s.mu.Unlock()

	newTokens, err := refreshOAuthToken(refresh)
	if err != nil {
		return "", fmt.Errorf("oauth token refresh failed: %w", err)
	}
	s.mu.Lock()
	s.tokens = &OAuthTokens{
		Provider:     s.provider,
		AccessToken:  newTokens.AccessToken,
		RefreshToken: coalesce(newTokens.RefreshToken, s.tokens.RefreshToken),
		ExpiresAt:    time.Now().Unix() + newTokens.ExpiresIn,
	}
	tok := s.tokens.AccessToken
	s.mu.Unlock()

	if err := s.persistLocked(); err != nil {
		log.Printf("oauth: failed to persist tokens for %s: %v", s.provider, err)
	}
	return tok, nil
}

func (s *OAuthTokenStore) persistLocked() error {
	data, err := json.MarshalIndent(s.tokens, "", "  ")
	if err != nil {
		return err
	}
	return os.WriteFile(s.path, data, 0o600)
}

// reloadLocked re-reads tokens from disk. Must be called with mu held.
func (s *OAuthTokenStore) reloadLocked() {
	data, err := os.ReadFile(s.path)
	if err != nil {
		return
	}
	var t OAuthTokens
	if json.Unmarshal(data, &t) == nil && t.AccessToken != "" {
		s.tokens = &t
	}
}

// SaveTokens persists tokens after initial OAuth login.
func (s *OAuthTokenStore) SaveTokens(tokens *OAuthTokens) error {
	s.mu.Lock()
	s.tokens = tokens
	err := s.persistLocked()
	s.mu.Unlock()
	return err
}

// ---------------------------------------------------------------------------
// OAuth login flow (user-facing CLI / first-time setup)
// ---------------------------------------------------------------------------

type oauthTokenResponse struct {
	AccessToken  string `json:"access_token"`
	RefreshToken string `json:"refresh_token"`
	IDToken      string `json:"id_token"`
	ExpiresIn    int64  `json:"expires_in"`
}

func generatePKCE() (verifier string, challenge string, err error) {
	b := make([]byte, 43)
	if _, err := rand.Read(b); err != nil {
		return "", "", err
	}
	const chars = "ABCDEFGHIJKLMNOPQRSTUVWXYZabcdefghijklmnopqrstuvwxyz0123456789-._~"
	for i := range b {
		b[i] = chars[int(b[i])%len(chars)]
	}
	verifier = string(b)
	h := sha256.Sum256([]byte(verifier))
	challenge = base64.RawURLEncoding.EncodeToString(h[:])
	return verifier, challenge, nil
}

func buildChatGPTAuthorizeURL(redirectURI, challenge, state string) string {
	params := url.Values{
		"response_type":               {"code"},
		"client_id":                   {openAIClientID},
		"redirect_uri":                {redirectURI},
		"scope":                       {"openid profile email offline_access"},
		"code_challenge":              {challenge},
		"code_challenge_method":       {"S256"},
		"id_token_add_organizations":  {"true"},
		"codex_cli_simplified_flow":   {"true"},
		"state":                       {state},
		"originator":                  {"opencode"},
	}
	return openAIIssuer + "/oauth/authorize?" + params.Encode()
}

func exchangeCodeForTokens(code, redirectURI, verifier string) (*oauthTokenResponse, error) {
	body := url.Values{
		"grant_type":    {"authorization_code"},
		"code":          {code},
		"redirect_uri":  {redirectURI},
		"client_id":     {openAIClientID},
		"code_verifier": {verifier},
	}.Encode()
	req, _ := http.NewRequest(http.MethodPost, openAIIssuer+"/oauth/token", strings.NewReader(body))
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()
	if resp.StatusCode >= 300 {
		return nil, fmt.Errorf("token exchange: status %d", resp.StatusCode)
	}
	var tr oauthTokenResponse
	if err := json.NewDecoder(resp.Body).Decode(&tr); err != nil {
		return nil, err
	}
	return &tr, nil
}

func refreshOAuthToken(refreshToken string) (*oauthTokenResponse, error) {
	body := url.Values{
		"grant_type":    {"refresh_token"},
		"refresh_token": {refreshToken},
		"client_id":     {openAIClientID},
	}.Encode()
	req, _ := http.NewRequest(http.MethodPost, openAIIssuer+"/oauth/token", strings.NewReader(body))
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()
	if resp.StatusCode >= 300 {
		return nil, fmt.Errorf("token refresh: status %d", resp.StatusCode)
	}
	var tr oauthTokenResponse
	if err := json.NewDecoder(resp.Body).Decode(&tr); err != nil {
		return nil, err
	}
	if tr.ExpiresIn <= 0 {
		tr.ExpiresIn = 3600
	}
	return &tr, nil
}

// StartOAuthLogin runs the browser-based OAuth PKCE flow and returns tokens.
// It starts a local HTTP server on oauthCallbackPort, opens the user's
// browser to the ChatGPT consent screen, and waits for the callback.
func StartOAuthLogin(providerName string) (*OAuthTokens, error) {
	verifier, challenge, err := generatePKCE()
	if err != nil {
		return nil, fmt.Errorf("pkce: %w", err)
	}
	stateBytes := make([]byte, 32)
	if _, err := rand.Read(stateBytes); err != nil {
		return nil, err
	}
	state := base64.RawURLEncoding.EncodeToString(stateBytes)
	redirectURI := fmt.Sprintf("http://localhost:%d/auth/callback", oauthCallbackPort)

	type result struct {
		tokens *OAuthTokens
		err    error
	}
	done := make(chan result, 1)

	mux := http.NewServeMux()
	mux.HandleFunc("/auth/callback", func(w http.ResponseWriter, r *http.Request) {
		q := r.URL.Query()
		if errStr := q.Get("error"); errStr != "" {
			done <- result{err: fmt.Errorf("oauth error: %s — %s", errStr, q.Get("error_description"))}
			http.Error(w, "Authentication failed. You can close this window.", http.StatusBadRequest)
			return
		}
		code := q.Get("code")
		if code == "" {
			done <- result{err: fmt.Errorf("missing authorization code")}
			http.Error(w, "Missing authorization code.", http.StatusBadRequest)
			return
		}
		if q.Get("state") != state {
			done <- result{err: fmt.Errorf("state mismatch — possible CSRF")}
			http.Error(w, "Invalid state.", http.StatusBadRequest)
			return
		}
		// Exchange in background — keep the browser happy fast.
		go func() {
			tokens, err := exchangeCodeForTokens(code, redirectURI, verifier)
			if err != nil {
				done <- result{err: fmt.Errorf("token exchange: %w", err)}
				return
			}
			done <- result{tokens: &OAuthTokens{
				Provider:     providerName,
				AccessToken:  tokens.AccessToken,
				RefreshToken: tokens.RefreshToken,
				ExpiresAt:    time.Now().Unix() + tokens.ExpiresIn,
			}}
		}()
		w.Header().Set("Content-Type", "text/html")
		fmt.Fprint(w, `<html><body><h2>✅ Authorized</h2><p>You can close this window and return to the terminal.</p></body></html>`)
	})

	ln, err := net.Listen("tcp", fmt.Sprintf("127.0.0.1:%d", oauthCallbackPort))
	if err != nil {
		return nil, fmt.Errorf("cannot start OAuth listener on port %d: %w", oauthCallbackPort, err)
	}
	srv := &http.Server{Handler: mux, ReadHeaderTimeout: 5 * time.Second}
	go srv.Serve(ln)
	defer srv.Close()

	authURL := buildChatGPTAuthorizeURL(redirectURI, challenge, state)
	fmt.Printf("\n🔑 Opening browser for ChatGPT OAuth login...\n")
	fmt.Printf("   If the browser doesn't open, visit:\n   %s\n\n", authURL)
	openBrowser(authURL)

	select {
	case r := <-done:
		return r.tokens, r.err
	case <-time.After(oauthTimeout):
		return nil, fmt.Errorf("oauth login timed out after %v", oauthTimeout)
	}
}

// openBrowser tries to open the OS default browser.
func openBrowser(urlStr string) {
	// Best-effort; failures are non-fatal.
	for _, cmd := range [][]string{
		{"open", urlStr},       // macOS
		{"xdg-open", urlStr},   // Linux
		{"rundll32", "url.dll,FileProtocolHandler", urlStr}, // Windows
	} {
		if err := runCommand(cmd[0], cmd[1:]...); err == nil {
			return
		}
	}
}

func runCommand(name string, args ...string) error {
	return exec.Command(name, args...).Start()
}

func coalesce(a, b string) string {
	if a != "" {
		return a
	}
	return b
}
