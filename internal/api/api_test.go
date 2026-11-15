package api

import (
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"

	"atad-project/internal/alert"
	"atad-project/internal/monitor"
)

type scripted struct {
	mu       sync.Mutex
	observed monitor.State
}

func (s *scripted) set(st monitor.State) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.observed = st
}

func (s *scripted) Check(context.Context, monitor.Target) monitor.Observation {
	s.mu.Lock()
	defer s.mu.Unlock()
	return monitor.Observation{At: time.Now(), Observed: s.observed, ResponseMS: 5, Detail: "scripted"}
}

func newServer(t *testing.T) (*httptest.Server, *scripted, *alert.Dispatcher) {
	t.Helper()
	chk := &scripted{observed: monitor.Down}
	alerts := alert.NewDispatcher()
	svc := monitor.NewService(monitor.NewStore(nil), chk, alerts)
	t.Cleanup(svc.Stop)
	mux := http.NewServeMux()
	(&API{Service: svc, Alerts: alerts}).Register(mux)
	srv := httptest.NewServer(mux)
	t.Cleanup(srv.Close)
	return srv, chk, alerts
}

func do(t *testing.T, method, url, body string, wantStatus int, out any) {
	t.Helper()
	req, _ := http.NewRequest(method, url, strings.NewReader(body))
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != wantStatus {
		t.Fatalf("%s %s: status %d, want %d", method, url, resp.StatusCode, wantStatus)
	}
	if ct := resp.Header.Get("Content-Type"); !strings.HasPrefix(ct, "application/json") && !strings.HasPrefix(ct, "text/html") {
		t.Fatalf("%s %s: content type %q", method, url, ct)
	}
	if out != nil {
		if err := json.NewDecoder(resp.Body).Decode(out); err != nil {
			t.Fatalf("%s %s: decode: %v", method, url, err)
		}
	}
}

const ghost = `{"name":"ghost","type":"tcp","address":"127.0.0.1:9","interval_seconds":60,"timeout_seconds":2}`

// The manual test from the project document, over HTTP.
func TestTwoFailedChecksRaiseNothing(t *testing.T) {
	srv, _, _ := newServer(t)
	var created map[string]any
	do(t, "POST", srv.URL+"/targets", ghost, 201, &created)
	if created["id"] != float64(1) {
		t.Fatalf("first id = %v, want 1", created["id"])
	}

	for i := 0; i < 2; i++ {
		var res map[string]any
		do(t, "POST", srv.URL+"/targets/1/check", "", 200, &res)
		if res["observed"] != "down" || res["state"] != "up" {
			t.Fatalf("check %d = %v", i+1, res)
		}
	}
	var targets []map[string]any
	do(t, "GET", srv.URL+"/targets", "", 200, &targets)
	if len(targets) != 1 || targets[0]["state"] != "up" || targets[0]["last_response_ms"] == nil || targets[0]["uptime"] == nil {
		t.Fatalf("targets = %v", targets)
	}
	var incidents []any
	do(t, "GET", srv.URL+"/incidents", "", 200, &incidents)
	if len(incidents) != 0 {
		t.Fatalf("incidents after two failures = %v", incidents)
	}

	var res map[string]any
	do(t, "POST", srv.URL+"/targets/1/check", "", 200, &res)
	if res["state"] != "down" {
		t.Fatalf("third failure: %v", res)
	}
	do(t, "GET", srv.URL+"/incidents", "", 200, &incidents)
	if len(incidents) != 1 {
		t.Fatalf("incidents after third failure = %v", incidents)
	}
}

func TestUptimeRoute(t *testing.T) {
	srv, chk, _ := newServer(t)
	do(t, "POST", srv.URL+"/targets", ghost, 201, nil)
	chk.set(monitor.Up)
	do(t, "POST", srv.URL+"/targets/1/check", "", 200, nil)
	chk.set(monitor.Down)
	do(t, "POST", srv.URL+"/targets/1/check", "", 200, nil)

	var u struct {
		Period        map[string]string `json:"period"`
		Checks, Up    int
		UptimePercent *float64 `json:"uptime_percent"`
	}
	from := time.Now().Add(-time.Hour).UTC().Format(time.RFC3339)
	to := time.Now().Add(time.Hour).UTC().Format(time.RFC3339)
	do(t, "GET", srv.URL+"/targets/1/uptime?from="+from+"&to="+to, "", 200, &u)
	if u.Checks != 2 || u.Up != 1 || u.UptimePercent == nil || *u.UptimePercent != 50 || u.Period["from"] == "" {
		t.Fatalf("uptime = %+v", u)
	}

	old := time.Now().Add(-48 * time.Hour).UTC().Format(time.RFC3339)
	do(t, "GET", srv.URL+"/targets/1/uptime?from="+old+"&to="+from, "", 200, &u)
	if u.Checks != 0 || u.UptimePercent != nil {
		t.Fatalf("empty period uptime = %+v", u)
	}
	do(t, "GET", srv.URL+"/targets/1/uptime?from="+to+"&to="+from, "", 422, nil)
	do(t, "GET", srv.URL+"/targets/1/uptime?from=yesterday", "", 400, nil)
	do(t, "GET", srv.URL+"/targets/7/uptime", "", 404, nil)
}

func TestErrors(t *testing.T) {
	srv, _, _ := newServer(t)
	do(t, "POST", srv.URL+"/targets", ghost, 201, nil)
	cases := []struct {
		method, path, body string
		status             int
		code               string
	}{
		{"POST", "/targets", ghost, 409, "conflict"},
		{"POST", "/targets", `{"name":"x","type":"tcp","address":"a:1","interval_seconds":5,"timeout_seconds":10}`, 422, "unprocessable"},
		{"POST", "/targets", `{"name":"x","type":"ping","address":"a:1","interval_seconds":5,"timeout_seconds":1}`, 400, "validation"},
		{"POST", "/targets", `{"name":"x","interval_seconds":"5"}`, 400, "validation"},
		{"POST", "/targets", `{"name":"x","colour":"red"}`, 400, "validation"},
		{"POST", "/targets", `not json`, 400, "validation"},
		{"POST", "/targets", ``, 400, "validation"},
		{"POST", "/targets/99/check", ``, 404, "not_found"},
		{"POST", "/targets/zero/check", ``, 400, "validation"},
		{"GET", "/targets/99", ``, 404, "not_found"},
	}
	for _, tc := range cases {
		var e map[string]string
		do(t, tc.method, srv.URL+tc.path, tc.body, tc.status, &e)
		if e["error"] != tc.code || e["message"] == "" {
			t.Errorf("%s %s %s: body %v, want error %q", tc.method, tc.path, tc.body, e, tc.code)
		}
	}
}

func TestStatusPageIsPlainLanguage(t *testing.T) {
	srv, _, _ := newServer(t)
	do(t, "POST", srv.URL+"/targets", ghost, 201, nil)
	for i := 0; i < monitor.Threshold; i++ {
		do(t, "POST", srv.URL+"/targets/1/check", "", 200, nil)
	}
	resp, err := http.Get(srv.URL + "/status")
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	body, _ := io.ReadAll(resp.Body)
	page := string(body)
	for _, want := range []string{"One service is having problems", "ghost", "Not working", "last 24 hours", "is unavailable"} {
		if !strings.Contains(page, want) {
			t.Errorf("status page missing %q", want)
		}
	}
	if strings.Contains(page, "127.0.0.1") {
		t.Error("status page exposes the target address")
	}
}
