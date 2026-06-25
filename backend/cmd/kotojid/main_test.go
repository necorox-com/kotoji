package main

import (
	"net/http"
	"testing"
	"time"
)

// TestNewControlServer_Timeouts pins D1: the control-plane server bounds every
// connection phase (header read, full read, write, idle), not just header read.
// ReadHeaderTimeout alone leaves slow-body / slow-read / idle-hoard attacks open.
func TestNewControlServer_Timeouts(t *testing.T) {
	srv := newControlServer(":8080", http.NotFoundHandler())

	if srv.ReadHeaderTimeout != readHeaderTimeout {
		t.Errorf("ReadHeaderTimeout = %v, want %v", srv.ReadHeaderTimeout, readHeaderTimeout)
	}
	if srv.ReadTimeout != readTimeout {
		t.Errorf("ReadTimeout = %v, want %v", srv.ReadTimeout, readTimeout)
	}
	// The control plane is non-streaming JSON => tighter write ceiling.
	if srv.WriteTimeout != controlWriteTimeout {
		t.Errorf("WriteTimeout = %v, want %v", srv.WriteTimeout, controlWriteTimeout)
	}
	if srv.IdleTimeout != idleTimeout {
		t.Errorf("IdleTimeout = %v, want %v", srv.IdleTimeout, idleTimeout)
	}
	// None may be zero (zero == unbounded == the vulnerability D1 fixes).
	assertNoUnboundedPhase(t, srv)
}

// TestNewServeServer_Timeouts pins D1 for the data plane: same bounded phases, but
// the generous writeTimeout so legitimate MCP streaming is not truncated.
func TestNewServeServer_Timeouts(t *testing.T) {
	srv := newServeServer(":8081", http.NotFoundHandler())

	if srv.ReadHeaderTimeout != readHeaderTimeout {
		t.Errorf("ReadHeaderTimeout = %v, want %v", srv.ReadHeaderTimeout, readHeaderTimeout)
	}
	if srv.ReadTimeout != readTimeout {
		t.Errorf("ReadTimeout = %v, want %v", srv.ReadTimeout, readTimeout)
	}
	// The data plane may stream => the generous write window.
	if srv.WriteTimeout != writeTimeout {
		t.Errorf("WriteTimeout = %v, want %v", srv.WriteTimeout, writeTimeout)
	}
	if srv.IdleTimeout != idleTimeout {
		t.Errorf("IdleTimeout = %v, want %v", srv.IdleTimeout, idleTimeout)
	}
	assertNoUnboundedPhase(t, srv)
}

// assertNoUnboundedPhase fails if any connection-phase timeout is left at zero, which
// http.Server treats as "no limit" — the exact resource-exhaustion gap D1 closes.
func assertNoUnboundedPhase(t *testing.T, srv *http.Server) {
	t.Helper()
	phases := map[string]time.Duration{
		"ReadHeaderTimeout": srv.ReadHeaderTimeout,
		"ReadTimeout":       srv.ReadTimeout,
		"WriteTimeout":      srv.WriteTimeout,
		"IdleTimeout":       srv.IdleTimeout,
	}
	for name, d := range phases {
		if d <= 0 {
			t.Errorf("%s is unbounded (%v); every phase must be capped", name, d)
		}
	}
}
