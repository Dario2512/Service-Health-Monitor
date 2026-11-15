// Package api exposes the monitor over HTTP: the project's own routes, and
// the JSON helpers every route in the service uses.
package api

import (
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"strconv"
	"strings"
	"time"

	"atad-project/internal/alert"
	"atad-project/internal/monitor"
)

// UptimeWindow is the period the target list reports uptime over. It is
// stated in every response next to the figure.
const UptimeWindow = 24 * time.Hour

const maxBody = 1 << 20

// API serves the project routes.
type API struct {
	Service *monitor.Service
	Alerts  *alert.Dispatcher
	Now     func() time.Time
}

// Register adds the project routes to mux.
func (a *API) Register(mux *http.ServeMux) {
	mux.HandleFunc("POST /targets", a.createTarget)
	mux.HandleFunc("GET /targets", a.listTargets)
	mux.HandleFunc("GET /targets/{id}", a.getTarget)
	mux.HandleFunc("POST /targets/{id}/check", a.checkTarget)
	mux.HandleFunc("GET /targets/{id}/uptime", a.uptime)
	mux.HandleFunc("GET /incidents", a.incidents)
	mux.HandleFunc("GET /alerts", a.alerts)
	mux.HandleFunc("GET /status", a.statusPage)
}

func (a *API) now() time.Time {
	if a.Now != nil {
		return a.Now()
	}
	return time.Now()
}

type createRequest struct {
	Name            string `json:"name"`
	Type            string `json:"type"`
	Address         string `json:"address"`
	IntervalSeconds int    `json:"interval_seconds"`
	TimeoutSeconds  int    `json:"timeout_seconds"`
	CertWarnDays    int    `json:"cert_warn_days"`
}

func (a *API) createTarget(w http.ResponseWriter, r *http.Request) {
	var req createRequest
	if !decode(w, r, &req) {
		return
	}
	t := monitor.Target{
		Name: req.Name, Type: req.Type, Address: req.Address,
		IntervalSeconds: req.IntervalSeconds, TimeoutSeconds: req.TimeoutSeconds, CertWarnDays: req.CertWarnDays,
	}
	if err := monitor.Validate(&t); err != nil {
		writeDomainError(w, err)
		return
	}
	t, err := a.Service.AddTarget(t)
	if err != nil {
		writeDomainError(w, err)
		return
	}
	st, err := a.Service.Store().Status(t.ID, UptimeWindow)
	if err != nil {
		writeDomainError(w, err)
		return
	}
	w.Header().Set("Location", fmt.Sprintf("/targets/%d", t.ID))
	WriteJSON(w, http.StatusCreated, targetView(st))
}

func (a *API) listTargets(w http.ResponseWriter, r *http.Request) {
	sts := a.Service.Store().Statuses(UptimeWindow)
	out := make([]targetJSON, len(sts))
	for i, st := range sts {
		out[i] = targetView(st)
	}
	WriteJSON(w, http.StatusOK, out)
}

func (a *API) getTarget(w http.ResponseWriter, r *http.Request) {
	id, ok := pathID(w, r)
	if !ok {
		return
	}
	st, err := a.Service.Store().Status(id, UptimeWindow)
	if err != nil {
		writeDomainError(w, err)
		return
	}
	WriteJSON(w, http.StatusOK, targetView(st))
}

func (a *API) checkTarget(w http.ResponseWriter, r *http.Request) {
	id, ok := pathID(w, r)
	if !ok {
		return
	}
	obs, state, err := a.Service.CheckNow(r.Context(), id)
	if err != nil {
		writeDomainError(w, err)
		return
	}
	st, err := a.Service.Store().Status(id, UptimeWindow)
	if err != nil {
		writeDomainError(w, err)
		return
	}
	WriteJSON(w, http.StatusOK, map[string]any{
		"observed":                obs.Observed,
		"response_ms":             obs.ResponseMS,
		"state":                   state,
		"detail":                  obs.Detail,
		"checked_at":              obs.At,
		"consecutive_disagreeing": st.Disagreeing,
		"threshold":               monitor.Threshold,
		"cert_expires_at":         obs.CertExpiresAt,
	})
}

func (a *API) uptime(w http.ResponseWriter, r *http.Request) {
	id, ok := pathID(w, r)
	if !ok {
		return
	}
	q := r.URL.Query()
	to := a.now().UTC()
	if v := q.Get("to"); v != "" {
		t, err := parseTime(v)
		if err != nil {
			WriteError(w, http.StatusBadRequest, "validation", "to: "+err.Error())
			return
		}
		to = t
	}
	from := to.Add(-UptimeWindow)
	if v := q.Get("from"); v != "" {
		t, err := parseTime(v)
		if err != nil {
			WriteError(w, http.StatusBadRequest, "validation", "from: "+err.Error())
			return
		}
		from = t
	}
	if !from.Before(to) {
		WriteError(w, http.StatusUnprocessableEntity, "unprocessable", "from must be earlier than to")
		return
	}
	u, err := a.Service.Store().Uptime(id, monitor.Period{From: from, To: to})
	if err != nil {
		writeDomainError(w, err)
		return
	}
	WriteJSON(w, http.StatusOK, u)
}

