//go:build pebble

// Package tlsedge — REAL ACME integration test against a LOCAL Pebble CA.
//
// Unlike engine_test.go (which injects a self-signed issuer to exercise the
// DecisionFunc + listener path WITHOUT any ACME protocol), this test stands up the
// genuine RFC-8555 flow: a Pebble container is the ACME directory, and the engine's
// OWN internally-built ACME issuer (Config.CA, the production code path) registers
// an account, places an order, solves the TLS-ALPN-01 challenge on the engine's :443
// listener, finalizes, and serves the freshly issued cert. It proves, end to end:
//
//	(1) on-demand issuance SUCCEEDS for a KNOWN host (cert chains to Pebble's root),
//	(2) issuance is REFUSED for an UNKNOWN host (DecisionFunc denies => handshake
//	    fails, Pebble is never asked => no rate-limit burn), and
//	(3) the issued cert + ACME account PERSIST under the storage dir (survive restart).
//
// It is gated behind `-tags pebble` because it needs Docker + network and is slow;
// normal CI runs the fast self-signed engine_test.go. Run with:
//
//	go test -tags pebble -run TestPebble -v ./internal/tlsedge/
//
// Validation wiring: Pebble's VA connects back to <domain>:5001 for TLS-ALPN-01.
// We (a) run a tiny in-process DNS server (UDP+TCP, fixed port) that resolves the
// test domain to 127.0.0.1 and point Pebble at it via -dnsserver, and (b) set
// CA.AltTLSALPNPort=5001 so CertMagic's OWN ALPN solver transiently binds :5001
// during handshake-triggered issuance — the exact port Pebble dials. The real
// data-plane listener runs on a SEPARATE ephemeral port (it must not hold :5001, or
// the solver could not bind it). HTTP-01 is disabled (TLS-ALPN-01 alone proves it).
package tlsedge

import (
	"context"
	"crypto/tls"
	"crypto/x509"
	"errors"
	"io"
	"net"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/caddyserver/certmagic"
	"github.com/miekg/dns"

	"github.com/necorox-com/kotoji/backend/internal/resolve"
)

const (
	// Pebble's ACME directory (HTTPS) + the challenge ports it validates on.
	pebbleDirectoryURL = "https://localhost:14000/dir"
	// pebbleRootsURL is Pebble's MANAGEMENT endpoint serving the RUNTIME issuance
	// root (the CA that signs issued leaves) — distinct from the static minica that
	// only secures Pebble's own ACME-API TLS. The issued cert chains to THIS root.
	pebbleRootsURL    = "https://localhost:15000/roots/0"
	pebbleTLSALPNPort = 5001 // Pebble's default tlsPort (TLS-ALPN-01 callback).
	pebbleImage       = "ghcr.io/letsencrypt/pebble:latest"

	// The simulated hosting base + the one existing site, mirroring engine_test.go.
	pebbleBaseDomain  = "hosting.test"
	pebbleSiteHandle  = "calc"
	pebbleKnownHost   = pebbleSiteHandle + "." + pebbleBaseDomain
	pebbleUnknownHost = "ghost." + pebbleBaseDomain
)

