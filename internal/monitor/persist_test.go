package monitor

import (
	"path/filepath"
	"testing"
	"time"
)

// A restart with a data file keeps history, belief, the streak in progress
// and the open incident, so uptime is not reset and the outage is not
// forgotten or re-alerted.
func TestSaveAndLoadSurvivesRestart(t *testing.T) {
	s, c, tg := newTestStore(t)
	for i := 0; i < Threshold; i++ {
		record(t, s, c, tg.ID, Down)
	}
	record(t, s, c, tg.ID, Up) // one good check towards recovery
	path := filepath.Join(t.TempDir(), "state", "monitor.json")
	if err := s.Save(path); err != nil {
		t.Fatal(err)
	}

	restarted := NewStore(c.now)
	if err := restarted.Load(path); err != nil {
		t.Fatal(err)
	}
	st, err := restarted.Status(tg.ID, time.Hour)
	if err != nil {
		t.Fatal(err)
	}
	if st.State != Down || st.Disagreeing != 1 || st.Uptime.Checks != Threshold+1 {
		t.Fatalf("restored status = %+v", st)
	}

	// Recovery completes against the restored incident, not a new one.
	for i := 0; i < Threshold-1; i++ {
		record(t, restarted, c, tg.ID, Up)
	}
	incs := restarted.Incidents()
	if len(incs) != 1 || incs[0].Ongoing() {
		t.Fatalf("incidents after restart = %+v", incs)
	}
	next, _ := restarted.AddTarget(Target{Name: "b", Type: TypeTCP, Address: "b:1", IntervalSeconds: 1, TimeoutSeconds: 1})
	if next.ID != tg.ID+1 {
		t.Fatalf("next id = %d, want %d", next.ID, tg.ID+1)
	}
}

func TestLoadMissingFileIsFirstStart(t *testing.T) {
	s := NewStore(nil)
	if err := s.Load(filepath.Join(t.TempDir(), "none.json")); err != nil {
		t.Fatal(err)
	}
}
