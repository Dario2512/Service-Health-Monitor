// Package monitor holds what the service knows about its targets: what each
// check observed, what the service believes as a result, the incidents that
// belief produced, and the uptime arithmetic over a stated period.
package monitor

import "time"

// Threshold is N: the number of consecutive observations that must disagree
// with the current state before the state changes. One failed check is not an
// outage, and one good check is not a recovery.
const Threshold = 3

// DefaultCertWarnDays is how close to expiry a certificate may get before an
// HTTPS check reports the target as down.
const DefaultCertWarnDays = 14

// State is either what one check observed or what the service believes.
type State string

const (
	Up   State = "up"
	Down State = "down"
)

// Check types a target can use.
const (
	TypeHTTP = "http"
	TypeTCP  = "tcp"
)

// Target is a service being watched, as the client described it.
type Target struct {
	ID              int       `json:"id"`
	Name            string    `json:"name"`
	Type            string    `json:"type"`
	Address         string    `json:"address"`
	IntervalSeconds int       `json:"interval_seconds"`
	TimeoutSeconds  int       `json:"timeout_seconds"`
	CertWarnDays    int       `json:"cert_warn_days"`
	CreatedAt       time.Time `json:"created_at"`
}

// Interval is the time between scheduled checks.
func (t Target) Interval() time.Duration { return time.Duration(t.IntervalSeconds) * time.Second }

// Timeout bounds a single check.
func (t Target) Timeout() time.Duration { return time.Duration(t.TimeoutSeconds) * time.Second }

// Observation is what one check saw. It says nothing about what the service
// believes; that is the job of the policy in Store.Record.
type Observation struct {
	At            time.Time  `json:"at"`
	Observed      State      `json:"observed"`
	ResponseMS    int64      `json:"response_ms"`
	Detail        string     `json:"detail"`
	CertExpiresAt *time.Time `json:"cert_expires_at,omitempty"`
}

// Incident is one period during which the service believed a target was down.
//
// StartedAt is the first failed observation of the streak that flipped the
// state, so the incident covers the whole outage. DetectedAt is when the
// policy was satisfied and the alert went out; the gap between the two is the
// price paid for not alerting on a single blip.
type Incident struct {
	ID           int           `json:"id"`
	TargetID     int           `json:"target_id"`
	TargetName   string        `json:"target_name"`
	StartedAt    time.Time     `json:"started_at"`
	DetectedAt   time.Time     `json:"detected_at"`
	EndedAt      *time.Time    `json:"ended_at"`
	Summary      string        `json:"summary"`
	FailedChecks int           `json:"failed_checks"`
	Observations []Observation `json:"observations"`
}

// Ongoing reports whether the incident has not yet ended.
func (i Incident) Ongoing() bool { return i.EndedAt == nil }

// Transition is a change of belief. Alerts are sent for transitions, never
// for individual failed checks.
type Transition struct {
	Target   Target    `json:"target"`
	From     State     `json:"from"`
	To       State     `json:"to"`
	At       time.Time `json:"at"`
	Incident Incident  `json:"incident"`
}

// Period is the window an uptime figure is computed over. It is always
// reported next to the percentage, because a percentage of nothing in
// particular is not a measurement.
type Period struct {
	From time.Time `json:"from"`
	To   time.Time `json:"to"`
}

// Uptime is the share of checks in a period that observed the target up.
// UptimePercent is nil when there were no checks: no data is not 100%.
type Uptime struct {
	Period        Period   `json:"period"`
	Checks        int      `json:"checks"`
	Up            int      `json:"up"`
	UptimePercent *float64 `json:"uptime_percent"`
}

// TargetStatus is a target together with what the service currently believes
// about it.
type TargetStatus struct {
	Target
	State         State        `json:"state"`
	StateSince    time.Time    `json:"state_since"`
	Disagreeing   int          `json:"consecutive_disagreeing"`
	LastCheck     *Observation `json:"last_check"`
	LastResponse  *int64       `json:"last_response_ms"`
	CertExpiresAt *time.Time   `json:"cert_expires_at,omitempty"`
	Uptime        Uptime       `json:"uptime"`
}