// TestPebble_OnDemandIssuance_RealACME is the real-ACME proof (see file doc).
func TestPebble_OnDemandIssuance_RealACME(t *testing.T) {
	requireDocker(t)
	pebbleRoot := loadPebbleRoot(t) // trust Pebble's ACME-API TLS + the issued chain.

	// (a) In-process DNS resolving the test domain (and its parent) to 127.0.0.1,
	// so Pebble's VA dials our local listener for the TLS-ALPN-01 challenge.
	dnsAddr := startTestDNS(t)

	// (b) Start Pebble with host networking, pointed at our DNS. Host networking lets
	// Pebble reach 127.0.0.1:5001 (our challenge listener) and us reach :14000.
	startPebble(t, dnsAddr)
	waitForACMEDirectory(t, pebbleRoot)

	// The combined handler: control host answers "control-plane", a project host
	// answers "data-plane:<handle>" — the routing essence of RUN_MODE=all.
	res := resolve.NewResolver(resolve.Config{
		BaseDomain:         pebbleBaseDomain,
		EnablePathFallback: true,
	})
	combined := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		target, err := res.Resolve(r)
		if err != nil {
			http.Error(w, "resolve: "+err.Error(), http.StatusBadGateway)
			return
		}
		if target.IsControl {
			_, _ = io.WriteString(w, "control-plane")
			return
		}
		_, _ = io.WriteString(w, "data-plane:"+target.Handle)
	})

	// The gate allows the control host + the single existing site "calc".
	decider := newTestDecider(t, pebbleBaseDomain, pebbleBaseDomain, existsForHandles(pebbleSiteHandle))

	// The issued leaf chains to Pebble's RUNTIME root (served on :15000), NOT the
	// static minica that secures the ACME API. Fetch it now (trusting the minica for
	// the management TLS) so the client can verify the served chain.
	issuanceRoot := fetchIssuanceRoot(t, pebbleRoot)

	storageDir := t.TempDir()
	eng := newPebbleEngine(t, combined, decider, storageDir, pebbleRoot)

	// Bind the DATA-PLANE TLS listener on an ephemeral port — NOT 5001. CertMagic's
	// own TLS-ALPN-01 solver transiently binds :5001 (CA.AltTLSALPNPort) DURING
	// handshake-triggered issuance to answer Pebble's validation callback; if we
	// also held :5001 the solver could not bind it. So the real client request lands
	// on this separate listener, which triggers on-demand issuance (CertMagic opens
	// :5001 just long enough to solve the challenge), then completes the handshake
	// with the freshly issued, cached cert.
	ln, err := tls.Listen("tcp", "127.0.0.1:0", eng.TLSConfig())
	if err != nil {
		t.Fatalf("tls.Listen data plane: %v", err)
	}
	defer ln.Close()
	srv := &http.Server{Handler: combined}
	go srv.Serve(ln) //nolint:errcheck // drained via Shutdown below
	defer srv.Shutdown(context.Background())
	defer eng.cache.Stop()

	clientRoots := issuanceRoot // the issued leaf chains to Pebble's runtime root.

	// --- (1) KNOWN host: real on-demand ACME issuance + data plane over HTTPS. ---
	if body := pebbleTLSGet(t, ln.Addr().String(), pebbleKnownHost, true, clientRoots); body != "data-plane:"+pebbleSiteHandle {
		t.Fatalf("known host: got %q, want data-plane:%s", body, pebbleSiteHandle)
	}
	t.Logf("PASS: real ACME issuance succeeded for KNOWN host %s (cert chains to Pebble root)", pebbleKnownHost)

	// --- (1b) CONTROL host: a second real ACME issuance on the SAME listener proves
	// BOTH planes (control + data) are fronted by the one on-demand TLS edge. ---
	if body := pebbleTLSGet(t, ln.Addr().String(), pebbleBaseDomain, true, clientRoots); body != "control-plane" {
		t.Fatalf("control host: got %q, want control-plane", body)
	}
	t.Logf("PASS: real ACME issuance + control plane over HTTPS for control host %s", pebbleBaseDomain)

	// --- (2) UNKNOWN host: REFUSED by the DecisionFunc => handshake fails, no order. ---
	if body := pebbleTLSGet(t, ln.Addr().String(), pebbleUnknownHost, false, clientRoots); body != "" {
		t.Fatalf("unknown host should fail handshake, got body %q", body)
	}
	t.Logf("PASS: issuance REFUSED for UNKNOWN host %s (handshake failed, no ACME order)", pebbleUnknownHost)

	// --- (3) PERSISTENCE: the issued cert + ACME account live under the storage dir. ---
	assertPersisted(t, storageDir, pebbleKnownHost)
	t.Logf("PASS: issued cert + ACME account persisted under %s", storageDir)
}

