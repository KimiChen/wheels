package monitor

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"reflect"
	"sort"
	"strings"
	"time"

	"github.com/fatedier/frp/extension/frpmonitor/monitor/control"
	"github.com/fatedier/frp/extension/frpmonitor/shared"
	"github.com/gorilla/websocket"
)

// This is local operator configuration. Only the explicit name is public;
// targets and the file path never enter a browser response.
type configuredProbe struct {
	ID       string `json:"id"`
	Name     string `json:"name"`
	Target   string `json:"target"`
	Interval uint32 `json:"interval"`
}
type probeBook struct {
	Version uint64
	Nodes   map[string][]configuredProbe
}
type probeFile struct {
	Version uint64 `json:"version"`
	Nodes   []struct {
		AgentID string            `json:"agent_id"`
		Tasks   []configuredProbe `json:"tasks"`
	} `json:"nodes"`
}

func (s *Service) reloadTasks() {
	next, err := s.readTasks()
	s.applyTasks(next, err)
}

// Caller holds configMu once the service is running.
func (s *Service) applyTasks(next *probeBook, err error) {
	if err != nil {
		s.taskError.Store(true)
		s.invalidateAdminSnapshot()
		return
	}
	previous := s.tasks.Load()
	if next.Version < previous.Version || (next.Version == previous.Version && !reflect.DeepEqual(next.Nodes, previous.Nodes)) {
		s.taskError.Store(true)
		s.invalidateAdminSnapshot()
		return
	}
	if next.Version > previous.Version {
		s.tasks.Store(next)
	}
	s.taskError.Store(false)
	s.invalidateAdminSnapshot()
}

func (s *Service) readTasks() (*probeBook, error) {
	invalid := errors.New("invalid local probe task configuration")
	ctx, cancel := context.WithTimeout(s.ctx, 2*time.Second)
	defer cancel()
	data, err := s.control.ReadProbes(ctx)
	if err != nil {
		return nil, invalid
	}
	return s.parseTasks(data)
}

