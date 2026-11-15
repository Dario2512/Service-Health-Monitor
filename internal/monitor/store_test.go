package monitor

import (
	"errors"
	"testing"
	"time"
)

type clock struct{ t time.Time }

func (c *clock) now() time.Time { return c.t }

func (c *clock) tick(d time.Duration) time.Time {
	c.t = c.t.Add(d)
	return c.t
}

func newTestStore(t *testing.T) (*Store, *clock, Target) {
	t.Helper()
	c := &clock{t: time.Date(2026, 11, 1, 12, 0, 0, 0, time.UTC)}
	s := NewStore(c.now)
	tg, err := s.AddTarget(Target{Name: "api", Type: TypeTCP, Address: "127.0.0.1:9", IntervalSeconds: 10, TimeoutSeconds: 2})
	if err != nil {
		t.Fatal(err)
	}
	return s, c, tg
}

func record(t *testing.T, s *Store, c *clock, id int, observed State) (State, *Transition) {
	t.Helper()
	st, tr, err := s.Record(id, Observation{At: c.tick(10 * time.Second), Observed: observed, Detail: string(observed)})
	if err != nil {
		t.Fatal(err)
	}
	return st, tr
}

// The test the project document names: two failures out of three needed must
// leave the state up and raise nothing.
func TestTwoFailuresAreNotAnOutage(t *testing.T) {
	s, c, tg := newTestStore(t)
	for i := 0; i < Threshold-1; i++ {
		st, tr := record(t, s, c, tg.ID, Down)
		if st != Up || tr != nil {
			t.Fatalf("after %d failures: state %s, transition %v; want up and none", i+1, st, tr)
		}
	}
	if n := len(s.Incidents()); n != 0 {
		t.Fatalf("incidents = %d, want 0", n)
	}
	st, _ := s.Status(tg.ID, time.Hour)
	if st.Disagreeing != Threshold-1 || st.LastCheck.Observed != Down {
		t.Fatalf("status = %+v", st)
	}
}

func TestThirdFailureOpensIncident(t *testing.T) {
	s, c, tg := newTestStore(t)
	first := c.t.Add(10 * time.Second)
	var tr *Transition
	for i := 0; i < Threshold; i++ {
		_, tr = record(t, s, c, tg.ID, Down)
	}
	if tr == nil || tr.From != Up || tr.To != Down {
		t.Fatalf("transition = %+v, want up->down", tr)
	}
	incs := s.Incidents()
	if len(incs) != 1 {
		t.Fatalf("incidents = %d, want 1", len(incs))
	}
	inc := incs[0]
	if !inc.StartedAt.Equal(first) || !inc.DetectedAt.Equal(c.t) || inc.EndedAt != nil {
		t.Fatalf("incident times = %+v", inc)
	}
	if inc.FailedChecks != Threshold || len(inc.Observations) != Threshold {
		t.Fatalf("incident observations = %d/%d", inc.FailedChecks, len(inc.Observations))
	}

	// Further failures neither alert again nor open another incident.
	if _, tr := record(t, s, c, tg.ID, Down); tr != nil {
		t.Fatalf("fourth failure produced a transition")
	}
	if n := len(s.Incidents()); n != 1 || s.Incidents()[0].FailedChecks != Threshold+1 {
		t.Fatalf("incidents after fourth failure = %+v", s.Incidents())
	}
}

func TestBlipResetsStreak(t *testing.T) {
	s, c, tg := newTestStore(t)
	for _, o := range []State{Down, Down, Up, Down, Down} {
		if _, tr := record(t, s, c, tg.ID, o); tr != nil {
			t.Fatalf("observation %s produced a transition", o)
		}
	}
	if st, _ := s.Status(tg.ID, time.Hour); st.State != Up {
		t.Fatalf("state = %s, want up", st.State)
	}
}

func TestRecoveryNeedsThresholdAndClosesIncident(t *testing.T) {
	s, c, tg := newTestStore(t)
	for i := 0; i < Threshold; i++ {
		record(t, s, c, tg.ID, Down)
	}
	firstGood := c.t.Add(10 * time.Second)
	for i := 0; i < Threshold-1; i++ {
		if st, tr := record(t, s, c, tg.ID, Up); st != Down || tr != nil {
			t.Fatalf("recovered after %d good checks", i+1)
		}
	}
	_, tr := record(t, s, c, tg.ID, Up)
	if tr == nil || tr.To != Up || tr.Incident.EndedAt == nil || !tr.Incident.EndedAt.Equal(firstGood) {
		t.Fatalf("recovery transition = %+v", tr)
	}
	inc := s.Incidents()[0]
	if inc.Ongoing() || len(inc.Observations) != 2*Threshold {
		t.Fatalf("incident after recovery = %+v", inc)
	}
}

