// Entry point for your project.
//
// Every ATAD project is a web service in a container. The four routes below are
// the common contract and are checked automatically: keep them working exactly
// as specified and add your own routes alongside them.
//
// --version must also keep working. It prints one line of JSON and exits 0.
package main

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"net/http"
	"os"
	"os/signal"
	"path/filepath"
	"syscall"
	"time"

	"atad-project/internal/alert"
	"atad-project/internal/api"
	"atad-project/internal/monitor"
	"atad-project/internal/version"
)

// started is the moment the process came up; /health reports the difference.
var started = time.Now()

func main() {
	if err := run(); err != nil {
		fmt.Fprintf(os.Stderr, "error: %v\n", err)
		os.Exit(1)
	}
}

func run() error {
	for _, a := range os.Args[1:] {
		if a == "--version" || a == "-v" {
			return version.WriteJSON(os.Stdout)
		}
	}

	log := slog.New(slog.NewTextHandler(os.Stderr, nil))

	// Shut down cleanly on Ctrl-C or on the signal Docker sends when stopping
	// the container, so in-flight requests are allowed to finish.
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()

	store := monitor.NewStore(nil)
	persist := func() {}
	// DATA_FILE keeps history, state and incidents across restarts. Without
	// it a restart starts from nothing, uptime included.
	if path := os.Getenv("DATA_FILE"); path != "" {
		if err := store.Load(path); err != nil {
			return fmt.Errorf("loading state: %w", err)
		}
		p := &monitor.Persister{Store: store, Path: path, Interval: 5 * time.Second, Log: log}
		done := make(chan struct{})
		go func() { p.Run(ctx); close(done) }()
		defer func() { stop(); <-done }() // runs after svc.Stop, so the final save sees every check
		persist = p.Flush
		log.Info("state is persisted", "path", path, "targets", len(store.Targets()))
	}

	alerts := alert.NewDispatcher(alertChannels(log)...)
	svc := monitor.NewService(store, &monitor.NetChecker{}, alerts)
	svc.Start()
	defer svc.Stop()

	mux := http.NewServeMux()

	// ---- the common contract: four routes, identical in every project -------

	mux.HandleFunc("GET /version", func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_ = version.WriteJSON(w)
	})

	mux.HandleFunc("GET /health", func(w http.ResponseWriter, r *http.Request) {
		api.WriteJSON(w, http.StatusOK, map[string]any{
			"status":         "ok",
			"uptime_seconds": int(time.Since(started).Seconds()),
		})
	})

	// The identity page. Replace the placeholders with your own details; the
	// marking script reads this page.
	mux.HandleFunc("GET /{$}", func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/html; charset=utf-8")
		fmt.Fprintf(w, `<!doctype html><html lang="en"><meta charset="utf-8">
<title>%s</title><body>
<h1>%s</h1>
<p>Friskan Dario, group SE</p>
<p>Project 7: Service Health Monitor</p>
</body></html>`, version.Current().App, version.Current().App)
	})

	// Discards everything the service holds, so tests can start from a known
	// state without restarting the container. If your project persists data,
	// this clears what was persisted too.
	mux.HandleFunc("POST /reset", func(w http.ResponseWriter, r *http.Request) {
		svc.Reset()
		alerts.Reset()
		persist()
		w.WriteHeader(http.StatusNoContent)
	})

	// ---- your own routes go below -------------------------------------------

	(&api.API{Service: svc, Alerts: alerts}).Register(mux)

	srv := &http.Server{
		Addr:         ":8080",
		Handler:      mux,
		ReadTimeout:  10 * time.Second,
		WriteTimeout: 10 * time.Second,
		IdleTimeout:  60 * time.Second,
	}

	errCh := make(chan error, 1)
	go func() {
		log.Info("listening", "addr", srv.Addr, "commit", version.Current().Commit)
		if err := srv.ListenAndServe(); err != nil && !errors.Is(err, http.ErrServerClosed) {
			errCh <- err
		}
	}()

	select {
	case err := <-errCh:
		return err
	case <-ctx.Done():
		log.Info("shutting down")
		shutdownCtx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
		defer cancel()
		return srv.Shutdown(shutdownCtx)
	}
}

// alertChannels builds the alert channels from the environment. The log and
// the file are always on, so every state change reaches at least two places;
// ALERT_WEBHOOK_URL adds a third.
func alertChannels(log *slog.Logger) []alert.Channel {
	path := os.Getenv("ALERT_FILE")
	if path == "" {
		path = filepath.Join(os.TempDir(), "service-health-monitor-alerts.jsonl")
	}
	chans := []alert.Channel{alert.LogChannel{Log: log}, &alert.FileChannel{Path: path}}
	if url := os.Getenv("ALERT_WEBHOOK_URL"); url != "" {
		chans = append(chans, &alert.WebhookChannel{URL: url, Client: &http.Client{Timeout: 10 * time.Second}})
	}
	return chans
}
