package monitor

import (
	"crypto/rand"
	"crypto/sha256"
	"crypto/subtle"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strings"
	"sync"
	"time"

	"github.com/fatedier/frp/extension/frpmonitor/shared"
)

const adminCookie = "frp_monitor_admin"
const adminTTL = 8 * time.Hour

type adminCredential struct {
	TokenSHA256 string `json:"token_sha256"`
}
type adminSession struct {
	CSRF      string    `json:"csrf_token"`
	ExpiresAt time.Time `json:"expires_at"`
}
type adminState struct {
	mu       sync.Mutex
	digest   string
	sessions map[string]adminSession
	tokens   float64
	at       time.Time
	degraded bool
}

func readAdmin(path string) (string, error) {
	data, err := readPrivate(path)
	if err != nil {
		return "", err
	}
	var cfg adminCredential
	if strictJSON(data, &cfg) != nil || !digestPattern.MatchString(cfg.TokenSHA256) {
		return "", errors.New("invalid admin credentials")
	}
	return cfg.TokenSHA256, nil
}
func newAdmin(path string) (*adminState, error) {
	digest, err := readAdmin(path)
	if err != nil {
		return nil, errors.New("admin credentials unavailable")
	}
	return &adminState{digest: digest, sessions: map[string]adminSession{}, tokens: 5, at: time.Now()}, nil
}
func (s *Service) reloadAdmin() {
	if s.admin == nil {
		return
	}
	digest, err := readAdmin(s.cfg.AdminCredentialsFile)
	s.admin.mu.Lock()
	defer s.admin.mu.Unlock()
	s.admin.degraded = err != nil
	if err != nil {
		return
	}
	if digest != s.admin.digest {
		s.admin.digest = digest
		s.admin.sessions = map[string]adminSession{}
	}
	for id, session := range s.admin.sessions {
		if !time.Now().Before(session.ExpiresAt) {
			delete(s.admin.sessions, id)
		}
	}
}
func randomToken() (string, error) {
	data := make([]byte, 32)
	if _, err := rand.Read(data); err != nil {
		return "", err
	}
	return base64.RawURLEncoding.EncodeToString(data), nil
}
func tokenHash(value string) string {
	digest := sha256.Sum256([]byte(value))
	return hex.EncodeToString(digest[:])
}
func equalToken(a, b string) bool { return subtle.ConstantTimeCompare([]byte(a), []byte(b)) == 1 }
func validAdminToken(token string) bool {
	raw, err := base64.RawURLEncoding.DecodeString(token)
	return err == nil && len(raw) == 32
}