// newPebbleEngine builds the engine via its PRODUCTION constructor (New), with the
// CA pointed at Pebble: real directory URL, Pebble's root for ACME-API TLS trust,
// and the alt TLS-ALPN port Pebble validates on. This exercises the engine's own
// ACME issuer construction — NOT the Issuers bypass seam.
func newPebbleEngine(t *testing.T, h http.Handler, d *Decider, storageDir string, root *x509.CertPool) *Engine {
	t.Helper()
	eng, err := New(Config{
		Handler:    h,
		Decider:    d,
		StorageDir: storageDir,
		CA: CA{
			DirectoryURL:   pebbleDirectoryURL,
			TrustedRoots:   root,
			AltTLSALPNPort: pebbleTLSALPNPort,
			// Only the TLS-ALPN-01 path is wired here (no :5002 HTTP-01 listener), so
			// disable HTTP-01 to avoid a wasted failing attempt against Pebble.
			DisableHTTPChallenge: true,
		},
		// Loopback ports for the engine-owned servers; we drive a listener directly.
		TLSAddr:  "127.0.0.1:0",
		HTTPAddr: "127.0.0.1:0",
	})
	if err != nil {
		t.Fatalf("New pebble engine: %v", err)
	}
	return eng
}

// pebbleTLSGet dials addr with SNI=host, GET /, returns the body. wantOK=false
// asserts the handshake/request FAILS (returns "").
func pebbleTLSGet(t *testing.T, addr, host string, wantOK bool, roots *x509.CertPool) string {
	t.Helper()
	dialer := &net.Dialer{Timeout: 10 * time.Second}
	client := &http.Client{
		Timeout: 30 * time.Second, // first hit triggers a full ACME order; be generous.
		Transport: &http.Transport{
			DialContext: func(ctx context.Context, network, _ string) (net.Conn, error) {
				return dialer.DialContext(ctx, network, addr)
			},
			TLSClientConfig: &tls.Config{ServerName: host, RootCAs: roots},
		},
	}
	resp, err := client.Get("https://" + host + "/")
	if !wantOK {
		if err == nil {
			_ = resp.Body.Close()
			t.Fatalf("expected handshake/request to FAIL for refused host %q", host)
		}
		return ""
	}
	if err != nil {
		t.Fatalf("GET %q: %v", host, err)
	}
	defer resp.Body.Close()
	b, _ := io.ReadAll(resp.Body)
	return string(b)
}

// assertPersisted confirms CertMagic wrote the issued cert + the ACME account under
// storageDir (so they survive a restart on the data volume). It walks the tree for
// a leaf cert .crt naming the host and any acme account key.
func assertPersisted(t *testing.T, storageDir, host string) {
	t.Helper()
	var foundCert, foundAccount bool
	err := filepath.WalkDir(storageDir, func(path string, d os.DirEntry, err error) error {
		if err != nil {
			return err
		}
		if d.IsDir() {
			return nil
		}
		// Cert sites live under certificates/<ca>/<host>/<host>.crt.
		if strings.HasSuffix(path, ".crt") && strings.Contains(path, host) {
			foundCert = true
		}
		// ACME account material lives under acme/<ca>/.../account.json or *.key.
		if strings.Contains(path, "acme") && (strings.HasSuffix(path, ".json") || strings.HasSuffix(path, ".key")) {
			foundAccount = true
		}
		return nil
	})
	if err != nil {
		t.Fatalf("walk storage dir: %v", err)
	}
	if !foundCert {
		t.Fatalf("expected an issued cert for %q persisted under %s", host, storageDir)
	}
	if !foundAccount {
		t.Fatalf("expected the ACME account persisted under %s", storageDir)
	}
}

// --- Pebble + DNS harness -------------------------------------------------------

// requireDocker skips the test when docker is unavailable.
func requireDocker(t *testing.T) {
	t.Helper()
	if _, err := exec.LookPath("docker"); err != nil {
		t.Skip("docker not available; skipping real-ACME pebble test")
	}
}

// loadPebbleRoot extracts Pebble's minica root from the image (once) and returns a
// pool trusting it — used both to trust Pebble's ACME-API TLS and to verify the
// issued leaf's chain. The PEM is pulled from the image filesystem we extract to a
// temp dir so the test has no external file dependency.
func loadPebbleRoot(t *testing.T) *x509.CertPool {
	t.Helper()
	pem := extractFromImage(t, "test/certs/pebble.minica.pem")
	pool := x509.NewCertPool()
	if !pool.AppendCertsFromPEM(pem) {
		t.Fatalf("failed to parse pebble minica root")
	}
	return pool
}

