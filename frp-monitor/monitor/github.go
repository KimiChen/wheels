package monitor

import (
	"context"
	"crypto/sha256"
	"encoding/base64"
	"encoding/json"
	"errors"
	"io"
	"net"
	"net/http"
	"net/url"
	"strconv"
	"strings"
	"time"

	"github.com/fatedier/frp/extension/frpmonitor/shared"
)

const oauthCookie = "frp_monitor_oauth"
const oauthTTL = 10 * time.Minute

// Bound unfinished OAuth attempts globally and per connection peer. A full
// share evicts its oldest attempt, including when users share a reverse proxy.
const maxOAuthPending = 64
const maxOAuthPendingPerIP = 8

type oauthAttempt struct {
	verifier string
	expires  time.Time
	ip       string
}
type githubAuth struct {
	clientID, secret, callback string
	allowed                    map[string]bool
	pending                    map[string]oauthAttempt
	client                     *http.Client
}

func newAdmin(cfg shared.MonitorConfig) (*adminState, error) {
	if cfg.GitHubClientID == "" {
		return nil, nil
	}
	data, err := readPrivate(cfg.GitHubClientSecretFile)
	if err != nil {
		return nil, errors.New("GitHub client secret unavailable")
	}
	secret := strings.TrimSpace(string(data))
	if len(secret) < 8 || len(secret) > 256 || strings.ContainsAny(secret, "\x00\r\n\t ") {
		return nil, errors.New("invalid GitHub client secret")
	}
	g := &githubAuth{clientID: cfg.GitHubClientID, secret: secret, callback: cfg.GitHubCallbackURL, allowed: map[string]bool{}, pending: map[string]oauthAttempt{}, client: &http.Client{Timeout: 8 * time.Second, CheckRedirect: func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse }}}
	for _, login := range cfg.GitHubAdminUsers {
		g.allowed[strings.ToLower(login)] = true
	}
	return &adminState{sessions: map[string]adminSession{}, tokens: 5, at: time.Now(), github: g}, nil
}

func (s *Service) secureAdminCookie(r *http.Request) bool {
	return r.TLS != nil || strings.HasPrefix(s.cfg.GitHubCallbackURL, "https://")
}
func (s *Service) adminSameOrigin(r *http.Request) bool {
	// newAdmin only exists once the four OAuth settings passed validation, so
	// the callback URL is never empty here; reject explicitly rather than
	// falling back to a weaker host comparison.
	if s.cfg.GitHubCallbackURL == "" {
		return false
	}
	if site := r.Header.Get("Sec-Fetch-Site"); site != "" && site != "same-origin" && site != "none" {
		return false
	}
	origins := r.Header.Values("Origin")
	if len(origins) == 0 {
		return true
	}
	if len(origins) != 1 {
		return false
	}
	callback, err := url.Parse(s.cfg.GitHubCallbackURL)
	if err != nil {
		return false
	}
	origin, err := url.Parse(origins[0])
	if err != nil || origin.User != nil || origin.Opaque != "" || origin.Path != "" || origin.RawQuery != "" || origin.ForceQuery || origin.Fragment != "" || strings.Contains(origins[0], "#") {
		return false
	}
	expected, ok := originAuthority(callback.Scheme, callback.Host)
	actual, valid := originAuthority(origin.Scheme, origin.Host)
	host, hostValid := originAuthority(callback.Scheme, r.Host)
	return ok && valid && hostValid && origin.Scheme == callback.Scheme && actual == expected && host == expected
}

// Browsers omit default ports from Origin. Compare effective ports while still
// requiring the request Host and the complete Origin authority to match.
func originAuthority(scheme, authority string) (string, bool) {
	if scheme != "http" && scheme != "https" || authority == "" || strings.ContainsAny(authority, "/?#@\\") {
		return "", false
	}
	u, err := url.Parse(scheme + "://" + authority)
	if err != nil || u.Host != authority || u.Hostname() == "" || u.User != nil {
		return "", false
	}
	if strings.HasPrefix(authority, "[") {
		if !strings.Contains(u.Hostname(), ":") || net.ParseIP(u.Hostname()) == nil {
			return "", false
		}
	} else if strings.Contains(u.Hostname(), ":") {
		return "", false
	}
	port := 80
	if scheme == "https" {
		port = 443
	}
	if value := u.Port(); value != "" {
		port, err = strconv.Atoi(value)
		if err != nil || port < 1 || port > 65535 {
			return "", false
		}
	} else if strings.HasSuffix(authority, ":") {
		return "", false
	}
	return net.JoinHostPort(strings.ToLower(u.Hostname()), strconv.Itoa(port)), true
}