func sameOrigin(r *http.Request) bool {
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
	expected := "http"
	if r.TLS != nil {
		expected = "https"
	}
	u, err := url.Parse(origins[0])
	return err == nil && u.Scheme == expected && u.Host == r.Host && u.User == nil && u.RawQuery == "" && u.Fragment == "" && u.Path == ""
}
func (s *Service) session(r *http.Request) (adminSession, string, bool) {
	if s.admin == nil {
		return adminSession{}, "", false
	}
	cookie, err := r.Cookie(adminCookie)
	if err != nil || len(cookie.Value) != 43 {
		return adminSession{}, "", false
	}
	key := tokenHash(cookie.Value)
	s.admin.mu.Lock()
	defer s.admin.mu.Unlock()
	session, ok := s.admin.sessions[key]
	if !ok || !time.Now().Before(session.ExpiresAt) {
		delete(s.admin.sessions, key)
		return adminSession{}, "", false
	}
	return session, key, true
}
func adminJSON(w http.ResponseWriter, status int, value any) {
	w.Header().Set("Content-Type", "application/json; charset=utf-8")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(value)
}
func decodeAdmin(w http.ResponseWriter, r *http.Request, target any) bool {
	if strings.Split(r.Header.Get("Content-Type"), ";")[0] != "application/json" {
		http.Error(w, "application/json required", http.StatusUnsupportedMediaType)
		return false
	}
	data, err := io.ReadAll(http.MaxBytesReader(w, r.Body, 1024*1024))
	if err != nil || strictJSON(data, target) != nil {
		http.Error(w, "invalid request", http.StatusBadRequest)
		return false
	}
	return true
}
func (s *Service) handleAdmin(w http.ResponseWriter, r *http.Request) {
	publicHeaders(w)
	w.Header().Set("Referrer-Policy", "no-referrer")
	_ = http.NewResponseController(w).SetReadDeadline(time.Now().Add(5 * time.Second))
	_ = http.NewResponseController(w).SetWriteDeadline(time.Now().Add(10 * time.Second))
	if s.admin == nil {
		http.NotFound(w, r)
		return
	}
	if !sameOrigin(r) {
		http.Error(w, "same origin required", http.StatusForbidden)
		return
	}
	path := strings.TrimPrefix(r.URL.Path, "/api/admin/v1/")
	if path == "login" {
		s.handleLogin(w, r)
		return
	}
	session, key, ok := s.session(r)
	if !ok {
		http.Error(w, "unauthorized", http.StatusUnauthorized)
		return
	}
	if r.Method != http.MethodGet && r.Method != http.MethodHead && (!equalToken(r.Header.Get("X-CSRF-Token"), session.CSRF) || len(r.Header.Values("X-CSRF-Token")) != 1) {
		http.Error(w, "CSRF token required", http.StatusForbidden)
		return
	}
	switch path {
	case "session":
		if r.Method != http.MethodGet {
			w.WriteHeader(405)
			return
		}
		adminJSON(w, 200, session)
	case "logout":
		if r.Method != http.MethodPost {
			w.WriteHeader(405)
			return
		}
		s.admin.mu.Lock()
		delete(s.admin.sessions, key)
		s.admin.mu.Unlock()
		http.SetCookie(w, &http.Cookie{Name: adminCookie, Path: "/", MaxAge: -1, Secure: r.TLS != nil, HttpOnly: true, SameSite: http.SameSiteStrictMode})
		w.WriteHeader(204)
	case "nodes":
		if r.Method == http.MethodGet {
			adminJSON(w, 200, s.adminSnapshot())
			return
		}
		if r.Method == http.MethodPost {
			s.createNode(w, r)
			return
		}
		w.WriteHeader(405)
	case "probes":
		s.handleAdminProbes(w, r)
	default:
		if strings.HasPrefix(path, "nodes/") {
			s.mutateNode(w, r, strings.TrimPrefix(path, "nodes/"))
			return
		}
		http.NotFound(w, r)
	}
}
func (s *Service) handleLogin(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		w.WriteHeader(405)
		return
	}
	a := s.admin
	a.mu.Lock()
	now := time.Now()
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
	a.mu.Unlock()
	var input struct {
		Token string `json:"token"`
	}
	if !decodeAdmin(w, r, &input) {
		return
	}
	sessionToken, err := randomToken()
	if err != nil {
		http.Error(w, "unavailable", 503)
		return
	}
	csrf, err := randomToken()
	if err != nil {
		http.Error(w, "unavailable", 503)
		return
	}
	a.mu.Lock()
	defer a.mu.Unlock()
	if !validAdminToken(input.Token) || !equalToken(tokenHash(input.Token), a.digest) {
		http.Error(w, "unauthorized", 401)
		return
	}
	for id, session := range a.sessions {
		if !now.Before(session.ExpiresAt) {
			delete(a.sessions, id)
		}
	}
	// Login replaces this browser's existing session, preventing fixation and leaks.
	if old, err := r.Cookie(adminCookie); err == nil {
		delete(a.sessions, tokenHash(old.Value))
	}
	if len(a.sessions) >= 64 {
		http.Error(w, "session limit", 503)
		return
	}
	session := adminSession{CSRF: csrf, ExpiresAt: now.Add(adminTTL)}
	a.sessions[tokenHash(sessionToken)] = session
	http.SetCookie(w, &http.Cookie{Name: adminCookie, Value: sessionToken, Path: "/", Expires: session.ExpiresAt, MaxAge: int(adminTTL.Seconds()), Secure: r.TLS != nil, HttpOnly: true, SameSite: http.SameSiteStrictMode})
	adminJSON(w, 200, session)
}

type adminNode struct {
	ID              string               `json:"id"`
	Name            string               `json:"name"`
	Session         string               `json:"session"`
	Freshness       string               `json:"freshness"`
	LastSeen        *time.Time           `json:"last_seen"`
	MetricsAt       *time.Time           `json:"metrics_at"`
	IntervalSeconds int                  `json:"interval_seconds"`
	Metrics         *PublicMetrics       `json:"metrics"`
	FRPSummary      PublicFRP            `json:"frp_summary"`
	Facts           *shared.BrowserFacts `json:"facts"`
	FRP             *shared.FRP          `json:"frp"`
	FRPBinding      *shared.FRPBinding   `json:"frp_binding"`
}
type adminSnapshot struct {
	GeneratedAt           time.Time      `json:"generated_at"`
	Nodes                 []adminNode    `json:"nodes"`
	CredentialsState      string         `json:"credentials_state"`
	ProbesState           string         `json:"probes_state"`
	AdminCredentialsState string         `json:"admin_credentials_state"`
	FRP                   Reconciliation `json:"frp"`
}