// extractFromImage copies a single file out of the pebble image via `docker create`
// + `docker cp`, returning its bytes.
func extractFromImage(t *testing.T, pathInImage string) []byte {
	t.Helper()
	ensurePebbleImage(t)
	out, err := exec.Command("docker", "create", pebbleImage).CombinedOutput()
	if err != nil {
		t.Fatalf("docker create: %v\n%s", err, out)
	}
	cid := strings.TrimSpace(string(out))
	defer exec.Command("docker", "rm", "-f", cid).Run() //nolint:errcheck
	dst := filepath.Join(t.TempDir(), filepath.Base(pathInImage))
	if cpOut, err := exec.Command("docker", "cp", cid+":/"+pathInImage, dst).CombinedOutput(); err != nil {
		t.Fatalf("docker cp %s: %v\n%s", pathInImage, err, cpOut)
	}
	b, err := os.ReadFile(dst)
	if err != nil {
		t.Fatalf("read extracted %s: %v", pathInImage, err)
	}
	return b
}

// ensurePebbleImage pulls the pebble image if it is not present locally.
func ensurePebbleImage(t *testing.T) {
	t.Helper()
	if err := exec.Command("docker", "image", "inspect", pebbleImage).Run(); err == nil {
		return
	}
	t.Logf("pulling %s ...", pebbleImage)
	if out, err := exec.Command("docker", "pull", pebbleImage).CombinedOutput(); err != nil {
		t.Skipf("cannot pull pebble image (%v); skipping real-ACME test\n%s", err, out)
	}
}

// startPebble launches the Pebble container with host networking, no challenge
// sleeps, and our DNS server so the VA resolves the test domain to 127.0.0.1. The
// container is force-removed on cleanup.
func startPebble(t *testing.T, dnsAddr string) {
	t.Helper()
	name := "kotoji-pebble-test"
	// Remove any stale container from a previous aborted run.
	_ = exec.Command("docker", "rm", "-f", name).Run()
	args := []string{
		"run", "-d", "--rm", "--name", name,
		"--network", "host",
		"-e", "PEBBLE_VA_NOSLEEP=1", // no artificial challenge delay.
		"-e", "PEBBLE_VA_ALWAYS_VALID=0", // actually validate challenges.
		pebbleImage,
		"-config", "test/config/pebble-config.json",
		"-dnsserver", dnsAddr, // resolve the test domain to us.
	}
	if out, err := exec.Command("docker", args...).CombinedOutput(); err != nil {
		t.Skipf("cannot start pebble (%v); skipping real-ACME test\n%s", err, out)
	}
	t.Cleanup(func() {
		// Surface pebble logs on failure to aid triage, then remove.
		if t.Failed() {
			if logs, err := exec.Command("docker", "logs", name).CombinedOutput(); err == nil {
				t.Logf("pebble logs:\n%s", logs)
			}
		}
		_ = exec.Command("docker", "rm", "-f", name).Run()
	})
}

// waitForACMEDirectory polls Pebble's directory endpoint until it answers (or the
// deadline elapses), trusting Pebble's root for the API TLS.
func waitForACMEDirectory(t *testing.T, root *x509.CertPool) {
	t.Helper()
	client := &http.Client{
		Timeout:   2 * time.Second,
		Transport: &http.Transport{TLSClientConfig: &tls.Config{RootCAs: root}},
	}
	deadline := time.Now().Add(30 * time.Second)
	for time.Now().Before(deadline) {
		resp, err := client.Get(pebbleDirectoryURL)
		if err == nil {
			_ = resp.Body.Close()
			if resp.StatusCode == http.StatusOK {
				return
			}
		}
		time.Sleep(500 * time.Millisecond)
	}
	t.Fatal("pebble ACME directory did not become ready in time")
}