func (s *Service) parseTasks(data []byte) (*probeBook, error) {
	invalid := errors.New("invalid local probe task configuration")
	var cfg probeFile
	if strictJSON(data, &cfg) != nil {
		return nil, invalid
	}
	return s.validateTasks(cfg)
}
func (s *Service) validateTasks(cfg probeFile) (*probeBook, error) {
	invalid := errors.New("invalid local probe task configuration")
	if cfg.Version == 0 || cfg.Nodes == nil || len(cfg.Nodes) > maxNodes {
		return nil, invalid
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	book := &probeBook{Version: cfg.Version, Nodes: make(map[string][]configuredProbe)}
	for _, n := range cfg.Nodes {
		if _, known := s.nodes[n.AgentID]; !known {
			return nil, invalid
		}
		if _, duplicate := book.Nodes[n.AgentID]; duplicate || n.Tasks == nil {
			return nil, invalid
		}
		params := shared.PingTasks{Meta: shared.Meta{Schema: shared.SchemaVersion, SessionID: "local-validation", Sequence: 1, CollectedAt: time.Now().UTC().Format(time.RFC3339Nano)}, Version: cfg.Version, Tasks: make([]shared.PingTask, 0, len(n.Tasks))}
		for _, task := range n.Tasks {
			if strings.TrimSpace(task.Name) == "" || len(task.Name) > 128 || strings.ContainsAny(task.Name, "\x00\r\n") {
				return nil, invalid
			}
			params.Tasks = append(params.Tasks, shared.PingTask{ID: task.ID, Target: task.Target, Interval: task.Interval})
		}
		if params.Validate() != nil {
			return nil, invalid
		}
		book.Nodes[n.AgentID] = n.Tasks
	}
	return book, nil
}
func (s *Service) taskLoop() {
	defer s.wg.Done()
	defer func() {
		if recover() != nil {
			s.taskError.Store(true)
		}
	}()
	ticker := time.NewTicker(2 * time.Second)
	defer ticker.Stop()
	for {
		select {
		case <-s.ctx.Done():
			return
		case <-ticker.C:
			s.refreshConfiguration()
		}
	}
}

// Database waits happen outside configMu. A concurrent management change
// replaces one of these immutable pointers; discard the old read in that case
// so a pre-revocation snapshot cannot restore a removed credential or task.
func (s *Service) refreshConfiguration() {
	read := configurationRead{configs: s.configs.Load(), tasks: s.tasks.Load(), groups: s.groups.Load()}
	ctx, cancel := context.WithTimeout(s.ctx, 2*time.Second)
	read.nodes, read.nodeErr = s.control.Nodes(ctx)
	read.taskData, read.taskErr = s.control.ReadProbes(ctx)
	read.groupData, read.groupErr = s.control.Groups(ctx)
	cancel()
	s.reloadAdmin()
	s.applyConfiguration(read)
}

type configurationRead struct {
	configs   *nodeConfigs
	tasks     *probeBook
	groups    *groupBook
	nodes     []*control.Node
	nodeErr   error
	taskData  []byte
	taskErr   error
	groupData []control.NodeGroup
	groupErr  error
}

func (s *Service) applyConfiguration(read configurationRead) {
	s.configMu.Lock()
	defer s.configMu.Unlock()
	if read.configs != s.configs.Load() || read.tasks != s.tasks.Load() || read.groups != s.groups.Load() {
		return
	}
	s.applyGroups(read.groupData, read.groupErr)
	if read.nodeErr != nil {
		s.credentialError.Store(true)
		s.invalidateAdminSnapshot()
	} else {
		s.applyNodes(read.nodes)
	}
	var next *probeBook
	if read.taskErr == nil {
		next, read.taskErr = s.parseTasks(read.taskData)
	}
	s.applyTasks(next, read.taskErr)
}
func hasCapability(values []string, name string) bool {
	for _, value := range values {
		if value == name {
			return true
		}
	}
	return false
}
func sendTasks(c *websocket.Conn, book *probeBook, id, session string, sequence uint64) error {
	params := shared.PingTasks{Meta: shared.Meta{Schema: shared.SchemaVersion, SessionID: session, Sequence: sequence, CollectedAt: time.Now().UTC().Format(time.RFC3339Nano)}, Version: book.Version, Tasks: []shared.PingTask{}}
	for _, t := range book.Nodes[id] {
		params.Tasks = append(params.Tasks, shared.PingTask{ID: t.ID, Target: t.Target, Interval: t.Interval})
	}
	_ = c.SetWriteDeadline(time.Now().Add(5 * time.Second))
	return c.WriteJSON(struct {
		JSONRPC string           `json:"jsonrpc"`
		Method  string           `json:"method"`
		Params  shared.PingTasks `json:"params"`
	}{"2.0", "ping.tasks", params})
}
func probeKey(task configuredProbe) string {
	// A changed target cannot silently inherit another target's history. The
	// digest itself remains internal; public task IDs and names come from config.
	h := sha256.Sum256([]byte(task.ID + "\x00" + task.Target))
	return hex.EncodeToString(h[:])
}
func (s *Service) acceptProbe(id string, c *websocket.Conn, now time.Time, result shared.PingResult) bool {
	s.mu.Lock()
	defer s.mu.Unlock()
	n := s.nodes[id]
	if n == nil || n.conn != c || result.SessionID != n.sessionID || result.Sequence <= n.sequence {
		return false
	}
	book := s.tasks.Load()
	if result.TaskVersion > book.Version {
		return false
	}
	n.sequence = result.Sequence
	n.lastSeen = now
	// Results already in flight during a full-list replacement are discarded.
	if result.TaskVersion < book.Version {
		return true
	}
	for _, task := range book.Nodes[id] {
		if task.ID == result.TaskID {
			if s.store != nil {
				s.store.AcceptProbe(id, probeKey(task), now, result.LatencyMS)
			}
			return true
		}
	}
	return false
}

func probeDocument(book *probeBook) probeFile {
	out := probeFile{Version: book.Version, Nodes: make([]struct {
		AgentID string            `json:"agent_id"`
		Tasks   []configuredProbe `json:"tasks"`
	}, 0, len(book.Nodes))}
	for id, tasks := range book.Nodes {
		out.Nodes = append(out.Nodes, struct {
			AgentID string            `json:"agent_id"`
			Tasks   []configuredProbe `json:"tasks"`
		}{id, tasks})
	}
	sort.Slice(out.Nodes, func(i, j int) bool { return out.Nodes[i].AgentID < out.Nodes[j].AgentID })
	return out
}