func (a *API) incidents(w http.ResponseWriter, r *http.Request) {
	WriteJSON(w, http.StatusOK, a.Service.Store().Incidents())
}

func (a *API) alerts(w http.ResponseWriter, r *http.Request) {
	WriteJSON(w, http.StatusOK, map[string]any{
		"channels":   a.Alerts.Channels(),
		"deliveries": a.Alerts.Deliveries(),
	})
}

// targetJSON is a target as the list shows it: the definition, the belief,
// the last observation and uptime over a stated window.
type targetJSON struct {
	monitor.Target
	State          monitor.State  `json:"state"`
	StateSince     time.Time      `json:"state_since"`
	Disagreeing    int            `json:"consecutive_disagreeing"`
	Threshold      int            `json:"threshold"`
	LastObserved   *monitor.State `json:"last_observed"`
	LastCheckedAt  *time.Time     `json:"last_checked_at"`
	LastResponseMS *int64         `json:"last_response_ms"`
	LastDetail     *string        `json:"last_detail"`
	CertExpiresAt  *time.Time     `json:"cert_expires_at,omitempty"`
	Uptime         monitor.Uptime `json:"uptime"`
	UptimePercent  *float64       `json:"uptime_percent"`
	UptimePeriod   string         `json:"uptime_period"`
}

func targetView(st monitor.TargetStatus) targetJSON {
	v := targetJSON{
		Target: st.Target, State: st.State, StateSince: st.StateSince,
		Disagreeing: st.Disagreeing, Threshold: monitor.Threshold,
		LastResponseMS: st.LastResponse, CertExpiresAt: st.CertExpiresAt,
		Uptime: st.Uptime, UptimePercent: st.Uptime.UptimePercent, UptimePeriod: "last 24 hours",
	}
	if c := st.LastCheck; c != nil {
		v.LastObserved, v.LastCheckedAt, v.LastDetail = &c.Observed, &c.At, &c.Detail
	}
	return v
}

// parseTime accepts RFC 3339 timestamps and Unix seconds.
func parseTime(s string) (time.Time, error) {
	if t, err := time.Parse(time.RFC3339, s); err == nil {
		return t.UTC(), nil
	}
	if n, err := strconv.ParseInt(s, 10, 64); err == nil {
		return time.Unix(n, 0).UTC(), nil
	}
	return time.Time{}, fmt.Errorf("%q is not an RFC 3339 time (2026-11-15T12:00:00Z) or Unix seconds", s)
}

func pathID(w http.ResponseWriter, r *http.Request) (int, bool) {
	id, err := strconv.Atoi(r.PathValue("id"))
	if err != nil || id < 1 {
		WriteError(w, http.StatusBadRequest, "validation", "target id must be a positive integer")
		return 0, false
	}
	return id, true
}

// decode reads a JSON body into v, answering 400 itself when it cannot.
func decode(w http.ResponseWriter, r *http.Request, v any) bool {
	dec := json.NewDecoder(http.MaxBytesReader(w, r.Body, maxBody))
	dec.DisallowUnknownFields()
	if err := dec.Decode(v); err != nil {
		msg := "body is not valid JSON: " + err.Error()
		var typeErr *json.UnmarshalTypeError
		switch {
		case errors.Is(err, io.EOF):
			msg = "body is empty; expected a JSON object"
		case errors.As(err, &typeErr):
			kind := typeErr.Type.Kind().String()
			if strings.HasPrefix(kind, "int") {
				kind = "integer"
			}
			msg = fmt.Sprintf("field %s must be a %s", typeErr.Field, kind)
		}
		WriteError(w, http.StatusBadRequest, "validation", msg)
		return false
	}
	if dec.More() {
		WriteError(w, http.StatusBadRequest, "validation", "body must contain a single JSON object")
		return false
	}
	return true
}

func writeDomainError(w http.ResponseWriter, err error) {
	var ve *monitor.ValidationError
	switch {
	case errors.As(err, &ve) && ve.Unprocessable:
		WriteError(w, http.StatusUnprocessableEntity, "unprocessable", ve.Message)
	case errors.As(err, &ve):
		WriteError(w, http.StatusBadRequest, "validation", ve.Message)
	case errors.Is(err, monitor.ErrNotFound):
		WriteError(w, http.StatusNotFound, "not_found", err.Error())
	case errors.Is(err, monitor.ErrConflict):
		WriteError(w, http.StatusConflict, "conflict", err.Error())
	default:
		WriteError(w, http.StatusInternalServerError, "internal", err.Error())
	}
}

// WriteJSON is the one place that sets the content type, so every response in
// the service is consistent about it.
func WriteJSON(w http.ResponseWriter, status int, body any) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(body)
}

// WriteError produces the error shape the common contract requires.
func WriteError(w http.ResponseWriter, status int, code, message string) {
	WriteJSON(w, status, map[string]string{"error": code, "message": message})
}
