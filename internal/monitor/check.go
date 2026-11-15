package monitor

import (
	"context"
	"crypto/tls"
	"crypto/x509"
	"errors"
	"fmt"
	"io"
	"math"
	"net"
	"net/http"
	"time"
)

// Checker performs one check of a target and reports what it saw.
type Checker interface {
	Check(ctx context.Context, t Target) Observation
}

// NetChecker checks targets over the network.
//
// An HTTP target is up when it answers with a status below 400 within the
// timeout. A 5xx means the service is reachable and broken, which is down. An
// HTTPS target is also down when its certificate fails verification or expires
// within the target's cert_warn_days: a certificate that expires on Saturday
// is a Saturday outage that has already been scheduled.
//
// A TCP target is up when a connection is accepted within the timeout.
type NetChecker struct {
	// RootCAs overrides the system roots; tests use it to trust their own
	// certificates. Nil means the system roots.
	RootCAs *x509.CertPool
	// Now is the clock used for certificate arithmetic; nil means time.Now.
	Now func() time.Time
}

func (c *NetChecker) now() time.Time {
	if c.Now != nil {
		return c.Now()
	}
	return time.Now()
}

// Check runs the check appropriate to the target's type.
func (c *NetChecker) Check(ctx context.Context, t Target) Observation {
	ctx, cancel := context.WithTimeout(ctx, t.Timeout())
	defer cancel()
	start := time.Now()
	var obs Observation
	switch t.Type {
	case TypeHTTP:
		obs = c.checkHTTP(ctx, t)
	case TypeTCP:
		obs = c.checkTCP(ctx, t)
	default:
		obs = Observation{Observed: Down, Detail: "unknown check type " + t.Type}
	}
	obs.At = start.UTC()
	obs.ResponseMS = time.Since(start).Milliseconds()
	return obs
}

func (c *NetChecker) checkTCP(ctx context.Context, t Target) Observation {
	var d net.Dialer
	conn, err := d.DialContext(ctx, "tcp", t.Address)
	if err != nil {
		return Observation{Observed: Down, Detail: describe(err, t)}
	}
	conn.Close()
	return Observation{Observed: Up, Detail: "connection accepted"}
}

func (c *NetChecker) checkHTTP(ctx context.Context, t Target) Observation {
	transport := &http.Transport{
		TLSClientConfig:   &tls.Config{RootCAs: c.RootCAs},
		DisableKeepAlives: true, // every check pays the full connection cost, as a new visitor would
	}
	defer transport.CloseIdleConnections()
	client := &http.Client{
		Transport: transport,
		// Judge the target itself, not wherever it redirects to.
		CheckRedirect: func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse },
	}

	req, err := http.NewRequestWithContext(ctx, http.MethodGet, t.Address, nil)
	if err != nil {
		return Observation{Observed: Down, Detail: err.Error()}
	}
	req.Header.Set("User-Agent", "service-health-monitor")
	resp, err := client.Do(req)
	if err != nil {
		return Observation{Observed: Down, Detail: describe(err, t)}
	}
	defer resp.Body.Close()
	_, _ = io.Copy(io.Discard, io.LimitReader(resp.Body, 64<<10))

	obs := Observation{Observed: Up, Detail: "HTTP " + resp.Status}
	if resp.StatusCode >= 400 {
		obs.Observed = Down
	}
	if resp.TLS != nil && len(resp.TLS.PeerCertificates) > 0 {
		c.judgeCertificate(&obs, resp.TLS.PeerCertificates[0], t)
	}
	return obs
}

// judgeCertificate is the expiring-certificate check. It records the expiry on
// every HTTPS observation, and turns an otherwise good observation into a
// failure once the certificate is inside the warning window.
func (c *NetChecker) judgeCertificate(obs *Observation, cert *x509.Certificate, t Target) {
	expires := cert.NotAfter.UTC()
	obs.CertExpiresAt = &expires
	left := expires.Sub(c.now())
	warn := time.Duration(t.CertWarnDays) * 24 * time.Hour
	if left < warn {
		obs.Observed = Down
		obs.Detail = fmt.Sprintf("%s; certificate expires in %s (%s), inside the %d-day warning window",
			obs.Detail, humanDuration(left), expires.Format(time.RFC3339), t.CertWarnDays)
	}
}

// describe turns a network error into something worth putting in an incident.
func describe(err error, t Target) string {
	var certErr *tls.CertificateVerificationError
	switch {
	case errors.Is(err, context.DeadlineExceeded) || isTimeout(err):
		return fmt.Sprintf("no answer within %ds timeout", t.TimeoutSeconds)
	case errors.As(err, &certErr):
		return "certificate rejected: " + certErr.Err.Error()
	default:
		return err.Error()
	}
}

func isTimeout(err error) bool {
	var ne net.Error
	return errors.As(err, &ne) && ne.Timeout()
}

func humanDuration(d time.Duration) string {
	if d <= 0 {
		return "0s (already expired)"
	}
	if d >= 48*time.Hour {
		return fmt.Sprintf("%d days", int(math.Round(d.Hours()/24)))
	}
	return d.Round(time.Minute).String()
}