func TestUptimeIsOverRequestedPeriod(t *testing.T) {
	s, c, tg := newTestStore(t)
	start := c.t
	for _, o := range []State{Up, Up, Down, Up} { // at +10s .. +40s
		record(t, s, c, tg.ID, o)
	}
	cases := []struct {
		from, to  time.Duration
		checks    int
		up        int
		percent   float64
		noPercent bool
	}{
		{0, time.Minute, 4, 3, 75, false},
		{25 * time.Second, time.Minute, 2, 1, 50, false},
		{10 * time.Second, 20 * time.Second, 2, 2, 100, false},
		{time.Hour, 2 * time.Hour, 0, 0, 0, true},
	}
	for _, tc := range cases {
		u, err := s.Uptime(tg.ID, Period{From: start.Add(tc.from), To: start.Add(tc.to)})
		if err != nil {
			t.Fatal(err)
		}
		if u.Checks != tc.checks || u.Up != tc.up {
			t.Errorf("%v..%v: checks %d up %d, want %d %d", tc.from, tc.to, u.Checks, u.Up, tc.checks, tc.up)
		}
		if tc.noPercent != (u.UptimePercent == nil) || (!tc.noPercent && *u.UptimePercent != tc.percent) {
			t.Errorf("%v..%v: percent %v, want %v", tc.from, tc.to, u.UptimePercent, tc.percent)
		}
	}
}

func TestIncidentsNewestFirst(t *testing.T) {
	s, c, a := newTestStore(t)
	b, _ := s.AddTarget(Target{Name: "db", Type: TypeTCP, Address: "127.0.0.1:5432", IntervalSeconds: 10, TimeoutSeconds: 2})
	for i := 0; i < Threshold; i++ {
		record(t, s, c, a.ID, Down)
	}
	for i := 0; i < Threshold; i++ {
		record(t, s, c, b.ID, Down)
	}
	incs := s.Incidents()
	if len(incs) != 2 || incs[0].TargetID != b.ID || incs[1].TargetID != a.ID {
		t.Fatalf("incidents order = %+v", incs)
	}
}

func TestDuplicateNameConflictsAndResetRestartsIDs(t *testing.T) {
	s, _, _ := newTestStore(t)
	_, err := s.AddTarget(Target{Name: "API", Type: TypeTCP, Address: "x:1", IntervalSeconds: 1, TimeoutSeconds: 1})
	if !errors.Is(err, ErrConflict) {
		t.Fatalf("err = %v, want conflict", err)
	}
	s.Reset()
	tg, err := s.AddTarget(Target{Name: "API", Type: TypeTCP, Address: "x:1", IntervalSeconds: 1, TimeoutSeconds: 1})
	if err != nil || tg.ID != 1 {
		t.Fatalf("after reset: %+v, %v", tg, err)
	}
	if _, _, err := s.Record(99, Observation{}); !errors.Is(err, ErrNotFound) {
		t.Fatalf("record unknown = %v", err)
	}
}

func TestValidate(t *testing.T) {
	ok := Target{Name: "x", Type: TypeHTTP, Address: "https://example.com/health", IntervalSeconds: 30, TimeoutSeconds: 5}
	if err := Validate(&ok); err != nil {
		t.Fatalf("valid target refused: %v", err)
	}
	bad := map[string]Target{
		"no name":      {Type: TypeTCP, Address: "a:1", IntervalSeconds: 1, TimeoutSeconds: 1},
		"bad type":     {Name: "x", Type: "icmp", Address: "a:1", IntervalSeconds: 1, TimeoutSeconds: 1},
		"http no url":  {Name: "x", Type: TypeHTTP, Address: "example.com", IntervalSeconds: 1, TimeoutSeconds: 1},
		"tcp no port":  {Name: "x", Type: TypeTCP, Address: "example.com", IntervalSeconds: 1, TimeoutSeconds: 1},
		"tcp bad port": {Name: "x", Type: TypeTCP, Address: "a:70000", IntervalSeconds: 1, TimeoutSeconds: 1},
		"zero timeout": {Name: "x", Type: TypeTCP, Address: "a:1", IntervalSeconds: 1, TimeoutSeconds: 0},
	}
	for name, tg := range bad {
		var ve *ValidationError
		if err := Validate(&tg); !errors.As(err, &ve) || ve.Unprocessable {
			t.Errorf("%s: err = %v, want 400 validation", name, err)
		}
	}
	slow := Target{Name: "x", Type: TypeTCP, Address: "a:1", IntervalSeconds: 5, TimeoutSeconds: 10}
	var ve *ValidationError
	if err := Validate(&slow); !errors.As(err, &ve) || !ve.Unprocessable {
		t.Errorf("timeout > interval: err = %v, want 422", err)
	}
}
