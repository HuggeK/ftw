package appproto

import (
	"os"
	"path/filepath"
	"testing"

	"github.com/srcfl/ftw/go/internal/appproto/gencontract"
	"github.com/srcfl/ftw/go/internal/control"
)

const (
	registryPath = "../../../contract/registry.yaml"
	// The generator lives here; its output lives in package protocol, which
	// FTW Cloud imports too.
	generatedPath = "../../protocol/contract_gen.go"
)

// The generated constants must be what the registry currently says. A snapshot
// updated without rerunning the generator is exactly the drift the registry
// exists to prevent.
func TestContractGenIsCurrent(t *testing.T) {
	raw, err := os.ReadFile(registryPath)
	if err != nil {
		t.Fatalf("read registry: %v", err)
	}
	want, err := gencontract.Generate(raw)
	if err != nil {
		t.Fatalf("generate: %v", err)
	}
	got, err := os.ReadFile(generatedPath)
	if err != nil {
		t.Fatalf("read generated file: %v", err)
	}
	if string(got) != string(want) {
		t.Fatalf("protocol/contract_gen.go is stale; run: go generate ./internal/...\n"+
			"(registry: %s)", filepath.Clean(registryPath))
	}
}

// The mode keys in the registry are this repository's, copied there so the app
// can validate them. control.ModeCatalog() stays the authority; this catches
// the two drifting apart.
func TestRegistryModesMatchCatalog(t *testing.T) {
	cat := control.ModeCatalog()
	if len(cat) != len(RegistryModeTiers) {
		t.Fatalf("catalogue has %d modes, registry has %d", len(cat), len(RegistryModeTiers))
	}
	for _, m := range cat {
		tier, ok := RegistryModeTiers[string(m.Key)]
		if !ok {
			t.Fatalf("mode %q is in the catalogue but not in contract/registry.yaml", m.Key)
		}
		if tier != string(m.Tier) {
			t.Fatalf("mode %q: catalogue tier %q, registry tier %q", m.Key, m.Tier, tier)
		}
	}
}

// The ops in the registry are this repository's, copied there so neither side
// hand-writes an operation name or the scope it demands. defaultOps() in
// command.go stays the authority on what this box accepts; this catches the
// two drifting apart. Containment rather than equality, on purpose: the
// registry may name an op ahead of the box — battery.hold today — and a box
// that lacks it answers ErrUnknownOp, which command_test.go pins.
func TestRegistryOpsMatchCommandTable(t *testing.T) {
	for op, spec := range defaultOps() {
		scope, ok := RegistryOps[op]
		if !ok {
			t.Fatalf("op %q is accepted but not in contract/registry.yaml", op)
		}
		if scope != spec.scope {
			t.Fatalf("op %q: command table demands scope %q, registry says %q", op, spec.scope, scope)
		}
	}
}

// Everything the box advertises must be something it can answer. A capability
// the app cannot use is hidden; one it can ask for and not get is a hang.
func TestDefaultCapsAreAllRegistryNames(t *testing.T) {
	known := map[string]bool{}
	for _, c := range AllCapabilities {
		known[c] = true
	}
	for _, c := range DefaultCaps() {
		if !known[c] {
			t.Fatalf("%q is not a capability in contract/registry.yaml", c)
		}
	}
}
