package api

import (
	"fmt"
	"html/template"
	"net/http"
	"time"

	"atad-project/internal/monitor"
)

// The status page is for people who use the services, not for the people who
// run them: plain words, no addresses, no response codes, and every
// percentage says what period it covers.

const statusWeek = 7 * 24 * time.Hour

type statusService struct {
	Name      string
	Class     string // ok, watch, down
	Headline  string
	Note      string
	Uptime24h string
	Uptime7d  string
	Checked   string
}

type statusIncident struct {
	Title   string
	When    string
	Ongoing bool
}

type statusData struct {
	Overall      string
	OverallClass string
	Services     []statusService
	Incidents    []statusIncident
	Generated    string
	Threshold    int
}

func (a *API) statusPage(w http.ResponseWriter, r *http.Request) {
	now := a.now().UTC()
	store := a.Service.Store()
	week := store.Statuses(statusWeek)
	day := store.Statuses(UptimeWindow)

	d := statusData{Generated: now.Format("2 January 2006, 15:04 MST"), Threshold: monitor.Threshold}
	down := 0
	for i, st := range day {
		svc := statusService{
			Name:      st.Name,
			Uptime24h: percent(st.Uptime),
			Uptime7d:  percent(week[i].Uptime),
			Checked:   "Not checked yet",
		}
		if st.LastCheck != nil {
			svc.Checked = "Last checked " + ago(now.Sub(st.LastCheck.At))
		}
		switch {
		case st.State == monitor.Down && st.Disagreeing > 0:
			svc.Class, svc.Headline = "watch", "Recovering"
			svc.Note = "Recent checks look good again; we are confirming before calling it fixed."
			down++
		case st.State == monitor.Down:
			svc.Class, svc.Headline = "down", "Not working"
			svc.Note = "Unavailable since " + clock(st.StateSince, now) + ". We have been alerted."
			if inc := ongoingFor(store.Incidents(), st.ID); inc != nil {
				svc.Note = "Unavailable since " + clock(inc.StartedAt, now) + ". We have been alerted."
			}
			down++
		case st.Disagreeing > 0:
			svc.Class, svc.Headline = "watch", "Working"
			svc.Note = "A recent check did not get a good answer. We are watching it; one failed check is not an outage."
		default:
			svc.Class, svc.Headline = "ok", "Working normally"
		}
		d.Services = append(d.Services, svc)
	}

	switch {
	case len(day) == 0:
		d.Overall, d.OverallClass = "No services are being monitored yet", "ok"
	case down == 0:
		d.Overall, d.OverallClass = "All services are working normally", "ok"
	case down == len(day) && down > 1:
		d.Overall, d.OverallClass = "All services are having problems", "down"
	case down == 1:
		d.Overall, d.OverallClass = "One service is having problems", "down"
	default:
		d.Overall, d.OverallClass = fmt.Sprintf("%d services are having problems", down), "down"
	}

	for _, inc := range store.Incidents() {
		if inc.EndedAt != nil && now.Sub(*inc.EndedAt) > statusWeek {
			continue
		}
		si := statusIncident{Ongoing: inc.Ongoing()}
		if si.Ongoing {
			si.Title = inc.TargetName + " is unavailable"
			si.When = "Since " + clock(inc.StartedAt, now) + " (" + duration(now.Sub(inc.StartedAt)) + " so far)"
		} else {
			si.Title = inc.TargetName + " was unavailable"
			si.When = "From " + clock(inc.StartedAt, now) + " to " + clock(*inc.EndedAt, now) +
				" (" + duration(inc.EndedAt.Sub(inc.StartedAt)) + ")"
		}
		d.Incidents = append(d.Incidents, si)
	}

	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	if err := statusTmpl.Execute(w, d); err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
	}
}

func ongoingFor(incs []monitor.Incident, targetID int) *monitor.Incident {
	for i := range incs {
		if incs[i].TargetID == targetID && incs[i].Ongoing() {
			return &incs[i]
		}
	}
	return nil
}

func percent(u monitor.Uptime) string {
	if u.UptimePercent == nil {
		return "no data"
	}
	return fmt.Sprintf("%.2f%%", *u.UptimePercent)
}

func clock(t, now time.Time) string {
	t = t.UTC()
	if t.YearDay() == now.YearDay() && t.Year() == now.Year() {
		return t.Format("15:04 MST") + " today"
	}
	return t.Format("2 Jan 15:04 MST")
}

