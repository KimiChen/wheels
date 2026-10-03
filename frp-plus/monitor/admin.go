package monitor

import (
	"context"
	"crypto/rand"
	"crypto/sha256"
	"crypto/subtle"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"io"
	"mime"
	"net/http"
	"strings"
	"sync"
	"time"

	"github.com/fatedier/frp/extension/frpmonitor/monitor/control"
	"github.com/fatedier/frp/extension/frpmonitor/shared"
)

const adminCookie = "frp_monitor_admin"
const adminTTL = 8 * time.Hour

type adminSession struct {
	CSRF      string    `json:"csrf_token"`
	ExpiresAt time.Time `json:"expires_at"`
	Login     string    `json:"login"`
}
type adminState struct {
	mu       sync.Mutex
	sessions map[string]adminSession
	tokens   float64
	at       time.Time
	github   *githubAuth
}

func (s *Service) reloadAdmin() {
	if s.admin == nil {
		return
	}
	s.admin.mu.Lock()
	defer s.admin.mu.Unlock()
	now := time.Now()
	for id, session := range s.admin.sessions {
		if !now.Before(session.ExpiresAt) {
			delete(s.admin.sessions, id)
		}
	}
	for id, attempt := range s.admin.github.pending {
		if !now.Before(attempt.expires) {
			delete(s.admin.github.pending, id)
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
func methodNotAllowed(w http.ResponseWriter, methods ...string) {
	w.Header().Set("Allow", strings.Join(methods, ", "))
	w.WriteHeader(http.StatusMethodNotAllowed)
}
func decodeAdmin(w http.ResponseWriter, r *http.Request, target any) bool {
	mediaType, _, err := mime.ParseMediaType(r.Header.Get("Content-Type"))
	if err != nil || !strings.EqualFold(mediaType, "application/json") {
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
	path := strings.TrimPrefix(r.URL.Path, "/api/admin/v1/")
	if path == "auth" {
		if r.Method != http.MethodGet {
			methodNotAllowed(w, http.MethodGet)
			return
		}
		adminJSON(w, 200, map[string]any{"provider": "github", "enabled": s.admin != nil})
		return
	}
	if s.admin == nil || path == "login" {
		http.NotFound(w, r)
		return
	}
	if path == "auth/github" {
		s.githubStart(w, r)
		return
	}
	if path == "auth/github/callback" {
		s.githubCallback(w, r)
		return
	}
	if !s.adminSameOrigin(r) {
		http.Error(w, "same origin required", http.StatusForbidden)
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
	if s.handleTunnelAdmin(w, r, path) {
		return
	}
	if s.handleAuditAdmin(w, r, path, session.Login) {
		return
	}
	if s.handleConfigAdmin(w, r, path, session.Login) {
		return
	}
	switch path {
	case "session":
		if r.Method != http.MethodGet {
			methodNotAllowed(w, http.MethodGet)
			return
		}
		adminJSON(w, 200, session)
	case "logout":
		if r.Method != http.MethodPost {
			methodNotAllowed(w, http.MethodPost)
			return
		}
		s.admin.mu.Lock()
		delete(s.admin.sessions, key)
		s.admin.mu.Unlock()
		http.SetCookie(w, &http.Cookie{Name: adminCookie, Path: "/", MaxAge: -1, Secure: s.secureAdminCookie(r), HttpOnly: true, SameSite: http.SameSiteStrictMode})
		w.WriteHeader(204)
	case "nodes":
		if r.Method == http.MethodGet {
			data := s.adminSnapshotBytes()
			if data == nil {
				http.Error(w, "snapshot unavailable", 503)
				return
			}
			w.Header().Set("Content-Type", "application/json; charset=utf-8")
			_, _ = w.Write(data)
			return
		}
		if r.Method == http.MethodPost {
			s.createNode(w, r)
			return
		}
		methodNotAllowed(w, http.MethodGet, http.MethodPost)
	case "probes":
		s.handleAdminProbes(w, r)
	case "groups":
		s.handleAdminGroups(w, r, "")
	default:
		if strings.HasPrefix(path, "nodes/") && strings.HasSuffix(path, "/frp-detail") {
			id := strings.TrimSuffix(strings.TrimPrefix(path, "nodes/"), "/frp-detail")
			if !validNodeID(id) {
				http.NotFound(w, r)
				return
			}
			s.handleFRPDetail(w, r, id)
			return
		}
		if strings.HasPrefix(path, "groups/") {
			id := strings.TrimPrefix(path, "groups/")
			if !validNodeID(id) {
				http.NotFound(w, r)
				return
			}
			s.handleAdminGroups(w, r, id)
			return
		}
		if strings.HasPrefix(path, "nodes/") {
			s.mutateNode(w, r, strings.TrimPrefix(path, "nodes/"))
			return
		}
		http.NotFound(w, r)
	}
}

type adminNode struct {
	ID              string               `json:"id"`
	Name            string               `json:"name"`
	Groups          []NodeGroupRef       `json:"groups"`
	Session         string               `json:"session"`
	Freshness       string               `json:"freshness"`
	LastSeen        *time.Time           `json:"last_seen"`
	MetricsAt       *time.Time           `json:"metrics_at"`
	IntervalSeconds int                  `json:"interval_seconds"`
	Metrics         *PublicMetrics       `json:"metrics"`
	FRPSummary      PublicFRP            `json:"frp_summary"`
	Facts           *shared.BrowserFacts `json:"facts"`
	FRP             *shared.FRP          `json:"frp"`
	FRPDetailState  string               `json:"frp_detail_state"`
	FRPDetailAt     *time.Time           `json:"frp_detail_at"`
	FRPBinding      *shared.FRPBinding   `json:"frp_binding"`
	Settings        *nodeSettings        `json:"settings"`
	Billing         *nodeBilling         `json:"billing"`
	TrafficToday    *nodeToday           `json:"traffic_today"`
	TrafficPlan     *nodePlan            `json:"traffic_plan"`
}
type adminSnapshot struct {
	GeneratedAt      time.Time           `json:"generated_at"`
	Nodes            []adminNode         `json:"nodes"`
	CredentialsState string              `json:"credentials_state"`
	ProbesState      string              `json:"probes_state"`
	GroupsState      string              `json:"groups_state"`
	Groups           []control.NodeGroup `json:"groups"`
	FRP              Reconciliation      `json:"frp"`
	NativeAccess     nativeAccess        `json:"native_access"`
}

type adminEncodedSnapshot struct {
	generation uint64
	data       []byte
}

// Configuration mutations invalidate without acquiring the publishing lock.
// Readers must match generations even if a previous encoder finishes late.
func (s *Service) invalidateAdminSnapshot() { s.adminGeneration.Add(1) }

func (s *Service) publishAdminSnapshot() { s.encodeAdminSnapshot(true) }

func (s *Service) encodeAdminSnapshot(force bool) {
	if s.admin == nil {
		return
	}
	// Lock order: adminPublishMu -> configMu -> mu. Mutations never acquire
	// adminPublishMu while holding configMu; they only advance the generation.
	s.adminPublishMu.Lock()
	defer s.adminPublishMu.Unlock()
	if cached := s.adminJSON.Load(); !force && cached != nil && cached.generation == s.adminGeneration.Load() {
		return
	}
	s.configMu.Lock()
	generation := s.adminGeneration.Load()
	next := s.adminSnapshotLocked()
	s.configMu.Unlock()
	data, err := json.Marshal(next)
	if err == nil && generation == s.adminGeneration.Load() {
		s.adminJSON.Store(&adminEncodedSnapshot{generation: generation, data: data})
	}
}

func (s *Service) adminSnapshotBytes() []byte {
	// Retry a concurrent invalidation once. Continuous mutations must not leave
	// a request generating snapshots forever or make it serve stale private data.
	for attempt := 0; attempt < 2; attempt++ {
		if cached := s.adminJSON.Load(); cached != nil && cached.generation == s.adminGeneration.Load() {
			return cached.data
		}
		s.encodeAdminSnapshot(false)
	}
	if cached := s.adminJSON.Load(); cached != nil && cached.generation == s.adminGeneration.Load() {
		return cached.data
	}
	return nil
}

func (s *Service) adminSnapshot() adminSnapshot {
	// Settings/revision and billing/usage must describe the same configuration.
	// Otherwise an editor could submit old calibrated usage with a newer revision.
	s.configMu.Lock()
	defer s.configMu.Unlock()
	return s.adminSnapshotLocked()
}

// Caller holds configMu so settings/revisions and derived usage agree.
func (s *Service) adminSnapshotLocked() adminSnapshot {
	now := time.Now()
	public := s.snapshotFor(now, true)
	out := adminSnapshot{GeneratedAt: now.UTC(), Nodes: []adminNode{}, CredentialsState: "ready", ProbesState: "ready"}
	out.NativeAccess = s.nativeAccessSnapshot()
	out.Groups = []control.NodeGroup{}
	out.GroupsState = "degraded"
	if groups := s.groups.Load(); groups != nil && !groups.Failed {
		out.Groups, out.GroupsState = groups.Groups, "ready"
	}
	if s.taskError.Load() {
		out.ProbesState = "degraded"
	}
	if s.credentialError.Load() {
		out.CredentialsState = "degraded"
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
		row := adminNode{ID: p.ID, Name: p.Name, Session: p.Session, Freshness: p.Freshness, LastSeen: p.LastSeen, MetricsAt: p.MetricsAt, IntervalSeconds: p.IntervalSeconds, Metrics: p.Metrics, FRPSummary: p.FRP, Facts: facts, FRP: n.frp, FRPBinding: n.credential.FRPBinding, Billing: p.Billing, TrafficToday: p.TrafficToday, TrafficPlan: p.TrafficPlan}
		row.Groups = p.Groups
		_, row.FRPDetailState, row.FRPDetailAt = privateDetail(n, now, p.IntervalSeconds)
		if configs := s.configs.Load(); configs != nil {
			if config := (*configs)[p.ID]; config != nil {
				row.Settings = &nodeSettings{config.NodeConfig, config.ConfigRevision}
			}
		}
		out.Nodes = append(out.Nodes, row)
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
	token, err := randomToken()
	if err != nil {
		http.Error(w, "unavailable", 503)
		return
	}
	created, err := func() (*control.Node, error) {
		s.configMu.Lock()
		defer s.configMu.Unlock()
		ctx, cancel := context.WithTimeout(r.Context(), 3*time.Second)
		defer cancel()
		created, err := s.control.CreateNode(ctx, control.DefaultNodeConfig(input.Name), tokenHash(token), input.FRPBinding)
		if err != nil {
			s.controlWriteError(err)
			return nil, err
		}
		s.refreshCommitted(ctx)
		return created, nil
	}()
	if err != nil {
		controlError(w, r, err)
		return
	}
	adminJSON(w, 201, map[string]string{"id": created.ID, "name": created.Name, "token": token})

}
func (s *Service) mutateNode(w http.ResponseWriter, r *http.Request, path string) {
	parts := strings.Split(path, "/")
	if len(parts) > 2 || !validNodeID(parts[0]) {
		http.NotFound(w, r)
		return
	}
	id := parts[0]
	action := ""
	if len(parts) == 2 {
		action = parts[1]
	}
	if action == "settings" || action == "reset-traffic" {
		s.updateSettings(w, r, id, action == "reset-traffic")
		return
	}
	var methods []string
	switch action {
	case "":
		methods = []string{http.MethodDelete}
	case "rotate":
		methods = []string{http.MethodPost}
	case "binding":
		methods = []string{http.MethodPut, http.MethodDelete}
	default:
		http.NotFound(w, r)
		return
	}
	if !(action == "" && r.Method == http.MethodDelete || action == "rotate" && r.Method == http.MethodPost || action == "binding" && (r.Method == http.MethodPut || r.Method == http.MethodDelete)) {
		methodNotAllowed(w, methods...)
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
	err = func() error {
		s.configMu.Lock()
		defer s.configMu.Unlock()
		ctx, cancel := context.WithTimeout(r.Context(), 3*time.Second)
		defer cancel()
		switch action {
		case "":
			err = s.control.DeleteNode(ctx, id)
		case "rotate":
			err = s.control.RotateToken(ctx, id, tokenHash(token))
		case "binding":
			// Only the binding change needs the current revision for optimistic
			// concurrency; delete and rotate carry no preconditions.
			current, gerr := s.control.Get(ctx, id)
			if gerr != nil {
				return gerr
			}
			err = s.control.SetBinding(ctx, id, binding, current.ConfigRevision)
		}
		if err != nil {
			s.controlWriteError(err)
			return err
		}
		s.refreshCommitted(ctx)
		if action == "" {
			s.reloadTasks()
		}
		return nil
	}()
	if err != nil {
		controlError(w, r, err)
		return
	}
	if action == "rotate" {
		adminJSON(w, 200, map[string]string{"id": id, "token": token})
		return
	}
	w.WriteHeader(204)
}

func (s *Service) handleAdminProbes(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodGet && r.Method != http.MethodPut {
		methodNotAllowed(w, http.MethodGet, http.MethodPut)
		return
	}
	if r.Method == http.MethodGet {
		// Published probe books are immutable; copying one needs no config lock.
		adminJSON(w, 200, probeDocument(s.tasks.Load()))
		return
	}
	var input probeFile
	if !decodeAdmin(w, r, &input) {
		return
	}
	data, err := json.Marshal(input)
	if err != nil {
		http.Error(w, "probe configuration unavailable", 503)
		return
	}
	// Hold the lock only for validation against the current nodes/version,
	// durable mutation and publication; slow request/response bodies stay outside.
	s.configMu.Lock()
	next, err := s.validateTasks(input)
	if err != nil {
		s.configMu.Unlock()
		http.Error(w, "invalid probes", 400)
		return
	}
	if next.Version <= s.tasks.Load().Version {
		s.configMu.Unlock()
		http.Error(w, "increase version", 409)
		return
	}
	ctx, cancel := context.WithTimeout(r.Context(), 3*time.Second)
	defer cancel()
	if err := s.control.WriteProbes(ctx, data); err != nil {
		s.controlWriteError(err)
		s.configMu.Unlock()
		controlError(w, r, err)
		return
	}
	s.tasks.Store(next)
	s.taskError.Store(false)
	s.invalidateAdminSnapshot()
	s.configMu.Unlock()
	adminJSON(w, 200, probeDocument(next))
}
func (s *Service) handleAdminEvents(w http.ResponseWriter, r *http.Request) {
	publicHeaders(w)
	if s.admin == nil {
		http.NotFound(w, r)
		return
	}
	if r.Method != http.MethodGet {
		methodNotAllowed(w, http.MethodGet)
		return
	}
	if !s.adminSameOrigin(r) {
		http.Error(w, "same origin required", 403)
		return
	}
	if _, _, ok := s.session(r); !ok {
		http.Error(w, "unauthorized", 401)
		return
	}
	select {
	case s.streamsAdmin <- struct{}{}:
		defer func() { <-s.streamsAdmin }()
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
		data := s.adminSnapshotBytes()
		if data == nil {
			return false
		}
		if controller.SetWriteDeadline(time.Now().Add(5*time.Second)) != nil {
			return false
		}
		if _, err := fmt.Fprintf(w, "event: snapshot\ndata: %s\n\n", data); err != nil {
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
		s.sampleServerTunnels()
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