func (s *Service) adminSnapshot() adminSnapshot {
	now := time.Now()
	public := s.snapshot(now)
	out := adminSnapshot{GeneratedAt: now.UTC(), Nodes: []adminNode{}, CredentialsState: "ready", AdminCredentialsState: "ready", ProbesState: "ready"}
	if s.cfg.ProbeTasksFile == "" {
		out.ProbesState = "disabled"
	} else if s.taskError.Load() {
		out.ProbesState = "degraded"
	}
	if s.credentialError.Load() {
		out.CredentialsState = "degraded"
	}
	if s.admin != nil {
		s.admin.mu.Lock()
		if s.admin.degraded {
			out.AdminCredentialsState = "degraded"
		}
		s.admin.mu.Unlock()
	}
	reconcile := make([]ReconcileNode, 0, len(public.Nodes))
	s.mu.Lock()
	for _, p := range public.Nodes {
		n := s.nodes[p.ID]
		if n == nil {
			continue
		}
		var facts *shared.BrowserFacts
		if n.facts != nil {
			b, err := n.facts.Browser()
			if err == nil {
				facts = &b
			}
		}
		out.Nodes = append(out.Nodes, adminNode{p.ID, p.Name, p.Session, p.Freshness, p.LastSeen, p.MetricsAt, p.IntervalSeconds, p.Metrics, p.FRP, facts, n.frp, n.credential.FRPBinding})
		reconcile = append(reconcile, ReconcileNode{ID: p.ID, Binding: n.credential.FRPBinding, Report: n.frp, Fresh: n.conn != nil && p.Freshness == "fresh"})
	}
	s.mu.Unlock()
	out.FRP = Reconcile(s.currentServerSnapshot(now), reconcile)
	return out
}
func validName(name string) bool {
	return strings.TrimSpace(name) != "" && len(name) <= 128 && !strings.ContainsAny(name, "\x00\r\n")
}
func (s *Service) createNode(w http.ResponseWriter, r *http.Request) {
	var input struct {
		Name       string             `json:"name"`
		FRPBinding *shared.FRPBinding `json:"frp_binding,omitempty"`
	}
	if !decodeAdmin(w, r, &input) {
		return
	}
	if !validName(input.Name) || (input.FRPBinding != nil && input.FRPBinding.Validate() != nil) {
		http.Error(w, "invalid node", 400)
		return
	}
	id, err := randomToken()
	if err != nil {
		http.Error(w, "unavailable", 503)
		return
	}
	token, err := randomToken()
	if err != nil {
		http.Error(w, "unavailable", 503)
		return
	}
	s.configMu.Lock()
	defer s.configMu.Unlock()
	creds, err := readCredentials(s.cfg.CredentialsFile)
	if err != nil {
		http.Error(w, "credentials unavailable", 503)
		return
	}
	if len(creds) >= maxNodes {
		http.Error(w, "node limit", 409)
		return
	}
	creds = append(creds, credential{AgentID: id, Name: input.Name, TokenSHA256: tokenHash(token), FRPBinding: input.FRPBinding})
	if writePrivate(s.cfg.CredentialsFile, creds) != nil {
		http.Error(w, "credentials unavailable", 503)
		return
	}
	s.applyCredentials(creds)
	adminJSON(w, 201, map[string]string{"id": id, "name": input.Name, "token": token})
}
func (s *Service) mutateNode(w http.ResponseWriter, r *http.Request, path string) {
	parts := strings.Split(path, "/")
	if len(parts) > 2 || !idPattern.MatchString(parts[0]) {
		http.NotFound(w, r)
		return
	}
	id := parts[0]
	action := ""
	if len(parts) == 2 {
		action = parts[1]
	}
	if !(action == "" && r.Method == http.MethodDelete || action == "rotate" && r.Method == http.MethodPost || action == "binding" && (r.Method == http.MethodPut || r.Method == http.MethodDelete)) {
		w.WriteHeader(405)
		return
	}
	var binding *shared.FRPBinding
	if action == "binding" && r.Method == http.MethodPut {
		binding = &shared.FRPBinding{}
		if !decodeAdmin(w, r, binding) {
			return
		}
		if binding.Validate() != nil {
			http.Error(w, "invalid binding", 400)
			return
		}
	}
	var token string
	var err error
	if action == "rotate" {
		token, err = randomToken()
		if err != nil {
			http.Error(w, "unavailable", 503)
			return
		}
	}
	s.configMu.Lock()
	defer s.configMu.Unlock()
	creds, err := readCredentials(s.cfg.CredentialsFile)
	if err != nil {
		http.Error(w, "credentials unavailable", 503)
		return
	}
	index := -1
	for i, c := range creds {
		if c.AgentID == id {
			index = i
			break
		}
	}
	if index < 0 {
		http.NotFound(w, r)
		return
	}
	switch action {
	case "":
		creds = append(creds[:index], creds[index+1:]...)
	case "rotate":
		creds[index].TokenSHA256 = tokenHash(token)
	case "binding":
		creds[index].FRPBinding = binding
	}
	if writePrivate(s.cfg.CredentialsFile, creds) != nil {
		http.Error(w, "credentials unavailable", 503)
		return
	}
	s.applyCredentials(creds)
	if action == "rotate" {
		adminJSON(w, 200, map[string]string{"id": id, "token": token})
		return
	}
	w.WriteHeader(204)
}
func (s *Service) handleAdminProbes(w http.ResponseWriter, r *http.Request) {
	if s.cfg.ProbeTasksFile == "" {
		http.NotFound(w, r)
		return
	}
	if r.Method != http.MethodGet && r.Method != http.MethodPut {
		w.WriteHeader(405)
		return
	}
	s.configMu.Lock()
	defer s.configMu.Unlock()
	if r.Method == http.MethodPut {
		var input probeFile
		if !decodeAdmin(w, r, &input) {
			return
		}
		next, err := s.validateTasks(input)
		if err != nil {
			http.Error(w, "invalid probes", 400)
			return
		}
		if next.Version <= s.tasks.Load().Version {
			http.Error(w, "increase version", 409)
			return
		}
		if writePrivate(s.cfg.ProbeTasksFile, input) != nil {
			http.Error(w, "probe configuration unavailable", 503)
			return
		}
		s.tasks.Store(next)
		s.taskError.Store(false)
	}
	book := s.tasks.Load()
	cfg := probeDocument(book)
	adminJSON(w, 200, cfg)
}
func (s *Service) handleAdminEvents(w http.ResponseWriter, r *http.Request) {
	publicHeaders(w)
	if s.admin == nil {
		http.NotFound(w, r)
		return
	}
	if r.Method != http.MethodGet {
		w.WriteHeader(405)
		return
	}
	if !sameOrigin(r) {
		http.Error(w, "same origin required", 403)
		return
	}
	if _, _, ok := s.session(r); !ok {
		http.Error(w, "unauthorized", 401)
		return
	}
	select {
	case s.streams <- struct{}{}:
		defer func() { <-s.streams }()
	default:
		http.Error(w, "try later", 503)
		return
	}
	w.Header().Set("Content-Type", "text/event-stream; charset=utf-8")
	w.Header().Set("X-Accel-Buffering", "no")
	controller := http.NewResponseController(w)
	send := func() bool {
		if _, _, ok := s.session(r); !ok {
			return false
		}
		data, err := json.Marshal(s.adminSnapshot())
		if err != nil {
			return false
		}
		if controller.SetWriteDeadline(time.Now().Add(5*time.Second)) != nil {
			return false
		}
		if _, err = fmt.Fprintf(w, "event: snapshot\ndata: %s\n\n", data); err != nil {
			return false
		}
		return controller.Flush() == nil
	}
	if !send() {
		return
	}
	ticker := time.NewTicker(time.Second)
	defer ticker.Stop()
	for {
		select {
		case <-s.ctx.Done():
			return
		case <-r.Context().Done():
			return
		case <-ticker.C:
			if !send() {
				return
			}
		}
	}
}

func (s *Service) serverLoop() {
	defer s.wg.Done()
	if s.serverProvider == nil {
		return
	}
	update := func() {
		defer func() {
			if recover() != nil {
				s.serverSnapshot.Store(&shared.ServerSnapshot{})
			}
		}()
		v := s.serverProvider()
		s.serverSnapshot.Store(&v)
	}
	update()
	ticker := time.NewTicker(time.Second)
	defer ticker.Stop()
	for {
		select {
		case <-s.ctx.Done():
			return
		case <-ticker.C:
			update()
		}
	}
}

// A delayed sampler cannot keep an old ready registry looking current forever.
// Sampling remains synchronous and bounded to one worker; callers never wait on
// the provider and no timeout goroutine is created per tick.
func (s *Service) currentServerSnapshot(now time.Time) shared.ServerSnapshot {
	cached := s.serverSnapshot.Load()
	if cached == nil {
		return shared.ServerSnapshot{State: "unavailable"}
	}
	copy := *cached
	age := now.Sub(copy.GeneratedAt)
	if copy.GeneratedAt.IsZero() || age > 5*time.Second || age < -5*time.Second {
		copy.State = "unavailable"
		copy.Clients = nil
		copy.Proxies = nil
	}
	return copy
}