func ago(d time.Duration) string {
	if d < time.Minute {
		return "less than a minute ago"
	}
	return duration(d) + " ago"
}

func duration(d time.Duration) string {
	switch {
	case d < time.Minute:
		return "under a minute"
	case d < time.Hour:
		return plural(int(d.Minutes()), "minute")
	case d < 48*time.Hour:
		h, m := int(d.Hours()), int(d.Minutes())%60
		if m == 0 {
			return plural(h, "hour")
		}
		return plural(h, "hour") + " " + plural(m, "minute")
	default:
		return plural(int(d.Hours()/24), "day")
	}
}

func plural(n int, unit string) string {
	if n == 1 {
		return "1 " + unit
	}
	return fmt.Sprintf("%d %ss", n, unit)
}

var statusTmpl = template.Must(template.New("status").Parse(`<!doctype html>
<html lang="en">
<head>
<meta charset="utf-8">
<meta name="viewport" content="width=device-width, initial-scale=1">
<meta http-equiv="refresh" content="30">
<title>Service status</title>
<style>
  :root { --ok:#1a7f37; --watch:#9a6700; --down:#cf222e; --muted:#59636e; --line:#d1d9e0; --bg:#f6f8fa; }
  * { box-sizing: border-box; }
  body { margin:0; padding:2rem 1rem; font:16px/1.5 system-ui, -apple-system, "Segoe UI", sans-serif; color:#1f2328; background:var(--bg); }
  main { max-width: 46rem; margin: 0 auto; }
  h1 { font-size:1.6rem; margin:0 0 1rem; }
  h2 { font-size:1.15rem; margin:2rem 0 .75rem; }
  .banner { padding:1rem 1.25rem; border-radius:.5rem; color:#fff; font-weight:600; font-size:1.1rem; }
  .banner.ok { background:var(--ok); } .banner.down { background:var(--down); }
  .card { background:#fff; border:1px solid var(--line); border-radius:.5rem; }
  .svc { display:flex; flex-wrap:wrap; gap:.25rem 1rem; justify-content:space-between; padding:1rem 1.25rem; border-top:1px solid var(--line); }
  .svc:first-child { border-top:0; }
  .name { font-weight:600; }
  .state { font-weight:600; }
  .state::before { content:""; display:inline-block; width:.7rem; height:.7rem; border-radius:50%; margin-right:.45rem; background:currentColor; }
  .ok .state { color:var(--ok); } .watch .state { color:var(--watch); } .down .state { color:var(--down); }
  .note { flex-basis:100%; margin:0; }
  .meta { flex-basis:100%; color:var(--muted); font-size:.9rem; }
  .inc { padding:.85rem 1.25rem; border-top:1px solid var(--line); }
  .inc:first-child { border-top:0; }
  .inc .when { color:var(--muted); font-size:.9rem; }
  .ongoing { color:var(--down); font-weight:600; }
  .empty { padding:1rem 1.25rem; color:var(--muted); }
  footer { margin-top:2rem; color:var(--muted); font-size:.85rem; }
</style>
</head>
<body>
<main>
  <h1>Service status</h1>
  <div class="banner {{.OverallClass}}">{{.Overall}}</div>

  <h2>Services</h2>
  <div class="card">
  {{range .Services}}
    <div class="svc {{.Class}}">
      <span class="name">{{.Name}}</span>
      <span class="state">{{.Headline}}</span>
      {{if .Note}}<p class="note">{{.Note}}</p>{{end}}
      <span class="meta">Available {{.Uptime24h}} of checks in the last 24 hours · {{.Uptime7d}} in the last 7 days · {{.Checked}}</span>
    </div>
  {{else}}
    <div class="empty">Nothing is being monitored yet.</div>
  {{end}}
  </div>

  <h2>Recent incidents (last 7 days)</h2>
  <div class="card">
  {{range .Incidents}}
    <div class="inc">
      <div>{{if .Ongoing}}<span class="ongoing">Ongoing:</span> {{end}}{{.Title}}</div>
      <div class="when">{{.When}}</div>
    </div>
  {{else}}
    <div class="empty">No incidents in the last 7 days.</div>
  {{end}}
  </div>

  <footer>
    A service is only marked as not working after {{.Threshold}} checks in a row fail, so a single
    hiccup does not count as an outage. This page refreshes every 30 seconds.
    Updated {{.Generated}}.
  </footer>
</main>
</body>
</html>
`))
