package session

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"time"

	"github.com/cascadecodes/banya-cli/internal/config"
)

// HistoryEntry is a summary of a past session for quick listing.
type HistoryEntry struct {
	SessionID   string    `json:"session_id"`
	FirstMessage string   `json:"first_message"`
	MessageCount int      `json:"message_count"`
	CreatedAt   time.Time `json:"created_at"`
	UpdatedAt   time.Time `json:"updated_at"`
}

// History manages the session history index.
type History struct {
	dataDir string
	entries []HistoryEntry
}

// NewHistory creates a new history manager.
func NewHistory() *History {
	return &History{
		dataDir: filepath.Join(config.DataDir(), "history"),
	}
}

// Add records a session in the history index.
func (h *History) Add(entry HistoryEntry) error {
	if err := h.load(); err != nil {
		return err
	}

	// Update existing or append
	found := false
	for i, e := range h.entries {
		if e.SessionID == entry.SessionID {
			h.entries[i] = entry
			found = true
			break
		}
	}
	if !found {
		h.entries = append(h.entries, entry)
	}

	return h.save()
}

// List returns recent history entries.
func (h *History) List(limit int) ([]HistoryEntry, error) {
	if err := h.load(); err != nil {
		return nil, err
	}

	if limit <= 0 || limit > len(h.entries) {
		limit = len(h.entries)
	}

	// Return most recent first (entries are appended chronologically)
	result := make([]HistoryEntry, limit)
	for i := 0; i < limit; i++ {
		result[i] = h.entries[len(h.entries)-1-i]
	}
	return result, nil
}

func (h *History) load() error {
	path := filepath.Join(h.dataDir, "history.json")
	data, err := os.ReadFile(path)
	if err != nil {
		if os.IsNotExist(err) {
			h.entries = nil
			return nil
		}
		return fmt.Errorf("read history: %w", err)
	}
	return json.Unmarshal(data, &h.entries)
}

func (h *History) save() error {
	if err := os.MkdirAll(h.dataDir, 0o755); err != nil {
		return fmt.Errorf("create history dir: %w", err)
	}

	data, err := json.MarshalIndent(h.entries, "", "  ")
	if err != nil {
		return fmt.Errorf("marshal history: %w", err)
	}

	path := filepath.Join(h.dataDir, "history.json")
	return os.WriteFile(path, data, 0o644)
}
