// Package session manages conversation sessions and local persistence.
package session

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"time"

	"github.com/cascadecodes/banya-cli/internal/config"
	"github.com/cascadecodes/banya-cli/pkg/protocol"
)

// Session represents a conversation session.
type Session struct {
	ID        string             `json:"id"`
	CreatedAt time.Time          `json:"created_at"`
	UpdatedAt time.Time          `json:"updated_at"`
	WorkDir   string             `json:"work_dir"`
	Messages  []protocol.Message `json:"messages"`
}

// Manager handles session persistence.
type Manager struct {
	dataDir string
}

// NewManager creates a new session manager.
func NewManager() *Manager {
	return &Manager{
		dataDir: filepath.Join(config.DataDir(), "sessions"),
	}
}

// Save persists a session to disk.
func (m *Manager) Save(session *Session) error {
	if err := os.MkdirAll(m.dataDir, 0o755); err != nil {
		return fmt.Errorf("create sessions dir: %w", err)
	}

	session.UpdatedAt = time.Now()

	data, err := json.MarshalIndent(session, "", "  ")
	if err != nil {
		return fmt.Errorf("marshal session: %w", err)
	}

	path := filepath.Join(m.dataDir, session.ID+".json")
	if err := os.WriteFile(path, data, 0o644); err != nil {
		return fmt.Errorf("write session: %w", err)
	}
	return nil
}

// Load reads a session from disk.
func (m *Manager) Load(id string) (*Session, error) {
	path := filepath.Join(m.dataDir, id+".json")
	data, err := os.ReadFile(path)
	if err != nil {
		if os.IsNotExist(err) {
			return nil, nil
		}
		return nil, fmt.Errorf("read session: %w", err)
	}

	var session Session
	if err := json.Unmarshal(data, &session); err != nil {
		return nil, fmt.Errorf("unmarshal session: %w", err)
	}
	return &session, nil
}

// List returns all saved sessions, sorted by most recent first.
func (m *Manager) List() ([]Session, error) {
	entries, err := os.ReadDir(m.dataDir)
	if err != nil {
		if os.IsNotExist(err) {
			return nil, nil
		}
		return nil, fmt.Errorf("read sessions dir: %w", err)
	}

	var sessions []Session
	for _, entry := range entries {
		if entry.IsDir() || filepath.Ext(entry.Name()) != ".json" {
			continue
		}
		id := entry.Name()[:len(entry.Name())-5] // strip .json
		s, err := m.Load(id)
		if err != nil || s == nil {
			continue
		}
		sessions = append(sessions, *s)
	}

	sort.Slice(sessions, func(i, j int) bool {
		return sessions[i].UpdatedAt.After(sessions[j].UpdatedAt)
	})

	return sessions, nil
}

// Delete removes a session from disk.
func (m *Manager) Delete(id string) error {
	path := filepath.Join(m.dataDir, id+".json")
	err := os.Remove(path)
	if err != nil && !os.IsNotExist(err) {
		return fmt.Errorf("delete session: %w", err)
	}
	return nil
}
