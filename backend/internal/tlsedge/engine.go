package tlsedge

import (
	"context"
	"crypto/tls"
	"crypto/x509"
	"errors"
	"fmt"
	"log/slog"
	"net"
	"net/http"
	"sync"
	"time"

	"github.com/caddyserver/certmagic"
	"go.uber.org/zap"
)

const (
	// shutdownTimeout bounds graceful drain of the TLS + redirect servers so a
	// stuck connection cannot hang process exit. Mirrors the entry point's budget.
	shutdownTimeout = 15 * time.Second
	// readHeaderTimeout matches the plain-HTTP servers; it bounds slow-loris on the
	// :80 challenge/redirect listener (the :443 listener gets it too).
	readHeaderTimeout = 10 * time.Second
	// D1: the remaining connection-phase caps, mirroring cmd/kotojid. ReadHeaderTimeout
	// alone leaves body-read, response-write, and idle keep-alive unbounded, so a
	// slow-body / slow-read / idle-hoard client can pin the TLS edge. readTimeout bounds
	// the whole request read; writeTimeout is generous because the :443 combined handler
	// carries MCP Streamable-HTTP streaming yet still caps the slow-read drain; idle
	// keep-alives are bounded by idleTimeout.
	readTimeout  = 30 * time.Second
	writeTimeout = 120 * time.Second
	idleTimeout  = 120 * time.Second
)

// CA identifies which ACME directory on-demand issuance targets. The production
// builder maps config.TLSCA onto these; the test builder injects a Pebble.
type CA struct {
	// DirectoryURL is the ACME directory endpoint. Empty => Let's Encrypt
	// production (CertMagic's default), which the prod builder selects for "prod".
	DirectoryURL string
	// TrustedRoots, when non-nil, is the root pool used to trust the ACME server's
	// TLS (needed for a private CA like Pebble whose root is not in the system
	// store). Nil => use the system roots (correct for Let's Encrypt prod/staging).
	TrustedRoots *x509.CertPool
	// AltHTTPPort / AltTLSALPNPort override the ports the ACME challenge solvers
	// LISTEN on (and that the CA's validation authority connects back to). Zero =>
	// the standard 80 / 443 (correct for Let's Encrypt). A private test CA such as
	// Pebble validates on non-standard ports (default 5002 / 5001), so the
	// integration test sets these to point the real ACME challenge flow at the
	// engine's own listeners without needing privileged ports. Production leaves
	// them zero.
	AltHTTPPort    int
	AltTLSALPNPort int
	// DisableHTTPChallenge, when true, removes the ACME HTTP-01 solver so issuance
	// relies on TLS-ALPN-01 alone. Production leaves it false (both challenge types
	// are offered; the CA picks one). The integration test sets it because it only
	// stands up the TLS-ALPN-01 path against Pebble.
	DisableHTTPChallenge bool
}

// LetsEncryptCA returns the CA targeting Let's Encrypt: the STAGING directory
// (untrusted test certs, generous rate limits) when staging is true, else the
// PRODUCTION directory. Both use the system root pool (TrustedRoots nil) since
// Let's Encrypt is publicly trusted. The composition root maps KOTOJI_TLS_CA onto
// this so app.go never imports certmagic's CA constants.
func LetsEncryptCA(staging bool) CA {
	if staging {
		return CA{DirectoryURL: certmagic.LetsEncryptStagingCA}
	}
	// Empty DirectoryURL => CertMagic's default (Let's Encrypt production). We set
	// it explicitly for clarity and so logs show the resolved endpoint.
	return CA{DirectoryURL: certmagic.LetsEncryptProductionCA}
}