// clientIP uses the connection peer for the pending-attempt quota only. Proxy
// headers are not trusted, and this is never part of an authorization decision.
func clientIP(r *http.Request) string {
	host, _, err := net.SplitHostPort(r.RemoteAddr)
	if err != nil {
		return r.RemoteAddr
	}
	return host
}

func (s *Service) githubStart(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodGet {
		methodNotAllowed(w, http.MethodGet)
		return
	}
	if !s.adminSameOrigin(r) {
		http.Error(w, "same origin required", 403)
		return
	}
	state, err := randomToken()
	if err != nil {
		http.Error(w, "unavailable", 503)
		return
	}
	verifier, err := randomToken()
	if err != nil {
		http.Error(w, "unavailable", 503)
		return
	}
	a := s.admin
	now := time.Now()
	ip := clientIP(r)
	a.mu.Lock()
	a.tokens += now.Sub(a.at).Seconds() / 3
	if a.tokens > 5 {
		a.tokens = 5
	}
	a.at = now
	if a.tokens < 1 {
		a.mu.Unlock()
		w.Header().Set("Retry-After", "3")
		http.Error(w, "try later", 429)
		return
	}
	a.tokens--
	// Replacing this browser's attempt already frees a slot. Remove it before
	// counting pending attempts, so no other browser is evicted unnecessarily.
	if old, e := r.Cookie(oauthCookie); e == nil {
		delete(a.github.pending, tokenHash(old.Value))
	}
	sameIP := 0
	oldest, oldestIP := "", ""
	for key, attempt := range a.github.pending {
		if !now.Before(attempt.expires) {
			delete(a.github.pending, key)
			continue
		}
		if attempt.ip == ip {
			sameIP++
			if oldestIP == "" || attempt.expires.Before(a.github.pending[oldestIP].expires) {
				oldestIP = key
			}
		}
		if oldest == "" || attempt.expires.Before(a.github.pending[oldest].expires) {
			oldest = key
		}
	}
	// Evict only one attempt: freeing a peer slot also frees a global slot.
	// Pending attempts behind a shared proxy must not block new logins.
	if sameIP >= maxOAuthPendingPerIP {
		delete(a.github.pending, oldestIP)
	} else if len(a.github.pending) >= maxOAuthPending {
		delete(a.github.pending, oldest)
	}
	a.github.pending[tokenHash(state)] = oauthAttempt{verifier: verifier, expires: now.Add(oauthTTL), ip: ip}
	a.mu.Unlock()
	hash := sha256.Sum256([]byte(verifier))
	query := url.Values{"client_id": {a.github.clientID}, "redirect_uri": {a.github.callback}, "state": {state}, "code_challenge": {base64.RawURLEncoding.EncodeToString(hash[:])}, "code_challenge_method": {"S256"}, "allow_signup": {"false"}, "scope": {""}}
	http.SetCookie(w, &http.Cookie{Name: oauthCookie, Value: state, Path: "/api/admin/v1/auth/github", MaxAge: int(oauthTTL.Seconds()), HttpOnly: true, Secure: s.secureAdminCookie(r), SameSite: http.SameSiteLaxMode})
	http.Redirect(w, r, "https://github.com/login/oauth/authorize?"+query.Encode(), http.StatusFound)
}

func (s *Service) githubCallback(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodGet {
		methodNotAllowed(w, http.MethodGet)
		return
	}
	fail := func(kind string) { http.Redirect(w, r, "/admin/?auth_error="+kind, http.StatusSeeOther) }
	q := r.URL.Query()
	state := q.Get("state")
	cookie, err := r.Cookie(oauthCookie)
	if err != nil || len(state) != 43 || len(q["state"]) != 1 || !equalToken(cookie.Value, state) {
		fail("failed")
		return
	}
	http.SetCookie(w, &http.Cookie{Name: oauthCookie, Path: "/api/admin/v1/auth/github", MaxAge: -1, HttpOnly: true, Secure: s.secureAdminCookie(r), SameSite: http.SameSiteLaxMode})
	a := s.admin
	a.mu.Lock()
	attempt, ok := a.github.pending[tokenHash(state)]
	delete(a.github.pending, tokenHash(state))
	a.mu.Unlock()
	if !ok || !time.Now().Before(attempt.expires) {
		fail("failed")
		return
	}
	if q.Get("error") != "" {
		fail("access_denied")
		return
	}
	code := q.Get("code")
	if len(q["code"]) != 1 || len(code) < 1 || len(code) > 512 || strings.ContainsAny(code, "\x00\r\n") {
		fail("failed")
		return
	}
	ctx, cancel := context.WithTimeout(r.Context(), 8*time.Second)
	defer cancel()
	login, err := a.github.identity(ctx, code, attempt.verifier)
	if err != nil {
		fail("failed")
		return
	}
	if !a.github.allowed[strings.ToLower(login)] {
		fail("access_denied")
		return
	}
	if err = s.createAdminSession(w, r, login); err != nil {
		fail("failed")
		return
	}
	http.Redirect(w, r, "/admin/", http.StatusSeeOther)
}

