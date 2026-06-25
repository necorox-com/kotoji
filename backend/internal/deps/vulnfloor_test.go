// Package deps holds supply-chain regression guards. This test enforces the
// minimum patched versions established by the security audit so the previously
// govulncheck-REACHABLE vulnerabilities (10 Go stdlib + golang.org/x/net idna)
// cannot silently regress via an accidental downgrade in go.mod.
//
// Fixes guarded here:
//   - Go toolchain floor go1.25.11 — resolves the 10 reachable stdlib vulns
//     (GO-2026-5039 net/textproto, GO-2026-5037/4947/4946 crypto/x509,
//     GO-2026-4971/4918 net & net/http, GO-2026-4870 crypto/tls,
//     GO-2026-4869 archive/tar, GO-2026-4602 os, GO-2026-4601 net/url).
//   - golang.org/x/net floor v0.55.0 — resolves GO-2026-5026 (idna).
package deps

import (
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"testing"
)

// minGoVersion is the lowest Go language/toolchain version that ships the
// patched standard library. Anything below this re-opens the 10 stdlib vulns.
const minGoVersion = "1.25.11"

// minXNetVersion is the lowest golang.org/x/net that contains the idna fix
// (GO-2026-5026). The audit confirmed v0.55.0 as the fixed release.
const minXNetVersion = "v0.55.0"

// findGoMod walks up from the test's working directory to locate the backend
// go.mod. The test runs from internal/deps, so the module root is two levels up,
// but walking keeps the guard robust if the package is ever relocated.
func findGoMod(t *testing.T) string {
	t.Helper()
	dir, err := os.Getwd()
	if err != nil {
		t.Fatalf("getwd: %v", err)
	}
	for {
		candidate := filepath.Join(dir, "go.mod")
		if _, statErr := os.Stat(candidate); statErr == nil {
			return candidate
		}
		parent := filepath.Dir(dir)
		if parent == dir {
			t.Fatalf("go.mod not found walking up from working directory")
		}
		dir = parent
	}
}

// parseSemverTriplet turns a version like "1.25.11" or "v0.56.0" into a
// comparable [3]int. Pre-release/build suffixes are ignored for the floor check.
func parseSemverTriplet(t *testing.T, v string) [3]int {
	t.Helper()
	v = strings.TrimPrefix(v, "v")
	// Drop any pre-release/build metadata so e.g. "1.25.11-rc1" still parses.
	if idx := strings.IndexAny(v, "-+"); idx >= 0 {
		v = v[:idx]
	}
	parts := strings.Split(v, ".")
	var out [3]int
	for i := 0; i < 3 && i < len(parts); i++ {
		n, err := strconv.Atoi(parts[i])
		if err != nil {
			t.Fatalf("unparseable version %q: %v", v, err)
		}
		out[i] = n
	}
	return out
}

// atLeast reports whether got >= want using the parsed triplet ordering.
func atLeast(got, want [3]int) bool {
	for i := 0; i < 3; i++ {
		if got[i] != want[i] {
			return got[i] > want[i]
		}
	}
	return true
}

// TestGoToolchainFloor asserts the go directive in go.mod is at least the
// patched toolchain. The go directive doubles as the minimum toolchain a build
// will accept, so this is the regression gate for the 10 stdlib vulns.
func TestGoToolchainFloor(t *testing.T) {
	data, err := os.ReadFile(findGoMod(t))
	if err != nil {
		t.Fatalf("read go.mod: %v", err)
	}
	var goLine string
	for _, line := range strings.Split(string(data), "\n") {
		trimmed := strings.TrimSpace(line)
		if strings.HasPrefix(trimmed, "go ") {
			goLine = strings.TrimSpace(strings.TrimPrefix(trimmed, "go "))
			break
		}
	}
	if goLine == "" {
		t.Fatal("no `go` directive found in go.mod")
	}
	if !atLeast(parseSemverTriplet(t, goLine), parseSemverTriplet(t, minGoVersion)) {
		t.Fatalf("go directive %q is below patched floor %q; this re-opens the audited stdlib vulns (GO-2026-5039 et al.)", goLine, minGoVersion)
	}
}

// TestXNetFloor asserts the resolved golang.org/x/net version pinned in go.mod
// is at or above the idna fix. go.mod is the source of truth that governs the
// build for every package in the module (the tlsedge sink links idna), so it is
// a more deterministic gate than this test binary's own linked Deps.
func TestXNetFloor(t *testing.T) {
	data, err := os.ReadFile(findGoMod(t))
	if err != nil {
		t.Fatalf("read go.mod: %v", err)
	}
	const target = "golang.org/x/net"
	var found string
	for _, line := range strings.Split(string(data), "\n") {
		fields := strings.Fields(line)
		// A require line looks like: `golang.org/x/net v0.56.0 // indirect`.
		if len(fields) >= 2 && fields[0] == target {
			found = fields[1]
			break
		}
	}
	if found == "" {
		t.Fatalf("%s not pinned in go.mod; cannot verify the idna fix floor", target)
	}
	if !atLeast(parseSemverTriplet(t, found), parseSemverTriplet(t, minXNetVersion)) {
		t.Fatalf("%s %s is below patched floor %s; this re-opens GO-2026-5026 (idna)", target, found, minXNetVersion)
	}
}
