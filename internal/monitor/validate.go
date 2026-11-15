package monitor

import (
	"fmt"
	"net"
	"net/url"
	"strconv"
	"strings"
)

// Limits on what a target may ask for. A check more often than once a second
// is a load test, and a timeout over a minute is not a health check.
const (
	MaxIntervalSeconds = 86_400
	MaxTimeoutSeconds  = 60
	MaxCertWarnDays    = 365
)

// ValidationError explains why a target was refused. Unprocessable is set when
// every field is well formed but they do not make sense together; the API
// answers 422 for those and 400 for the rest.
type ValidationError struct {
	Message       string
	Unprocessable bool
}

func (e *ValidationError) Error() string { return e.Message }

func invalid(format string, a ...any) error {
	return &ValidationError{Message: fmt.Sprintf(format, a...)}
}

// Validate checks a target as submitted by a client and normalises its name.
func Validate(t *Target) error {
	t.Name = strings.TrimSpace(t.Name)
	t.Address = strings.TrimSpace(t.Address)
	switch {
	case t.Name == "":
		return invalid("name is required")
	case len(t.Name) > 100:
		return invalid("name must be at most 100 characters")
	case t.Type != TypeHTTP && t.Type != TypeTCP:
		return invalid(`type must be "http" or "tcp"`)
	case t.Address == "":
		return invalid("address is required")
	case t.IntervalSeconds < 1 || t.IntervalSeconds > MaxIntervalSeconds:
		return invalid("interval_seconds must be between 1 and %d", MaxIntervalSeconds)
	case t.TimeoutSeconds < 1 || t.TimeoutSeconds > MaxTimeoutSeconds:
		return invalid("timeout_seconds must be between 1 and %d", MaxTimeoutSeconds)
	case t.CertWarnDays < 0 || t.CertWarnDays > MaxCertWarnDays:
		return invalid("cert_warn_days must be between 0 and %d", MaxCertWarnDays)
	}

	switch t.Type {
	case TypeHTTP:
		u, err := url.Parse(t.Address)
		if err != nil || (u.Scheme != "http" && u.Scheme != "https") || u.Host == "" {
			return invalid("address for an http target must be a URL such as https://example.com/health")
		}
	case TypeTCP:
		host, port, err := net.SplitHostPort(t.Address)
		if err != nil || host == "" {
			return invalid("address for a tcp target must be host:port, such as 127.0.0.1:5432")
		}
		if p, err := strconv.Atoi(port); err != nil || p < 1 || p > 65535 {
			return invalid("port must be a number between 1 and 65535")
		}
	}

	// Well formed, but a check that may still be running when the next one
	// is due would let checks of the same target overlap.
	if t.TimeoutSeconds > t.IntervalSeconds {
		return &ValidationError{
			Message:       "timeout_seconds must not exceed interval_seconds",
			Unprocessable: true,
		}
	}
	return nil
}