// Config bundles everything the engine needs. It is built by the composition root
// (production) or a test, never parsed from env here — keeping this package free
// of the config import and trivially testable.
type Config struct {
	// Handler is the SINGLE combined handler that fronts BOTH planes via Host
	// routing (the RUN_MODE=all combined router). REQUIRED.
	Handler http.Handler
	// Decider gates on-demand issuance (see decision.go). REQUIRED.
	Decider *Decider
	// StorageDir persists issued certs/keys + the ACME account so they survive
	// restarts (${KOTOJI_DATA_DIR}/certmagic). REQUIRED.
	StorageDir string
	// CA selects the ACME directory + trust pool. The zero value targets Let's
	// Encrypt production with the system roots.
	CA CA
	// Email is the OPTIONAL ACME account email (may be empty).
	Email string
	// TLSAddr / HTTPAddr are the listen addresses (default :443 / :80 applied by
	// the composition root from config).
	TLSAddr  string
	HTTPAddr string
	// Logger is the slog logger for engine lifecycle + a zap bridge for CertMagic.
	Logger *slog.Logger
	// Issuers, when non-empty, REPLACES the default ACME issuer. This is the test
	// seam: an integration test injects a local self-signed issuer (or a pebble-
	// backed ACMEIssuer) so the on-demand path can be exercised WITHOUT reaching
	// real Let's Encrypt. Production leaves it nil (the ACME issuer is built from
	// CA + Email). The :80 ACME HTTP-01 handler is wired only for the default ACME
	// issuer; an injected issuer set is assumed to self-solve (e.g. self-signed).
	Issuers []certmagic.Issuer
}

// Engine owns the CertMagic config + cache and the :443/:80 servers. It is started
// once and stopped once (idempotent); concurrency is bounded by a sync.Once + a
// WaitGroup mirroring the entry point's server lifecycle.
type Engine struct {
	cfg     Config
	magic   *certmagic.Config
	cache   *certmagic.Cache
	issuer  *certmagic.ACMEIssuer
	tlsSrv  *http.Server
	httpSrv *http.Server

	logger *slog.Logger

	stopOnce sync.Once
}

