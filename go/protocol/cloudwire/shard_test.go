package cloudwire

import (
	"encoding/json"
	"os"
	"testing"
)

// testdata/cloud-shard-vectors.json is what every client must decide about a
// wrong-shard host and a restart's spread. A phone that follows Ftw-Shard on
// /v1/now carries its bearer token there, so the clients must agree to the
// byte.
const shardVectorsPath = "testdata/cloud-shard-vectors.json"

type shardVectors struct {
	ShardHost []struct {
		Name, Base, Host string
		OK               bool `json:"ok"`
	} `json:"shardHost"`
	RestartSpread []struct {
		Name, Reason string
		MS           int64 `json:"ms"`
	} `json:"restartSpread"`
}

func loadShardVectors(t *testing.T) shardVectors {
	t.Helper()
	raw, err := os.ReadFile(shardVectorsPath)
	if err != nil {
		t.Fatal(err)
	}
	var v shardVectors
	if err := json.Unmarshal(raw, &v); err != nil {
		t.Fatal(err)
	}
	if len(v.ShardHost) == 0 || len(v.RestartSpread) == 0 {
		t.Fatal("empty vectors")
	}
	return v
}

func TestShardHostVectors(t *testing.T) {
	for _, c := range loadShardVectors(t).ShardHost {
		if got := ShardHostOK(c.Base, c.Host); got != c.OK {
			t.Errorf("%s: ShardHostOK(%q, %q) = %v, want %v", c.Name, c.Base, c.Host, got, c.OK)
		}
	}
}

func TestRestartSpreadVectors(t *testing.T) {
	for _, c := range loadShardVectors(t).RestartSpread {
		if got := RestartSpreadMs(c.Reason); got != c.MS {
			t.Errorf("%s: RestartSpreadMs(%q) = %d, want %d", c.Name, c.Reason, got, c.MS)
		}
	}
}
