package alert

import (
	"bufio"
	"context"
	"encoding/json"
	"errors"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"atad-project/internal/monitor"
)

func transition(to monitor.State) monitor.Transition {
	start := time.Date(2026, 11, 1, 12, 0, 0, 0, time.UTC)
	inc := monitor.Incident{
		ID: 1, TargetID: 1, TargetName: "api", StartedAt: start, DetectedAt: start.Add(20 * time.Second),
		FailedChecks: 3, Observations: []monitor.Observation{{Observed: monitor.Down, Detail: "connection refused"}},
	}
	tr := monitor.Transition{
		Target: monitor.Target{ID: 1, Name: "api", Type: "tcp", Address: "127.0.0.1:9"},
		From:   monitor.Up, To: to, At: inc.DetectedAt, Incident: inc,
	}
	if to == monitor.Up {
		tr.From = monitor.Down
		end := start.Add(5 * time.Minute)
		tr.Incident.EndedAt = &end
		tr.At = end.Add(20 * time.Second)
	}
	return tr
}

func TestComposeDownAndRecovery(t *testing.T) {
	down := Compose(transition(monitor.Down))
	if down.Kind != "down" || !strings.Contains(down.Subject, "[DOWN] api") ||
		!strings.Contains(down.Text, "3 consecutive failed checks") || !strings.Contains(down.Text, "connection refused") {
		t.Errorf("down message = %+v", down)
	}
	up := Compose(transition(monitor.Up))
	if up.Kind != "recovered" || !strings.Contains(up.Subject, "[RECOVERED]") || !strings.Contains(up.Text, "lasted 5m0s") {
		t.Errorf("recovery message = %+v", up)
	}
}

type failing struct{}

func (failing) Name() string                        { return "broken" }
func (failing) Send(context.Context, Message) error { return errors.New("unreachable") }

func TestDispatcherSendsToEveryChannel(t *testing.T) {
	var (
		mu     sync.Mutex
		hooked []Message
	)
	hook := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		var body struct {
			Text string `json:"text"`
			Message
		}
		json.NewDecoder(r.Body).Decode(&body)
		mu.Lock()
		hooked = append(hooked, body.Message)
		mu.Unlock()
		if body.Text == "" {
			w.WriteHeader(http.StatusBadRequest)
		}
	}))
	defer hook.Close()
	path := filepath.Join(t.TempDir(), "alerts.jsonl")

	d := NewDispatcher(
		LogChannel{Log: slog.New(slog.NewTextHandler(io.Discard, nil))},
		&FileChannel{Path: path},
		&WebhookChannel{URL: hook.URL},
		failing{},
	)
	d.Notify(transition(monitor.Down))
	d.Notify(transition(monitor.Up))
	d.Wait()

	dels := d.Deliveries()
	if len(dels) != 8 {
		t.Fatalf("deliveries = %d, want 8", len(dels))
	}
	for _, del := range dels {
		if del.Delivered != (del.Channel != "broken") {
			t.Errorf("delivery %+v", del)
		}
	}
	if len(hooked) != 2 {
		t.Errorf("webhook received %d messages, want 2", len(hooked))
	}

	f, err := os.Open(path)
	if err != nil {
		t.Fatal(err)
	}
	defer f.Close()
	lines := 0
	for sc := bufio.NewScanner(f); sc.Scan(); lines++ {
	}
	if lines != 2 {
		t.Errorf("file has %d lines, want 2", lines)
	}

	d.Reset()
	if len(d.Deliveries()) != 0 {
		t.Error("reset kept deliveries")
	}
}
