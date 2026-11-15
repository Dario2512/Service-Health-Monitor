package monitor

import (
	"context"
	"errors"
	"sync"
	"testing"
	"time"
)

type fakeChecker struct {
	mu       sync.Mutex
	observed State
	calls    int
}

func (f *fakeChecker) Check(context.Context, Target) Observation {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.calls++
	return Observation{At: time.Now(), Observed: f.observed, Detail: "fake"}
}

func (f *fakeChecker) count() int {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.calls
}

type recorder struct {
	mu  sync.Mutex
	got []Transition
}

func (r *recorder) Notify(t Transition) {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.got = append(r.got, t)
}

func (r *recorder) transitions() []Transition {
	r.mu.Lock()
	defer r.mu.Unlock()
	return append([]Transition(nil), r.got...)
}

func TestServiceAlertsOnStateChangeNotOnFailure(t *testing.T) {
	fc := &fakeChecker{observed: Down}
	rec := &recorder{}
	svc := NewService(NewStore(nil), fc, rec)
	defer svc.Stop()
	tg, _ := svc.AddTarget(Target{Name: "x", Type: TypeTCP, Address: "a:1", IntervalSeconds: 3600, TimeoutSeconds: 1})

	for i := 0; i < 5; i++ {
		obs, _, err := svc.CheckNow(context.Background(), tg.ID)
		if err != nil || obs.Observed != Down {
			t.Fatalf("check %d: %+v %v", i, obs, err)
		}
	}
	fc.observed = Up
	for i := 0; i < Threshold; i++ {
		svc.CheckNow(context.Background(), tg.ID)
	}
	got := rec.transitions()
	if len(got) != 2 || got[0].To != Down || got[1].To != Up {
		t.Fatalf("transitions = %+v, want down then up", got)
	}
}

func TestServiceSchedulesChecksAfterInterval(t *testing.T) {
	fc := &fakeChecker{observed: Up}
	svc := NewService(NewStore(nil), fc, nil)
	defer svc.Stop()
	svc.AddTarget(Target{Name: "x", Type: TypeTCP, Address: "a:1", IntervalSeconds: 1, TimeoutSeconds: 1})

	if fc.count() != 0 {
		t.Fatal("checked immediately on creation")
	}
	deadline := time.Now().Add(3 * time.Second)
	for fc.count() == 0 && time.Now().Before(deadline) {
		time.Sleep(20 * time.Millisecond)
	}
	if fc.count() == 0 {
		t.Fatal("no scheduled check within 3s of a 1s interval")
	}

	svc.Reset()
	n := fc.count()
	time.Sleep(1200 * time.Millisecond)
	if fc.count() != n {
		t.Fatal("checks continued after reset")
	}
}

func TestCheckFromBeforeResetIsDiscarded(t *testing.T) {
	s := NewStore(nil)
	s.AddTarget(Target{Name: "old", Type: TypeTCP, Address: "a:1", IntervalSeconds: 1, TimeoutSeconds: 1})
	epoch := s.Epoch()
	s.Reset()
	s.AddTarget(Target{Name: "new", Type: TypeTCP, Address: "b:1", IntervalSeconds: 1, TimeoutSeconds: 1})
	if _, _, err := s.RecordInEpoch(epoch, 1, Observation{Observed: Down}); !errors.Is(err, ErrNotFound) {
		t.Fatalf("stale record err = %v", err)
	}
	if st, _ := s.Status(1, time.Hour); st.LastCheck != nil {
		t.Fatal("stale observation reached the new target")
	}
}
