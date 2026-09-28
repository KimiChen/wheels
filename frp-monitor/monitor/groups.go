package monitor

import (
	"context"
	"errors"
	"net/http"
	"time"

	"github.com/fatedier/frp/extension/frpmonitor/monitor/control"
)

// Only the group identity and display name belong in a public node. Membership
// lists, including hidden nodes, are available exclusively to administrators.
type NodeGroupRef struct {
	ID   string `json:"id"`
	Name string `json:"name"`
}

type groupBook struct {
	Groups []control.NodeGroup
	ByNode map[string][]NodeGroupRef
	Failed bool
}

// Published books are immutable. Replacing even a failed book invalidates an
// in-flight background read, which must not restore a deleted group.
func (s *Service) applyGroups(groups []control.NodeGroup, err error) {
	book := &groupBook{Groups: []control.NodeGroup{}, ByNode: map[string][]NodeGroupRef{}, Failed: err != nil}
	if err == nil {
		for _, group := range groups {
			book.Groups = append(book.Groups, group)
			for _, id := range group.NodeIDs {
				book.ByNode[id] = append(book.ByNode[id], NodeGroupRef{ID: group.ID, Name: group.Name})
			}
		}
	}
	s.groups.Store(book)
	s.invalidateAdminSnapshot()
}

func (s *Service) handleAdminGroups(w http.ResponseWriter, r *http.Request, id string) {
	if id == "" && r.Method == http.MethodGet {
		book := s.groups.Load()
		if book == nil || book.Failed {
			http.Error(w, "groups unavailable", http.StatusServiceUnavailable)
			return
		}
		adminJSON(w, http.StatusOK, map[string]any{"groups": book.Groups})
		return
	}
	if id == "" && r.Method != http.MethodPost {
		methodNotAllowed(w, http.MethodGet, http.MethodPost)
		return
	}
	if id != "" {
		if !validNodeID(id) {
			http.NotFound(w, r)
			return
		}
		if r.Method != http.MethodPatch && r.Method != http.MethodDelete {
			methodNotAllowed(w, http.MethodPatch, http.MethodDelete)
			return
		}
	}
	var input struct {
		Name           string   `json:"name"`
		NodeIDs        []string `json:"node_ids"`
		ConfigRevision int64    `json:"config_revision"`
	}
	if r.Method == http.MethodDelete {
		var revision struct {
			ConfigRevision int64 `json:"config_revision"`
		}
		if !decodeAdmin(w, r, &revision) {
			return
		}
		input.ConfigRevision = revision.ConfigRevision
	} else if !decodeAdmin(w, r, &input) {
		return
	}
	if (id != "" && input.ConfigRevision <= 0) || (r.Method != http.MethodDelete && input.NodeIDs == nil) {
		http.Error(w, "invalid group", http.StatusBadRequest)
		return
	}
	group, err := func() (*control.NodeGroup, error) {
		s.configMu.Lock()
		defer s.configMu.Unlock()
		ctx, cancel := context.WithTimeout(r.Context(), 3*time.Second)
		defer cancel()
		var group *control.NodeGroup
		var err error
		switch r.Method {
		case http.MethodPost:
			group, err = s.control.CreateGroup(ctx, input.Name, input.NodeIDs)
		case http.MethodPatch:
			group, err = s.control.UpdateGroup(ctx, id, input.Name, input.NodeIDs, input.ConfigRevision)
		case http.MethodDelete:
			err = s.control.DeleteGroup(ctx, id, input.ConfigRevision)
		}
		if err != nil {
			// An uncertain commit must not leave old membership visible. Group
			// changes have no effect on agent credentials or FRP connections.
			if errors.Is(err, context.Canceled) || errors.Is(err, context.DeadlineExceeded) {
				s.applyGroups(nil, err)
				s.publishSnapshot(time.Now())
			}
			return nil, err
		}
		groups, readErr := s.control.Groups(ctx)
		s.applyGroups(groups, readErr)
		s.publishSnapshot(time.Now())
		return group, nil
	}()
	if err != nil {
		controlError(w, r, err)
		return
	}
	if r.Method == http.MethodDelete {
		w.WriteHeader(http.StatusNoContent)
		return
	}
	status := http.StatusOK
	if r.Method == http.MethodPost {
		status = http.StatusCreated
	}
	adminJSON(w, status, group)
}
