package tlsedge

import (
	"context"
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/tls"
	"crypto/x509"
	"crypto/x509/pkix"
	"encoding/pem"
	"io"
	"math/big"
	"net"
	"net/http"
	"strings"
	"testing"
	"time"

	"github.com/caddyserver/certmagic"

	"github.com/necorox-com/kotoji/backend/internal/resolve"
)

// selfSignedIssuer is a minimal certmagic.Issuer for tests: it signs the on-demand
// CSR with an in-test CA, standing in for a real ACME CA (Let's Encrypt / pebble).
// CertMagic runs the DecisionFunc BEFORE invoking Issue, so this issuer is only
// ever reached for ALLOWED hosts — exactly the property the integration test pins.
type selfSignedIssuer struct {
	caCert *x509.Certificate
	caKey  *ecdsa.PrivateKey
	// issued records every subject Issue was called for, so the test can assert the
	// gate never let a refused host reach issuance.
	issued chan string
}

func newSelfSignedIssuer(t *testing.T) *selfSignedIssuer {
	t.Helper()
	key, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		t.Fatalf("gen ca key: %v", err)
	}
	tmpl := &x509.Certificate{
		SerialNumber:          big.NewInt(1),
		Subject:               pkix.Name{CommonName: "kotoji-test-ca"},
		NotBefore:             time.Now().Add(-time.Hour),
		NotAfter:              time.Now().Add(24 * time.Hour),
		IsCA:                  true,
		KeyUsage:              x509.KeyUsageCertSign | x509.KeyUsageDigitalSignature,
		BasicConstraintsValid: true,
	}
	der, err := x509.CreateCertificate(rand.Reader, tmpl, tmpl, &key.PublicKey, key)
	if err != nil {
		t.Fatalf("create ca cert: %v", err)
	}
	caCert, err := x509.ParseCertificate(der)
	if err != nil {
		t.Fatalf("parse ca cert: %v", err)
	}
	return &selfSignedIssuer{caCert: caCert, caKey: key, issued: make(chan string, 16)}
}

func (s *selfSignedIssuer) IssuerKey() string { return "kotoji-test-selfsigned" }

// Issue signs the requested CSR's DNS names into a leaf cert chained to the test CA.
func (s *selfSignedIssuer) Issue(_ context.Context, csr *x509.CertificateRequest) (*certmagic.IssuedCertificate, error) {
	if len(csr.DNSNames) > 0 {
		s.issued <- csr.DNSNames[0]
	}
	leaf := &x509.Certificate{
		SerialNumber: big.NewInt(time.Now().UnixNano()),
		Subject:      pkix.Name{CommonName: firstName(csr)},
		DNSNames:     csr.DNSNames,
		NotBefore:    time.Now().Add(-time.Minute),
		NotAfter:     time.Now().Add(24 * time.Hour),
		KeyUsage:     x509.KeyUsageDigitalSignature,
		ExtKeyUsage:  []x509.ExtKeyUsage{x509.ExtKeyUsageServerAuth},
	}
	der, err := x509.CreateCertificate(rand.Reader, leaf, s.caCert, csr.PublicKey, s.caKey)
	if err != nil {
		return nil, err
	}
	pemBytes := pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: der})
	pemBytes = append(pemBytes, pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: s.caCert.Raw})...)
	return &certmagic.IssuedCertificate{Certificate: pemBytes}, nil
}

func firstName(csr *x509.CertificateRequest) string {
	if len(csr.DNSNames) > 0 {
		return csr.DNSNames[0]
	}
	return csr.Subject.CommonName
}

// rootPool returns a cert pool trusting the test CA, for the client's TLS config.
func (s *selfSignedIssuer) rootPool() *x509.CertPool {
	p := x509.NewCertPool()
	p.AddCert(s.caCert)
	return p
}

