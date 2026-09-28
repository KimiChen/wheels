package control

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"strconv"
	"strings"
	"unicode"
	"unicode/utf8"
)

func validateGroup(tx *sql.Tx, name string, nodeIDs []string) (string, error) {
	if !utf8.ValidString(name) || strings.ContainsFunc(name, unicode.IsControl) {
		return "", fmt.Errorf("%w: group name", ErrInvalid)
	}
	name = strings.TrimSpace(name)
	if name == "" || len(name) > 128 || len(nodeIDs) > 1024 {
		return "", fmt.Errorf("%w: group name or member limit", ErrInvalid)
	}
	seen := make(map[string]bool, len(nodeIDs))
	for _, id := range nodeIDs {
		if !validID(id) || seen[id] {
			return "", fmt.Errorf("%w: group member ID", ErrInvalid)
		}
		seen[id] = true
		var exists int
		if err := tx.QueryRow("SELECT 1 FROM nodes WHERE id=?", id).Scan(&exists); err != nil {
			if errors.Is(err, sql.ErrNoRows) {
				return "", fmt.Errorf("%w: unknown group member", ErrInvalid)
			}
			return "", err
		}
	}
	return name, nil
}

func readGroup(tx *sql.Tx, id string) (*NodeGroup, error) {
	if !validID(id) {
		return nil, fmt.Errorf("%w: group ID", ErrInvalid)
	}
	g := &NodeGroup{NodeIDs: []string{}}
	err := tx.QueryRow("SELECT id,name,config_revision FROM node_groups WHERE id=?", id).Scan(&g.ID, &g.Name, &g.ConfigRevision)
	if errors.Is(err, sql.ErrNoRows) {
		return nil, ErrNotFound
	}
	if err != nil {
		return nil, err
	}
	rows, err := tx.Query("SELECT node_id FROM node_group_members WHERE group_id=? ORDER BY node_id", id)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	for rows.Next() {
		var nodeID string
		if err := rows.Scan(&nodeID); err != nil {
			return nil, err
		}
		g.NodeIDs = append(g.NodeIDs, nodeID)
	}
	return g, rows.Err()
}

func insertGroupMembers(tx *sql.Tx, groupID string, nodeIDs []string) error {
	for _, nodeID := range nodeIDs {
		if _, err := tx.Exec("INSERT INTO node_group_members(group_id,node_id) VALUES(?,?)", groupID, nodeID); err != nil {
			return err
		}
	}
	return nil
}

// Groups returns one consistent configuration snapshot, ordered by numeric
// group ID and then numeric node ID. Empty groups have an empty member array.
func (s *Store) Groups(ctx context.Context) ([]NodeGroup, error) {
	result := []NodeGroup{}
	err := s.call(ctx, func(tx *sql.Tx) error {
		rows, err := tx.Query(`SELECT g.id,g.name,g.config_revision,m.node_id
 FROM node_groups g LEFT JOIN node_group_members m ON m.group_id=g.id
 ORDER BY g.id,m.node_id`)
		if err != nil {
			return err
		}
		defer rows.Close()
		for rows.Next() {
			var g NodeGroup
			var nodeID sql.NullString
			if err := rows.Scan(&g.ID, &g.Name, &g.ConfigRevision, &nodeID); err != nil {
				return err
			}
			if len(result) == 0 || result[len(result)-1].ID != g.ID {
				g.NodeIDs = []string{}
				result = append(result, g)
			}
			if nodeID.Valid {
				index := len(result) - 1
				result[index].NodeIDs = append(result[index].NodeIDs, nodeID.String)
			}
		}
		return rows.Err()
	})
	if err != nil {
		return nil, err
	}
	return result, nil
}

func (s *Store) CreateGroup(ctx context.Context, name string, nodeIDs []string) (*NodeGroup, error) {
	// The worker may outlive a canceled caller; never retain its mutable slice.
	nodeIDs = append([]string(nil), nodeIDs...)
	var result *NodeGroup
	err := s.call(ctx, func(tx *sql.Tx) error {
		name, err := validateGroup(tx, name, nodeIDs)
		if err != nil {
			return err
		}
		var count int
		if err := tx.QueryRow("SELECT count(*) FROM node_groups").Scan(&count); err != nil {
			return err
		}
		if count >= 128 {
			return fmt.Errorf("%w: group limit exceeded", ErrConflict)
		}
		created, err := tx.Exec("INSERT INTO node_groups(name) VALUES(?)", name)
		if err != nil {
			return err
		}
		id, err := created.LastInsertId()
		if err != nil {
			return err
		}
		groupID := strconv.FormatInt(id, 10)
		if err := insertGroupMembers(tx, groupID, nodeIDs); err != nil {
			return err
		}
		result, err = readGroup(tx, groupID)
		return err
	})
	if err != nil {
		return nil, err
	}
	return result, nil
}

func (s *Store) UpdateGroup(ctx context.Context, id, name string, nodeIDs []string, expectedRevision int64) (*NodeGroup, error) {
	nodeIDs = append([]string(nil), nodeIDs...)
	var result *NodeGroup
	err := s.call(ctx, func(tx *sql.Tx) error {
		g, err := readGroup(tx, id)
		if err != nil {
			return err
		}
		if expectedRevision <= 0 || g.ConfigRevision != expectedRevision {
			return ErrConflict
		}
		name, err = validateGroup(tx, name, nodeIDs)
		if err != nil {
			return err
		}
		if _, err = tx.Exec("UPDATE node_groups SET name=?,config_revision=config_revision+1 WHERE id=?", name, id); err != nil {
			return err
		}
		if _, err = tx.Exec("DELETE FROM node_group_members WHERE group_id=?", id); err != nil {
			return err
		}
		if err = insertGroupMembers(tx, id, nodeIDs); err != nil {
			return err
		}
		result, err = readGroup(tx, id)
		return err
	})
	if err != nil {
		return nil, err
	}
	return result, nil
}

func (s *Store) DeleteGroup(ctx context.Context, id string, expectedRevision int64) error {
	return s.call(ctx, func(tx *sql.Tx) error {
		g, err := readGroup(tx, id)
		if err != nil {
			return err
		}
		if expectedRevision <= 0 || g.ConfigRevision != expectedRevision {
			return ErrConflict
		}
		_, err = tx.Exec("DELETE FROM node_groups WHERE id=?", id)
		return err
	})
}
