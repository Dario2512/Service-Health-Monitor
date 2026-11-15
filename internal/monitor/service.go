package monitor

import (
	"context"
	"sync"
	"time"
)

// Notifier is told about every change of belief. It must not block: it is
// called on the checking goroutine.
type Notifier interface {
	Notify(Transition)
}

// Service runs checks, on a schedule and on demand, feeds what they observe
// through the policy in Store, and hands any resulting transition to the
// notifier.
type Service struct {
	store    *Store
	checker  Checker
	notifier Notifier

	mu      sync.Mutex
	ctx     context.Context
	cancel  context.CancelFunc
	loops   sync.WaitGroup
	running map[int]context.CancelFunc
	locks   map[int]*sync.Mutex // one check at a time per target, so history stays in order
}

// NewService wires a store, a checker and a notifier together. notifier may
// be nil.
func NewService(store *Store, checker Checker, notifier Notifier) *Service {
	ctx, cancel := context.WithCancel(context.Background())
	return &Service{
		store: store, checker: checker, notifier: notifier,
		ctx: ctx, cancel: cancel,
		running: map[int]context.CancelFunc{},
		locks:   map[int]*sync.Mutex{},
	}
}

// Store gives read access to what the service knows.
func (s *Service) Store() *Store { return s.store }

// Start schedules every target already in the store, such as those restored
// from disk.
func (s *Service) Start() {
	for _, t := range s.store.Targets() {
		s.schedule(t)
	}
}

// Stop ends every scheduled loop and waits for them to finish.
func (s *Service) Stop() {
	s.cancel()
	s.loops.Wait()
}

// AddTarget validates nothing; the caller has. It stores the target and starts
// checking it every interval. The first scheduled check comes one interval
// after creation, not immediately.
func (s *Service) AddTarget(t Target) (Target, error) {
	t, err := s.store.AddTarget(t)
	if err != nil {
		return Target{}, err
	}
	s.schedule(t)
	return t, nil
}

// Reset stops all checking and discards everything.
func (s *Service) Reset() {
	s.mu.Lock()
	for _, stop := range s.running {
		stop()
	}
	s.running = map[int]context.CancelFunc{}
	s.locks = map[int]*sync.Mutex{}
	s.mu.Unlock()
	s.store.Reset()
}

// CheckNow runs one check of a target immediately. It counts toward the
// policy exactly like a scheduled check.
func (s *Service) CheckNow(ctx context.Context, id int) (Observation, State, error) {
	epoch := s.store.Epoch()
	t, err := s.store.Target(id)
	if err != nil {
		return Observation{}, "", err
	}
	lock := s.lockFor(id)
	lock.Lock()
	defer lock.Unlock()

	obs := s.checker.Check(ctx, t)
	state, tr, err := s.store.RecordInEpoch(epoch, id, obs)
	if err != nil {
		return Observation{}, "", err
	}
	if tr != nil && s.notifier != nil {
		s.notifier.Notify(*tr)
	}
	return obs, state, nil
}

func (s *Service) lockFor(id int) *sync.Mutex {
	s.mu.Lock()
	defer s.mu.Unlock()
	l, ok := s.locks[id]
	if !ok {
		l = &sync.Mutex{}
		s.locks[id] = l
	}
	return l
}

func (s *Service) schedule(t Target) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.ctx.Err() != nil {
		return
	}
	ctx, stop := context.WithCancel(s.ctx)
	s.running[t.ID] = stop
	s.loops.Add(1)
	go func() {
		defer s.loops.Done()
		ticker := time.NewTicker(t.Interval())
		defer ticker.Stop()
		for {
			select {
			case <-ctx.Done():
				return
			case <-ticker.C:
				if _, _, err := s.CheckNow(ctx, t.ID); err != nil {
					return // the target is gone
				}
			}
		}
	}()
}