func (g *githubAuth) identity(ctx context.Context, code, verifier string) (string, error) {
	invalid := errors.New("GitHub authentication failed")
	values := url.Values{"client_id": {g.clientID}, "client_secret": {g.secret}, "code": {code}, "redirect_uri": {g.callback}, "code_verifier": {verifier}}
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, "https://github.com/login/oauth/access_token", strings.NewReader(values.Encode()))
	if err != nil {
		return "", invalid
	}
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	req.Header.Set("Accept", "application/json")
	req.Header.Set("User-Agent", "frp-monitor/"+shared.Version)
	var token struct {
		AccessToken string `json:"access_token"`
		TokenType   string `json:"token_type"`
		Error       string `json:"error"`
	}
	if err = g.readJSON(req, &token); err != nil || token.Error != "" || token.AccessToken == "" || len(token.AccessToken) > 4096 || strings.ContainsAny(token.AccessToken, "\x00\r\n\t ") || !strings.EqualFold(token.TokenType, "bearer") {
		return "", invalid
	}
	req, err = http.NewRequestWithContext(ctx, http.MethodGet, "https://api.github.com/user", nil)
	if err != nil {
		return "", invalid
	}
	req.Header.Set("Authorization", "Bearer "+token.AccessToken)
	req.Header.Set("Accept", "application/vnd.github+json")
	req.Header.Set("X-GitHub-Api-Version", "2022-11-28")
	req.Header.Set("User-Agent", "frp-monitor/"+shared.Version)
	var user struct {
		Login string `json:"login"`
		ID    int64  `json:"id"`
		Type  string `json:"type"`
	}
	if g.readJSON(req, &user) != nil || user.ID <= 0 || user.Type != "User" || len(user.Login) < 1 || len(user.Login) > 39 {
		return "", invalid
	}
	return user.Login, nil
}
func (g *githubAuth) readJSON(req *http.Request, target any) error {
	res, err := g.client.Do(req)
	if err != nil {
		return errors.New("GitHub unavailable")
	}
	defer res.Body.Close()
	if res.StatusCode != 200 {
		return errors.New("GitHub unavailable")
	}
	data, err := io.ReadAll(io.LimitReader(res.Body, 65537))
	if err != nil || len(data) > 65536 {
		return errors.New("invalid GitHub response")
	}
	return json.Unmarshal(data, target)
}
func (s *Service) createAdminSession(w http.ResponseWriter, r *http.Request, login string) error {
	token, err := randomToken()
	if err != nil {
		return err
	}
	csrf, err := randomToken()
	if err != nil {
		return err
	}
	now := time.Now()
	a := s.admin
	a.mu.Lock()
	defer a.mu.Unlock()
	for key, session := range a.sessions {
		if !now.Before(session.ExpiresAt) {
			delete(a.sessions, key)
		}
	}
	if old, e := r.Cookie(adminCookie); e == nil {
		delete(a.sessions, tokenHash(old.Value))
	}
	if len(a.sessions) >= 64 {
		return errors.New("session limit")
	}
	session := adminSession{CSRF: csrf, ExpiresAt: now.Add(adminTTL), Login: login}
	a.sessions[tokenHash(token)] = session
	http.SetCookie(w, &http.Cookie{Name: adminCookie, Value: token, Path: "/", Expires: session.ExpiresAt, MaxAge: int(adminTTL.Seconds()), HttpOnly: true, Secure: s.secureAdminCookie(r), SameSite: http.SameSiteStrictMode})
	return nil
}
