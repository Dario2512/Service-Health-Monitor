package monitor

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"os"
	"path/filepath"
	"sync"
	"time"
)

// The store can be written to a JSON file and read back, so a restart of the
// monitor keeps its targets, their check history, their current belief and
// the incidents. Without a file everything lives in memory and a restart
// starts from nothing.

const snapshotVersion = 1

type snapshot struct {
	Version        int              `json:"version"`
	SavedAt        time.Time        `json:"saved_at"`
	NextTargetID   int              `json:"next_target_id"`
	NextIncidentID int              `json:"next_incident_id"`
	Targets        []snapshotTarget `json:"targets"`
	Incidents      []Incident       `json:"incidents"`
}

type snapshotTarget struct {
	Target         Target        `json:"target"`
	State          State         `json:"state"`
	StateSince     time.Time     `json:"state_since"`
	Streak         []Observation `json:"streak"`
	History        []Observation `json:"history"`
	CertExpiresAt  *time.Time    `json:"cert_expires_at,omitempty"`
	OpenIncidentID int           `json:"open_incident_id,omitempty"`
}

// Changes counts mutations, so a saver can tell whether there is anything
// new to write.
func (s *Store) Changes() uint64 {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.changes
}

// Save writes the whole store to path atomically: a crash mid-write leaves
// the previous file intact.
func (s *Store) Save(path string) error {
	s.mu.Lock()
	snap := snapshot{
		Version: snapshotVersion, SavedAt: s.now().UTC(),
		NextTargetID: s.nextTargetID, NextIncidentID: s.nextIncidentID,
	}
	for _, id := range s.order {
		e := s.targets[id]
		st := snapshotTarget{
			Target: e.target, State: e.state, StateSince: e.stateSince,
			Streak: e.streak, History: e.history, CertExpiresAt: e.certExpiresAt,
		}
		if e.incident != nil {
			st.OpenIncidentID = e.incident.ID
		}
		snap.Targets = append(snap.Targets, st)
	}
	for _, inc := range s.incidents {
		snap.Incidents = append(snap.Incidents, *inc)
	}
	data, err := json.Marshal(snap)
	s.mu.Unlock()
	if err != nil {
		return err
	}

	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		return err
	}
	tmp, err := os.CreateTemp(filepath.Dir(path), ".monitor-*.json")
	if err != nil {
		return err
	}
	defer os.Remove(tmp.Name())
	if _, err := tmp.Write(data); err != nil {
		tmp.Close()
		return err
	}
	if err := tmp.Close(); err != nil {
		return err
	}
	return os.Rename(tmp.Name(), path)
}

// Load replaces the store's contents with what path holds. A missing file is
// not an error: it is a first start.
func (s *Store) Load(path string) error {
	data, err := os.ReadFile(path)
	if errors.Is(err, os.ErrNotExist) {
		return nil
	}
	if err != nil {
		return err
	}
	var snap snapshot
	if err := json.Unmarshal(data, &snap); err != nil {
		return fmt.Errorf("%s: %w", path, err)
	}
	if snap.Version != snapshotVersion {
		return fmt.Errorf("%s: snapshot version %d, want %d", path, snap.Version, snapshotVersion)
	}

	s.mu.Lock()
	defer s.mu.Unlock()
	s.resetLocked()
	s.nextTargetID, s.nextIncidentID = snap.NextTargetID, snap.NextIncidentID
	byID := map[int]*Incident{}
	for i := range snap.Incidents {
		inc := snap.Incidents[i]
		s.incidents = append(s.incidents, &inc)
		byID[inc.ID] = &inc
	}
	for _, st := range snap.Targets {
		s.targets[st.Target.ID] = &entry{
			target: st.Target, state: st.State, stateSince: st.StateSince,
			streak: st.Streak, history: st.History, certExpiresAt: st.CertExpiresAt,
			incident: byID[st.OpenIncidentID],
		}
		s.order = append(s.order, st.Target.ID)
	}
	return nil
}

// Persister saves the store to a file whenever it has changed, at most once
// per interval, and once more when it stops.
type Persister struct {
	Store    *Store
	Path     string
	Interval time.Duration
	Log      *slog.Logger

	mu    sync.Mutex
	saved uint64
}

// Run saves until ctx is cancelled, then saves a final time.
func (p *Persister) Run(ctx context.Context) {
	t := time.NewTicker(p.Interval)
	defer t.Stop()
	for {
		select {
		case <-ctx.Done():
			p.Flush()
			return
		case <-t.C:
			p.Flush()
		}
	}
}

// Flush saves now if anything changed since the last save.
func (p *Persister) Flush() {
	p.mu.Lock()
	defer p.mu.Unlock()
	n := p.Store.Changes()
	if n == p.saved {
		return
	}
	if err := p.Store.Save(p.Path); err != nil {
		p.Log.Error("saving state", "path", p.Path, "err", err)
		return
	}
	p.saved = n
}
