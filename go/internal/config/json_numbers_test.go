package config

import (
	"encoding/json"
	"strings"
	"testing"
)

func TestDriverJSONKeepsIntegerIdentifiersAndOrdinaryNumbers(t *testing.T) {
	raw := []byte(`{"drivers":[{"name":"meter","config":{"vendor_id":9007199254740993,"unsigned_id":18446744073709551615,"nested":[{"port":502,"ratio":0.1,"scale":1e3}],"string_id":"9007199254740993"}}]}`)
	var cfg Config
	if err := DecodeStrictJSON(raw, &cfg); err != nil {
		t.Fatal(err)
	}
	c := cfg.Drivers[0].Config
	if c["vendor_id"] != int64(9007199254740993) || c["unsigned_id"] != uint64(18446744073709551615) {
		t.Fatal(c)
	}
	row := c["nested"].([]any)[0].(map[string]any)
	if row["port"] != int64(502) || row["ratio"] != float64(0.1) || row["scale"] != float64(1000) {
		t.Fatal(row)
	}
	encoded, err := json.Marshal(cfg)
	if err != nil || !strings.Contains(string(encoded), `"vendor_id":9007199254740993`) || !strings.Contains(string(encoded), `"unsigned_id":18446744073709551615`) {
		t.Fatal(string(encoded), err)
	}
}

func TestDriverJSONRefusesSilentNumericLossWithItsFieldPath(t *testing.T) {
	for _, number := range []string{"0.123456789012345678901", "18446744073709551617", "1e-999999999", "1e999999999"} {
		var cfg Config
		err := DecodeStrictJSON([]byte(`{"drivers":[{"config":{"nested":[{"vendor/id":`+number+`}]}}]}`), &cfg)
		if err == nil || !strings.Contains(err.Error(), "/drivers/0/config/nested/0/vendor~1id") {
			t.Fatal("silent numeric loss or missing path", number, err)
		}
	}
	var cfg Config
	if err := DecodeStrictJSON([]byte(`{"drivers":[{"config":{"zero":0e-999999999}}]}`), &cfg); err != nil {
		t.Fatal("literal zero rejected", err)
	}
}
