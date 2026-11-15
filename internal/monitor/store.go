package monitor

import (
	"errors"
	"fmt"
	"math"
	"sort"
	"strings"
	"sync"
	"time"
)

var (
	ErrNotFound = errors.New("not found")
	ErrConflict = errors.New("conflict")
)

// maxHistory bounds the observations kept per target. At one check every ten
// seconds this is well over a month, which covers any period worth asking for.
const maxHistory = 300_000

// maxIncidentObservations bounds what an incident keeps of what was observed:
// the first half of the slots hold how it began, the rest roll with the latest
// checks so the recovery is always there. FailedChecks keeps the true count.
const maxIncidentObservations = 20

type entry struct {
	target        Target
	state         State
	stateSince    time.Time
	streak        []Observation // consecutive observations disagreeing with state
	history       []Observation
	certExpiresAt *time.Time
	incident      *Incident // the open incident, if any
}

// Store is the service's memory. It is safe for concurrent use; the scheduler
// and the HTTP handlers both write to it.
type Store struct {
	mu             sync.Mutex
	now            func() time.Time
	targets        map[int]*entry
	order          []int
	incidents      []*Incident
	nextTargetID   int
	nextIncidentID int
	epoch          int // bumped by Reset, so a check started before it cannot land after it
}

// NewStore returns an empty store. now is the clock; pass nil for time.Now.
func NewStore(now func() time.Time) *Store {
	if now == nil {
		now = time.Now
	}
	s := &Store{now: now}
	s.resetLocked()
	return s
}

// Reset discards every target, observation and incident, and restarts the
// identifiers at 1.
func (s *Store) Reset() {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.resetLocked()
}

func (s *Store) resetLocked() {
	s.epoch++
	s.targets = map[int]*entry{}
	s.order = nil
	s.incidents = nil
	s.nextTargetID = 1
	s.nextIncidentID = 1
}

// AddTarget stores a validated target and assigns its identifier. A new
// target is believed up until Threshold checks say otherwise.
func (s *Store) AddTarget(t Target) (Target, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	for _, e := range s.targets {
		if strings.EqualFold(e.target.Name, t.Name) {
			return Target{}, fmt.Errorf("%w: a target named %q already exists", ErrConflict, e.target.Name)
		}
	}
	t.ID = s.nextTargetID
	s.nextTargetID++
	t.CreatedAt = s.now().UTC()
	if t.CertWarnDays == 0 {
		t.CertWarnDays = DefaultCertWarnDays
	}
	s.targets[t.ID] = &entry{target: t, state: Up, stateSince: t.CreatedAt}
	s.order = append(s.order, t.ID)
	return t, nil
}

// Target returns one target's definition.
func (s *Store) Target(id int) (Target, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	e, ok := s.targets[id]
	if !ok {
		return Target{}, notFound(id)
	}
	return e.target, nil
}

// Targets returns every target's definition in creation order.
func (s *Store) Targets() []Target {
	s.mu.Lock()
	defer s.mu.Unlock()
	out := make([]Target, 0, len(s.order))
	for _, id := range s.order {
		out = append(out, s.targets[id].target)
	}
	return out
}

// Statuses returns every target with its current belief, and uptime over the
// window ending now.
func (s *Store) Statuses(window time.Duration) []TargetStatus {
	s.mu.Lock()
	defer s.mu.Unlock()
	now := s.now().UTC()
	p := Period{From: now.Add(-window), To: now}
	out := make([]TargetStatus, 0, len(s.order))
	for _, id := range s.order {
		out = append(out, s.statusLocked(s.targets[id], p))
	}
	return out
}

// Status returns one target with its current belief.
func (s *Store) Status(id int, window time.Duration) (TargetStatus, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	e, ok := s.targets[id]
	if !ok {
		return TargetStatus{}, notFound(id)
	}
	now := s.now().UTC()
	return s.statusLocked(e, Period{From: now.Add(-window), To: now}), nil
}

func (s *Store) statusLocked(e *entry, p Period) TargetStatus {
	st := TargetStatus{
		Target:        e.target,
		State:         e.state,
		StateSince:    e.stateSince,
		Disagreeing:   len(e.streak),
		CertExpiresAt: e.certExpiresAt,
		Uptime:        uptime(e.history, p),
	}
	if n := len(e.history); n > 0 {
		last := e.history[n-1]
		st.LastCheck = &last
		st.LastResponse = &last.ResponseMS
	}
	return st
}

// Epoch identifies the store's contents between resets.
func (s *Store) Epoch() int {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.epoch
}