// TestEngine_OnDemandIssuance_KnownAndUnknownHosts is the integration test the
// design requires: it stands up the engine's on-demand TLS listener (backed by a
// local self-signed CA instead of real Let's Encrypt) and proves
//  1. issuance SUCCEEDS for a KNOWN host (the gate allows it) and the combined
//     handler serves the response over TLS, AND
//  2. issuance is REFUSED for an UNKNOWN host (the DecisionFunc denies it, so the
//     handshake fails and the self-signed issuer is never reached), AND
//  3. BOTH planes (control + data) are reachable over the single TLS listener via
//     SNI/Host routing.
func TestEngine_OnDemandIssuance_KnownAndUnknownHosts(t *testing.T) {
	const baseDomain = "hosting.test"
	const controlHost = "hosting.test"

	// A combined handler that mimics the RUN_MODE=all router: control host answers
	// "control-plane", a project host answers "data-plane:<handle>". This is the
	// SAME shape app.CombinedRouter produces (serve.Handler with a Control hook),
	// reduced to the routing essence so the test stays in-package.
	res := resolve.NewResolver(resolve.Config{
		BaseDomain:         baseDomain,
		EnablePathFallback: true,
		TrustForwardedHost: false,
	})
	combined := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		target, err := res.Resolve(r)
		if err != nil {
			http.Error(w, "resolve: "+err.Error(), http.StatusBadGateway)
			return
		}
		if target.IsControl {
			io.WriteString(w, "control-plane")
			return
		}
		io.WriteString(w, "data-plane:"+target.Handle)
	})

	// The gate allows the control host + the single existing site "calc".
	decider := newTestDecider(t, baseDomain, controlHost, existsForHandles("calc"))

	issuer := newSelfSignedIssuer(t)
	eng, err := New(Config{
		Handler:    combined,
		Decider:    decider,
		StorageDir: t.TempDir(), // certs persist under a throwaway dir for the test
		Issuers:    []certmagic.Issuer{issuer},
		// Ephemeral loopback ports so the test needs no privileges and cannot clash.
		TLSAddr:  "127.0.0.1:0",
		HTTPAddr: "127.0.0.1:0",
	})
	if err != nil {
		t.Fatalf("New engine: %v", err)
	}

	// Stand up the TLS listener directly with the engine's recommended TLS config
	// (GetCertificate -> on-demand cache -> DecisionFunc -> issuer). This exercises
	// the exact handshake path Run uses, on an ephemeral port.
	ln, err := tls.Listen("tcp", "127.0.0.1:0", eng.TLSConfig())
	if err != nil {
		t.Fatalf("tls.Listen: %v", err)
	}
	defer ln.Close()
	srv := &http.Server{Handler: combined}
	go srv.Serve(ln) //nolint:errcheck // closed via Shutdown below
	defer srv.Shutdown(context.Background())
	defer eng.cache.Stop()

	addr := ln.Addr().String()

	// --- (1) + (3a): KNOWN data-plane host issues a cert and serves the data plane. ---
	if body := tlsGet(t, addr, "calc."+baseDomain, true, issuer.rootPool()); body != "data-plane:calc" {
		t.Fatalf("known site host: got %q, want data-plane:calc", body)
	}

	// --- (3b): control host issues a cert and serves the control plane. ---
	if body := tlsGet(t, addr, controlHost, true, issuer.rootPool()); body != "control-plane" {
		t.Fatalf("control host: got %q, want control-plane", body)
	}

	// --- (2): UNKNOWN host is REFUSED — the handshake must fail (no cert issued). ---
	if body := tlsGet(t, addr, "ghost."+baseDomain, false, issuer.rootPool()); body != "" {
		t.Fatalf("unknown host should fail handshake, but got body %q", body)
	}

	// The self-signed issuer must have been reached ONLY for the two allowed hosts.
	close(issuer.issued)
	got := map[string]bool{}
	for name := range issuer.issued {
		got[name] = true
	}
	if !got["calc."+baseDomain] || !got[controlHost] {
		t.Fatalf("expected issuance for the two allowed hosts, got: %v", got)
	}
	if got["ghost."+baseDomain] {
		t.Fatal("UNKNOWN host must NEVER reach the issuer (DecisionFunc must gate it out)")
	}
}

// tlsGet dials addr with the given SNI, does a GET /, and returns the body. When
// wantOK is false it asserts the handshake/request FAILS and returns "".
func tlsGet(t *testing.T, addr, sni string, wantOK bool, roots *x509.CertPool) string {
	t.Helper()
	dialer := &net.Dialer{Timeout: 5 * time.Second}
	client := &http.Client{
		Timeout: 10 * time.Second,
		Transport: &http.Transport{
			DialContext: func(ctx context.Context, network, _ string) (net.Conn, error) {
				// Always dial the loopback listener regardless of the URL host.
				return dialer.DialContext(ctx, network, addr)
			},
			TLSClientConfig: &tls.Config{ServerName: sni, RootCAs: roots},
		},
	}
	// The URL host is the SNI so SNI + Host header match; the dialer ignores it.
	resp, err := client.Get("https://" + sni + "/")
	if !wantOK {
		if err == nil {
			_ = resp.Body.Close()
			t.Fatalf("expected handshake/request to FAIL for refused host %q", sni)
		}
		// A refused on-demand host surfaces as a TLS handshake error client-side.
		if !strings.Contains(strings.ToLower(err.Error()), "tls") &&
			!strings.Contains(strings.ToLower(err.Error()), "handshake") &&
			!strings.Contains(strings.ToLower(err.Error()), "certificate") {
			t.Logf("note: refusal surfaced as: %v", err)
		}
		return ""
	}
	if err != nil {
		t.Fatalf("GET %q: %v", sni, err)
	}
	defer resp.Body.Close()
	b, _ := io.ReadAll(resp.Body)
	return string(b)
}