// New builds the CertMagic on-demand engine. It validates required deps, wires the
// DecisionFunc, points storage at StorageDir, selects the CA, and prepares the
// :443 (TLS) + :80 (ACME HTTP-01 + redirect) servers. It does NOT bind sockets —
// call Run for that.
func New(cfg Config) (*Engine, error) {
	if cfg.Handler == nil {
		return nil, errors.New("tlsedge: Config.Handler is required")
	}
	if cfg.Decider == nil {
		return nil, errors.New("tlsedge: Config.Decider is required")
	}
	if cfg.StorageDir == "" {
		return nil, errors.New("tlsedge: Config.StorageDir is required")
	}
	logger := cfg.Logger
	if logger == nil {
		logger = slog.Default()
	}
	tlsAddr := cfg.TLSAddr
	if tlsAddr == "" {
		tlsAddr = ":443"
	}
	httpAddr := cfg.HTTPAddr
	if httpAddr == "" {
		httpAddr = ":80"
	}

	// A dedicated cache + config (NOT the package-global Default) so the engine is
	// fully self-contained and a second instance (e.g. in tests) cannot clobber it.
	// GetConfigForCert returns our one config for every managed cert.
	var magic *certmagic.Config
	cache := certmagic.NewCache(certmagic.CacheOptions{
		GetConfigForCert: func(certmagic.Certificate) (*certmagic.Config, error) {
			return magic, nil
		},
	})

	template := certmagic.Config{
		// Persist under the existing data volume so certs survive restarts.
		Storage: &certmagic.FileStorage{Path: cfg.StorageDir},
		// On-demand: defer issuance to the handshake, gated by our DecisionFunc.
		OnDemand: &certmagic.OnDemandConfig{
			DecisionFunc: cfg.Decider.Allow,
		},
		Logger: zapBridge(logger),
	}
	magic = certmagic.New(cache, template)

	// Issuer wiring. Default: a single ACME issuer (CA.DirectoryURL empty => Let's
	// Encrypt production, the CertMagic default; staging/Pebble override it, and a
	// non-nil TrustedRoots trusts a private CA over the network). The :80 handler
	// then solves ACME HTTP-01 + redirects. TEST seam: when Config.Issuers is set we
	// use it verbatim (e.g. a self-signed issuer) and the :80 handler is redirect-only.
	var acmeIssuer *certmagic.ACMEIssuer
	httpHandler := redirectHandler()
	if len(cfg.Issuers) > 0 {
		magic.Issuers = cfg.Issuers
	} else {
		acmeIssuer = certmagic.NewACMEIssuer(magic, certmagic.ACMEIssuer{
			CA:           cfg.CA.DirectoryURL,
			Email:        cfg.Email,
			Agreed:       true, // self-host operator accepts the CA terms by enabling auto-TLS.
			TrustedRoots: cfg.CA.TrustedRoots,
			// Non-standard challenge ports for a private/test CA (Pebble). Zero in
			// production => CertMagic's defaults (80 / 443).
			AltHTTPPort:          cfg.CA.AltHTTPPort,
			AltTLSALPNPort:       cfg.CA.AltTLSALPNPort,
			DisableHTTPChallenge: cfg.CA.DisableHTTPChallenge,
			Logger:               zapBridge(logger),
		})
		magic.Issuers = []certmagic.Issuer{acmeIssuer}
		// Wrap the redirect with the ACME HTTP-01 solver so challenge paths short-
		// circuit and everything else 301s to https.
		httpHandler = acmeIssuer.HTTPChallengeHandler(httpHandler)
	}

	return &Engine{
		cfg:    cfg,
		magic:  magic,
		cache:  cache,
		issuer: acmeIssuer,
		logger: logger,
		tlsSrv: &http.Server{
			Addr: tlsAddr,
			// HS1: in auto-TLS mode kotoji terminates TLS itself, so it OWNS the HSTS
			// header. Wrap the combined handler so every HTTPS response on :443 carries
			// Strict-Transport-Security (off/dev mode never reaches here, so a proxy
			// deployment is unaffected and keeps owning its own HSTS).
			Handler: hstsHandler(cfg.Handler),
			// D1: bound every connection phase (not just headers).
			ReadHeaderTimeout: readHeaderTimeout,
			ReadTimeout:       readTimeout,
			WriteTimeout:      writeTimeout,
			IdleTimeout:       idleTimeout,
		},
		httpSrv: &http.Server{
			Addr:    httpAddr,
			Handler: httpHandler,
			// D1: bound every connection phase on the :80 challenge/redirect listener.
			ReadHeaderTimeout: readHeaderTimeout,
			ReadTimeout:       readTimeout,
			WriteTimeout:      writeTimeout,
			IdleTimeout:       idleTimeout,
		},
	}, nil
}

// TLSConfig returns the *tls.Config CertMagic recommends for the :443 listener
// (GetCertificate wired to the on-demand cache + ALPN for TLS-ALPN-01). Exposed so
// a test can stand up its own TLS listener against the same engine.
func (e *Engine) TLSConfig() *tls.Config { return e.magic.TLSConfig() }

// Run binds both listeners and blocks until ctx is cancelled or a listener fails,
// then drains gracefully. It is the engine analogue of cmd/kotojid's serve(): the
// caller runs it in a goroutine alongside the plain-HTTP servers (if any). The
// returned error is the first non-graceful listener error, or nil on clean stop.
func (e *Engine) Run(ctx context.Context) error {
	// errCh carries the first non-graceful listener error from either server.
	errCh := make(chan error, 2)
	var wg sync.WaitGroup

	// :443 — the combined handler over CertMagic's on-demand TLS.
	tlsListener, err := tls.Listen("tcp", e.tlsSrv.Addr, e.magic.TLSConfig())
	if err != nil {
		return fmt.Errorf("tlsedge: bind %s: %w", e.tlsSrv.Addr, err)
	}
	wg.Add(1)
	go func() {
		defer wg.Done()
		e.logger.Info("tls listening", slog.String("addr", e.tlsSrv.Addr))
		if serr := e.tlsSrv.Serve(tlsListener); serr != nil && !errors.Is(serr, http.ErrServerClosed) {
			errCh <- serr
		}
	}()

	// :80 — ACME HTTP-01 + HTTPS redirect.
	httpListener, err := net.Listen("tcp", e.httpSrv.Addr)
	if err != nil {
		// Roll back the already-bound TLS listener before returning.
		_ = tlsListener.Close()
		wg.Wait()
		return fmt.Errorf("tlsedge: bind %s: %w", e.httpSrv.Addr, err)
	}
	wg.Add(1)
	go func() {
		defer wg.Done()
		e.logger.Info("acme http listening", slog.String("addr", e.httpSrv.Addr))
		if serr := e.httpSrv.Serve(httpListener); serr != nil && !errors.Is(serr, http.ErrServerClosed) {
			errCh <- serr
		}
	}()

	var runErr error
	select {
	case <-ctx.Done():
		e.logger.Info("tlsedge shutdown signal received")
	case runErr = <-errCh:
		e.logger.Error("tlsedge listener error, shutting down", slog.Any("error", runErr))
	}

	e.shutdown(&runErr)
	wg.Wait()
	return runErr
}