// RecordInEpoch is Record, refused if the store has been reset since epoch
// was read. Target identifiers restart at 1 after a reset, so without this a
// slow check of the old target 1 could be recorded against the new one.
func (s *Store) RecordInEpoch(epoch, id int, obs Observation) (State, *Transition, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if epoch != s.epoch {
		return "", nil, notFound(id)
	}
	return s.recordLocked(id, obs)
}

// Record applies one observation to a target and returns what the service
// now believes. The policy is the whole of this function: the state changes
// only when Threshold consecutive observations disagree with it, and a
// transition is returned only when it does.
func (s *Store) Record(id int, obs Observation) (State, *Transition, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.recordLocked(id, obs)
}

func (s *Store) recordLocked(id int, obs Observation) (State, *Transition, error) {
	e, ok := s.targets[id]
	if !ok {
		return "", nil, notFound(id)
	}
	obs.At = obs.At.UTC()

	e.history = append(e.history, obs)
	if len(e.history) > maxHistory {
		e.history = append(e.history[:0], e.history[len(e.history)-maxHistory:]...)
	}
	if obs.CertExpiresAt != nil {
		e.certExpiresAt = obs.CertExpiresAt
	}
	if e.incident != nil {
		e.incident.addObservation(obs)
	}

	if obs.Observed == e.state {
		e.streak = e.streak[:0]
		return e.state, nil, nil
	}
	e.streak = append(e.streak, obs)
	if len(e.streak) < Threshold {
		return e.state, nil, nil
	}

	tr := &Transition{Target: e.target, From: e.state, To: obs.Observed, At: obs.At}
	first := e.streak[0]
	if obs.Observed == Down {
		inc := &Incident{
			ID:           s.nextIncidentID,
			TargetID:     e.target.ID,
			TargetName:   e.target.Name,
			StartedAt:    first.At,
			DetectedAt:   obs.At,
			Summary:      fmt.Sprintf("%d consecutive failed checks; last: %s", len(e.streak), obs.Detail),
			FailedChecks: len(e.streak),
			Observations: append([]Observation(nil), e.streak...),
		}
		s.nextIncidentID++
		s.incidents = append(s.incidents, inc)
		e.incident = inc
		tr.Incident = inc.clone()
	} else if e.incident != nil {
		// The outage ended with the first good check of the streak that
		// proved it; confirmation came Threshold-1 checks later.
		ended := first.At
		e.incident.EndedAt = &ended
		tr.Incident = e.incident.clone()
		e.incident = nil
	}
	e.state = obs.Observed
	e.stateSince = obs.At
	e.streak = e.streak[:0]
	return e.state, tr, nil
}

// Uptime computes the share of checks that observed the target up in p.
func (s *Store) Uptime(id int, p Period) (Uptime, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	e, ok := s.targets[id]
	if !ok {
		return Uptime{}, notFound(id)
	}
	return uptime(e.history, p), nil
}

// Incidents returns every incident, newest first.
func (s *Store) Incidents() []Incident {
	s.mu.Lock()
	defer s.mu.Unlock()
	out := make([]Incident, 0, len(s.incidents))
	for _, inc := range s.incidents {
		out = append(out, inc.clone())
	}
	sort.SliceStable(out, func(i, j int) bool {
		if !out[i].StartedAt.Equal(out[j].StartedAt) {
			return out[i].StartedAt.After(out[j].StartedAt)
		}
		return out[i].ID > out[j].ID
	})
	return out
}

// uptime counts observations with From <= At <= To.
func uptime(history []Observation, p Period) Uptime {
	u := Uptime{Period: Period{From: p.From.UTC(), To: p.To.UTC()}}
	// history is in time order, so binary search for the start.
	i := sort.Search(len(history), func(i int) bool { return !history[i].At.Before(p.From) })
	for ; i < len(history) && !history[i].At.After(p.To); i++ {
		u.Checks++
		if history[i].Observed == Up {
			u.Up++
		}
	}
	if u.Checks > 0 {
		pct := math.Round(float64(u.Up)/float64(u.Checks)*10000) / 100
		u.UptimePercent = &pct
	}
	return u
}

func (inc *Incident) addObservation(obs Observation) {
	if obs.Observed == Down {
		inc.FailedChecks++
	}
	if len(inc.Observations) >= maxIncidentObservations {
		head := maxIncidentObservations / 2
		inc.Observations = append(inc.Observations[:head], inc.Observations[head+1:]...)
	}
	inc.Observations = append(inc.Observations, obs)
}

func (inc *Incident) clone() Incident {
	c := *inc
	c.Observations = append([]Observation(nil), inc.Observations...)
	if inc.EndedAt != nil {
		t := *inc.EndedAt
		c.EndedAt = &t
	}
	return c
}

func notFound(id int) error {
	return fmt.Errorf("%w: no target with id %d", ErrNotFound, id)
}