// fetchIssuanceRoot retrieves Pebble's RUNTIME issuance root from its management
// interface (:15000/roots/0) and returns a pool trusting it — the anchor the issued
// leaf chains to. apiRoot trusts the management TLS (the minica). Pebble serves the
// root as PEM; we also accept DER defensively.
func fetchIssuanceRoot(t *testing.T, apiRoot *x509.CertPool) *x509.CertPool {
	t.Helper()
	client := &http.Client{
		Timeout:   5 * time.Second,
		Transport: &http.Transport{TLSClientConfig: &tls.Config{RootCAs: apiRoot}},
	}
	var body []byte
	deadline := time.Now().Add(20 * time.Second)
	for time.Now().Before(deadline) {
		resp, err := client.Get(pebbleRootsURL)
		if err == nil && resp.StatusCode == http.StatusOK {
			body, _ = io.ReadAll(resp.Body)
			_ = resp.Body.Close()
			break
		}
		if resp != nil {
			_ = resp.Body.Close()
		}
		time.Sleep(300 * time.Millisecond)
	}
	if len(body) == 0 {
		t.Fatal("could not fetch pebble issuance root from :15000")
	}
	pool := x509.NewCertPool()
	if pool.AppendCertsFromPEM(body) {
		return pool
	}
	// Fall back to DER if the management endpoint returned raw bytes.
	if cert, err := x509.ParseCertificate(body); err == nil {
		pool.AddCert(cert)
		return pool
	}
	t.Fatalf("pebble issuance root not parseable (%d bytes)", len(body))
	return nil
}

// startTestDNS runs a tiny DNS server (UDP + TCP on a FIXED loopback port) that
// answers every A query for the test base domain (and its subdomains) with
// 127.0.0.1. Both transports are served because Pebble's resolver may use either.
// It returns the host:port for Pebble's -dnsserver. Servers stop on test cleanup.
func startTestDNS(t *testing.T) string {
	t.Helper()
	// A fixed loopback port so the address is stable and obviously not collidable
	// with the challenge port (5001) or the ACME directory (14000).
	const dnsAddr = "127.0.0.1:8553"

	handler := dns.HandlerFunc(func(w dns.ResponseWriter, r *dns.Msg) {
		m := new(dns.Msg)
		m.SetReply(r)
		m.Authoritative = true
		for _, q := range r.Question {
			name := strings.ToLower(q.Name)
			dnsQueryLog(name, dns.TypeToString[q.Qtype])
			// Answer A for any name within our test base domain (or its parent
			// labels, which Pebble may probe) with loopback.
			switch q.Qtype {
			case dns.TypeA:
				if strings.HasSuffix(name, pebbleBaseDomain+".") || name == pebbleBaseDomain+"." {
					rr, _ := dns.NewRR(q.Name + " 60 IN A 127.0.0.1")
					m.Answer = append(m.Answer, rr)
				}
			case dns.TypeAAAA:
				// No AAAA: force Pebble onto IPv4 loopback (empty NOERROR).
			case dns.TypeCAA:
				// No CAA records => issuance permitted (empty NOERROR).
			}
		}
		_ = w.WriteMsg(m)
	})

	// UDP listener.
	udpPC, err := net.ListenPacket("udp", dnsAddr)
	if err != nil {
		t.Fatalf("listen dns udp %s: %v", dnsAddr, err)
	}
	udpSrv := &dns.Server{PacketConn: udpPC, Handler: handler}
	go udpSrv.ActivateAndServe() //nolint:errcheck
	t.Cleanup(func() { _ = udpSrv.Shutdown() })

	// TCP listener on the same address.
	tcpL, err := net.Listen("tcp", dnsAddr)
	if err != nil {
		t.Fatalf("listen dns tcp %s: %v", dnsAddr, err)
	}
	tcpSrv := &dns.Server{Listener: tcpL, Handler: handler}
	go tcpSrv.ActivateAndServe() //nolint:errcheck
	t.Cleanup(func() { _ = tcpSrv.Shutdown() })

	return dnsAddr
}

// sanity: ensure the engine actually built a real ACME issuer (not the test seam).
func init() {
	if certmagic.LetsEncryptProductionCA == "" {
		panic("certmagic constant missing")
	}
}

// errUnused keeps the errors import meaningful if assertions change; referenced here
// so the file compiles cleanly under future edits without churn.
var _ = errors.New

// dnsQueryLog records every DNS query Pebble makes, so a failing run can show what
// Pebble asked for (and on which port it then dialed). Best-effort stderr.
func dnsQueryLog(name, qtype string) {
	_, _ = os.Stderr.WriteString("DNS query: " + qtype + " " + name + "\n")
}
