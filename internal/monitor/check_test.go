package monitor

import (
	"context"
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/tls"
	"crypto/x509"
	"crypto/x509/pkix"
	"io"
	"log"
	"math/big"
	"net"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"
)

func httpTarget(addr string) Target {
	return Target{Name: "t", Type: TypeHTTP, Address: addr, IntervalSeconds: 10, TimeoutSeconds: 1, CertWarnDays: DefaultCertWarnDays}
}

func TestTCPCheck(t *testing.T) {
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	addr := ln.Addr().String()
	c := &NetChecker{}
	tg := Target{Type: TypeTCP, Address: addr, TimeoutSeconds: 1}

	if obs := c.Check(context.Background(), tg); obs.Observed != Up {
		t.Fatalf("listening port observed %s: %s", obs.Observed, obs.Detail)
	}
	ln.Close()
	if obs := c.Check(context.Background(), tg); obs.Observed != Down {
		t.Fatalf("closed port observed %s", obs.Observed)
	}
}

func TestHTTPCheck(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/ok":
		case "/moved":
			http.Redirect(w, r, "/broken", http.StatusFound)
		case "/broken":
			w.WriteHeader(http.StatusInternalServerError)
		case "/slow":
			time.Sleep(1500 * time.Millisecond)
		}
	}))
	defer srv.Close()
	c := &NetChecker{}

	cases := map[string]State{"/ok": Up, "/moved": Up, "/broken": Down, "/slow": Down}
	for path, want := range cases {
		obs := c.Check(context.Background(), httpTarget(srv.URL+path))
		if obs.Observed != want {
			t.Errorf("%s observed %s (%s), want %s", path, obs.Observed, obs.Detail, want)
		}
	}
	slow := c.Check(context.Background(), httpTarget(srv.URL+"/slow"))
	if !strings.Contains(slow.Detail, "timeout") || slow.ResponseMS < 900 {
		t.Errorf("slow check: %+v", slow)
	}
}

// This is the check that catches an expiring certificate: a server that
// answers 200 OK, with a valid certificate that runs out in three days, is
// observed down, and the observation says why.
func TestCertificateExpiringSoonIsDown(t *testing.T) {
	now := time.Now()
	cases := []struct {
		name     string
		notAfter time.Time
		want     State
		detail   string
	}{
		{"healthy", now.Add(90 * 24 * time.Hour), Up, "200 OK"},
		{"expires in 3 days", now.Add(3 * 24 * time.Hour), Down, "certificate expires in 3 days"},
		{"expired", now.Add(-time.Hour), Down, "certificate rejected"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			srv, pool := tlsServer(t, tc.notAfter)
			c := &NetChecker{RootCAs: pool}
			obs := c.Check(context.Background(), httpTarget(srv.URL))
			if obs.Observed != tc.want || !strings.Contains(obs.Detail, tc.detail) {
				t.Fatalf("observed %s (%s), want %s containing %q", obs.Observed, obs.Detail, tc.want, tc.detail)
			}
			if tc.want == Up && (obs.CertExpiresAt == nil || !obs.CertExpiresAt.Equal(tc.notAfter.UTC().Truncate(time.Second))) {
				t.Fatalf("cert expiry not recorded: %v", obs.CertExpiresAt)
			}
		})
	}
}

func tlsServer(t *testing.T, notAfter time.Time) (*httptest.Server, *x509.CertPool) {
	t.Helper()
	key, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	tmpl := &x509.Certificate{
		SerialNumber:          big.NewInt(1),
		Subject:               pkix.Name{CommonName: "127.0.0.1"},
		NotBefore:             time.Now().Add(-48 * time.Hour),
		NotAfter:              notAfter,
		IPAddresses:           []net.IP{net.ParseIP("127.0.0.1")},
		KeyUsage:              x509.KeyUsageDigitalSignature | x509.KeyUsageCertSign,
		ExtKeyUsage:           []x509.ExtKeyUsage{x509.ExtKeyUsageServerAuth},
		BasicConstraintsValid: true,
		IsCA:                  true,
	}
	der, err := x509.CreateCertificate(rand.Reader, tmpl, tmpl, &key.PublicKey, key)
	if err != nil {
		t.Fatal(err)
	}
	cert, _ := x509.ParseCertificate(der)
	pool := x509.NewCertPool()
	pool.AddCert(cert)

	srv := httptest.NewUnstartedServer(http.HandlerFunc(func(http.ResponseWriter, *http.Request) {}))
	srv.TLS = &tls.Config{Certificates: []tls.Certificate{{Certificate: [][]byte{der}, PrivateKey: key}}}
	srv.Config.ErrorLog = log.New(io.Discard, "", 0) // the expired case is meant to fail the handshake
	srv.StartTLS()
	t.Cleanup(srv.Close)
	return srv, pool
}
