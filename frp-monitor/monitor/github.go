package monitor

import (
	"context"
	"crypto/sha256"
	"encoding/base64"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"net/url"
	"strings"
	"time"

	"github.com/fatedier/frp/extension/frpmonitor/shared"
)

const oauthCookie = "frp_monitor_oauth"
const oauthTTL = 10 * time.Minute

type oauthAttempt struct {
	verifier string
	expires  time.Time
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
	if s.cfg.GitHubCallbackURL == "" {
		return sameOrigin(r)
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
	u, err := url.Parse(s.cfg.GitHubCallbackURL)
	return err == nil && origins[0] == u.Scheme+"://"+u.Host && r.Host == u.Host
}

func (s *Service) githubStart(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodGet {
		w.WriteHeader(405)
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
	a.mu.Lock()
	a.tokens += now.Sub(a.at).Seconds() / 3
	if a.tokens > 5 {
		a.tokens = 5
	}
	a.at = now
	for key, attempt := range a.github.pending {
		if !now.Before(attempt.expires) {
			delete(a.github.pending, key)
		}
	}
	if a.tokens < 1 || len(a.github.pending) >= 64 {
		a.mu.Unlock()
		w.Header().Set("Retry-After", "3")
		http.Error(w, "try later", 429)
		return
	}
	a.tokens--
	if old, e := r.Cookie(oauthCookie); e == nil {
		delete(a.github.pending, tokenHash(old.Value))
	}
	a.github.pending[tokenHash(state)] = oauthAttempt{verifier: verifier, expires: now.Add(oauthTTL)}
	a.mu.Unlock()
	hash := sha256.Sum256([]byte(verifier))
	query := url.Values{"client_id": {a.github.clientID}, "redirect_uri": {a.github.callback}, "state": {state}, "code_challenge": {base64.RawURLEncoding.EncodeToString(hash[:])}, "code_challenge_method": {"S256"}, "allow_signup": {"false"}, "scope": {""}}
	http.SetCookie(w, &http.Cookie{Name: oauthCookie, Value: state, Path: "/api/admin/v1/auth/github", MaxAge: int(oauthTTL.Seconds()), HttpOnly: true, Secure: s.secureAdminCookie(r), SameSite: http.SameSiteLaxMode})
	http.Redirect(w, r, "https://github.com/login/oauth/authorize?"+query.Encode(), http.StatusFound)
}

func (s *Service) githubCallback(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodGet {
		w.WriteHeader(405)
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
