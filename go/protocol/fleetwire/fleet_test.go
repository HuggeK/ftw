package fleetwire

import (
	"encoding/json"
	"fmt"
	"strings"
	"testing"
)

const valid = `{"schema":"ftw.fleet/1","ftw_version":"v0.146.0-beta.1","channel":"beta","drivers":["pixii","pixii_pv"],"battery_kwh":"5-15","price_zone":"SE4","install_age":"1-6m"}`

func TestOnlySharedLabelsAndBucketsAreAccepted(t *testing.T) {
	if _, err := Decode([]byte(valid)); err != nil {
		t.Fatal(err)
	}
	for _, bad := range []string{
		strings.Replace(valid, `"beta"`, `"stable"`, 1),
		strings.Replace(valid, `"pixii","pixii_pv"`, `"pixii","pixii"`, 1),
		strings.Replace(valid, `"pixii","pixii_pv"`, `"pixii_pv","pixii"`, 1),
		strings.Replace(valid, `["pixii","pixii_pv"]`, `null`, 1),
		strings.Replace(valid, `"SE4"`, `"myhouse"`, 1),
		strings.Replace(valid, `"5-15"`, `"12.456"`, 1),
		strings.Replace(valid, `"v0.146.0-beta.1"`, `"abcdef-dirty"`, 1),
		strings.TrimSuffix(valid, "}") + `,"home":"secret"}`,
		valid + valid, `[]`, `null`, strings.Repeat(" ", MaxBodyBytes+1) + valid,
	} {
		if _, err := Decode([]byte(bad)); err == nil {
			t.Fatalf("accepted %s", bad)
		}
	}
}

func TestCountersHaveFixedLimitsAndNoReportSurvives(t *testing.T) {
	d := NewDay("2026-10-17")
	for i := 0; i < MaxReports; i++ {
		if !d.Add(Report{Version: fmt.Sprint(i), Drivers: []string{fmt.Sprint(i)}, Channel: "beta", Battery: "5-15", Zone: "SE4", Age: "1-6m"}) {
			t.Fatal(i)
		}
	}
	if d.Add(Report{}) || len(d.Versions) > MaxLabels || len(d.Drivers) > MaxLabels || d.Reports != MaxReports {
		t.Fatal(d.Reports, len(d.Versions))
	}
	data, _ := json.Marshal(d)
	if strings.Contains(string(data), `"home"`) {
		t.Fatal("identifier persisted")
	}
}