// shutdown gracefully drains both servers and stops the cert cache. It is invoked
// from Run on stop and is safe to call once. The first drain error is recorded in
// *runErr only when no listener error already won.
func (e *Engine) shutdown(runErr *error) {
	e.stopOnce.Do(func() {
		shutdownCtx, cancel := context.WithTimeout(context.Background(), shutdownTimeout)
		defer cancel()
		for _, srv := range []*http.Server{e.tlsSrv, e.httpSrv} {
			if serr := srv.Shutdown(shutdownCtx); serr != nil {
				e.logger.Error("tlsedge graceful shutdown failed", slog.String("addr", srv.Addr), slog.Any("error", serr))
				if runErr != nil && *runErr == nil {
					*runErr = serr
				}
			}
		}
		// Stop the cache's maintenance goroutine + release storage locks.
		e.cache.Stop()
	})
}

// hstsValue is the Strict-Transport-Security header emitted on the :443 listener in
// auto-TLS mode (HS1). One-year max-age with subdomains (every project host is a
// subdomain kotoji serves over HTTPS in this mode) but NO preload by default — preload
// is an irreversible commitment a self-hoster must opt into deliberately, not a default.
const hstsValue = "max-age=31536000; includeSubDomains"

// hstsHandler wraps next so every response served over the auto-TLS :443 listener
// carries Strict-Transport-Security (HS1). It is applied ONLY here, so it is emitted
// solely when kotoji terminates TLS itself (KOTOJI_TLS_MODE=auto). In off/dev mode the
// engine is never constructed, so plain-HTTP and proxy-fronted deployments never see
// it — the fronting proxy keeps sole ownership of HSTS there. The header is set before
// next runs so it survives even when a downstream handler writes its own headers.
func hstsHandler(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Strict-Transport-Security", hstsValue)
		next.ServeHTTP(w, r)
	})
}

// redirectHandler 301-redirects every (non-challenge) request to its https
// equivalent on the SAME host + path + query. The issuer's HTTPChallengeHandler
// wraps this, so ACME HTTP-01 challenge paths never reach it.
func redirectHandler() http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		host := r.Host
		// Strip a :80 (or any) port; the redirect target is the default https port.
		if h, _, err := net.SplitHostPort(host); err == nil {
			host = h
		}
		target := "https://" + host + r.URL.RequestURI()
		// 301 permanent: browsers cache the upgrade; safe because auto-mode means
		// the operator intends HTTPS-only for these hosts.
		http.Redirect(w, r, target, http.StatusMovedPermanently)
	})
}

// zapBridge returns a *zap.Logger that forwards into the app's slog logger so all
// CertMagic logging lands in the same structured stream. A nil slog logger yields
// a no-op zap logger (CertMagic requires a non-nil logger).
func zapBridge(logger *slog.Logger) *zap.Logger {
	if logger == nil {
		return zap.NewNop()
	}
	return zap.New(newSlogZapCore(logger))
}
