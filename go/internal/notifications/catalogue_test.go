package notifications

import (
	"os"
	"testing"

	"github.com/srcfl/ftw/go/internal/appproto/gencontract"
)

// The sentence table must be what the catalogue currently says. A snapshot
// updated without rerunning the generator is exactly the drift the pairing
// exists to prevent — these are the only words the box may put on a lock
// screen, and they are the app's words.
func TestPushSentencesAreCurrent(t *testing.T) {
	raw, err := os.ReadFile("../../../contract/push-catalogue.yaml")
	if err != nil {
		t.Fatalf("read push catalogue: %v", err)
	}
	want, err := gencontract.GeneratePush(raw)
	if err != nil {
		t.Fatalf("generate: %v", err)
	}
	got, err := os.ReadFile("../../protocol/push_gen.go")
	if err != nil {
		t.Fatalf("read generated file: %v", err)
	}
	if string(got) != string(want) {
		t.Fatal("protocol/push_gen.go is stale; run: go generate ./internal/...")
	}
}
