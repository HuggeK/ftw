package redact

import (
	"encoding/hex"
	"encoding/json"
	"os"
	"reflect"
	"strings"
	"testing"
)

func testRedactor() *Redactor {
	return New([]byte("box key"), []string{
		"Easee carport", "Easee", "sungrow", "Garage meter", "Storgatan 12", "Volvo EX30", "pv",
	})
}

// Each line carries something that must not leave the box. The test asserts
// on what is gone, not on how the rest reads.
func TestALogLineLosesEverythingThatNamesOrReaches(t *testing.T) {
	r := testRedactor()
	cases := []struct {
		in    string
		never []string
	}{
		{"connected to Easee carport at 192.168.1.40:502", []string{"Easee", "carport", "192.168", "502"}},
		{"sungrow: modbus timeout from 10.0.0.7", []string{"sungrow", "10.0.0.7"}},
		{"inverter serial A2234567890 online", []string{"A2234567890", "2234567890"}},
		{"charger SN EHXYZ123 firmware 4.2", []string{"EHXYZ123"}},
		{"vehicle VIN YV1XZK7V5P2012345 linked", []string{"YV1XZK7V5P2012345"}},
		{"mac 00:1A:2B:3C:4D:5E seen", []string{"00:1A:2B:3C:4D:5E", "1A:2B"}},
		{"mac 001a2b3c4d5e seen", []string{"001a2b3c4d5e"}},
		{"ipv6 fe80::1ff:fe23:4567:890a reachable", []string{"fe80", "890a"}},
		{"GET https://api.easee.com/api/chargers/EH123456/state failed", []string{"api.easee.com", "EH123456", "https"}},
		{"resolving fritz.box and inverter.local", []string{"fritz.box", "inverter.local"}},
		{"owner fredrik@example.com notified", []string{"fredrik", "example.com"}},
		{"Authorization: Bearer eyJhbGciOiJIUzI1NiJ9.eyJzdWIiOiIxIn0.sig", []string{"eyJhbGci", "eyJzdWIi"}},
		{"login with password=hunter2 and token: abc", []string{"hunter2", "abc"}},
		{`HTTP 401: {"access_token":"tok_live_123","refresh_token":"r-456"}`, []string{"tok_live_123", "r-456"}},
		{"api key 9f86d081884c7d659a2feaa0c55ad015a3bf4f1b2b0b822cd15d6c15b0f00a08", []string{"9f86d081884c"}},
		{"site at Storgatan 12 sees Garage meter", []string{"Storgatan", "Garage meter"}},
		{"car Volvo EX30 at 54 %", []string{"Volvo", "EX30"}},
		{`device "Living room heat pump" answered`, []string{"Living room"}},
		{"phone +46701234567 registered", []string{"46701234567", "0701234567"}},
		{"Easee Easee twice", []string{"Easee"}},
	}
	for _, c := range cases {
		got := r.Text(c.in)
		for _, bad := range c.never {
			if strings.Contains(strings.ToLower(got), strings.ToLower(bad)) {
				t.Errorf("%q\n  became %q\n  and still holds %q", c.in, got, bad)
			}
		}
	}
}

// Scrubbing that leaves nothing useful helps nobody debug. These survive.
func TestALogLineKeepsWhatHelps(t *testing.T) {
	r := testRedactor()
	cases := map[string]string{
		"planner fell back to planner_passive_arbitrage":     "planner_passive_arbitrage",
		"control tick overran by 250 ms at 12:34:56":         "12:34:56",
		"dispatch blocked: site meter stale for 45 s":        "45 s",
		"loadpoint hold refused: E_CAP_EXCEEDED":             "E_CAP_EXCEEDED",
		"battery setpoint -3000 W, soc 54 %":                 "-3000 W",
		"pv forecast 4.2 kW":                                 "pv forecast",
		"op loadpoint.hold needs ftw.dispatch.hold":          "loadpoint.hold",
		"self_consumption_horizon_steps clipped to 96 slots": "self_consumption_horizon_steps",
	}
	for in, keep := range cases {
		if got := r.Text(in); !strings.Contains(got, keep) {
			t.Errorf("%q became %q and lost %q", in, got, keep)
		}
	}
}

func TestAttributesKeepOnlySafeValues(t *testing.T) {
	r := testRedactor()
	got := r.Attrs(`err=dial tcp 192.168.1.40:502: connection refused power_w=3000 took=250ms ok=true mode=self_consumption ` +
		`name=Easee carport serial=12345678 host=fritz.box user=fredrik id=lp1 note=Hello there ` +
		`lat=59.3293 phone=46701234567 total_wh=1234567 state=42 reason=timeout`)
	want := "err=[omitted] power_w=3000 took=250ms ok=true mode=self_consumption state=[omitted] reason=timeout"
	if got != want {
		t.Errorf("attrs\n got %q\nwant %q", got, want)
	}
	if got := r.Attrs("no pairs here"); got != "" {
		t.Errorf("a tail without pairs: %q", got)
	}
}

func TestADriversLineNamesItsSourceByID(t *testing.T) {
	r := testRedactor()
	out := r.Log(Line{TsMs: 5, Level: "WARN", Driver: "sungrow", Msg: "sungrow read failed", Attrs: "reg=13000 err=timeout"})
	if out.Src == "" || strings.Contains(out.Src, "sungrow") {
		t.Fatalf("src %q", out.Src)
	}
	if out.Src != r.ID("sungrow") || out.Src == New([]byte("other box"), nil).ID("sungrow") {
		t.Error("an id must be stable for one box and differ between boxes")
	}
	// A driver's own words never leave; its allowed attributes do.
	if out.Msg != "err=timeout" {
		t.Errorf("msg %q", out.Msg)
	}
}

func TestConfigLosesSecretsPlacesAndNames(t *testing.T) {
	r := testRedactor()
	raw := `{
	  "site": {"name": "Storgatan 12", "latitude": 59.3, "longitude": 18.0, "fuse": {"max_amps": 25}},
	  "drivers": [
	    {"name": "sungrow", "lua": "sungrow.lua", "capabilities": {"modbus": {"host": "192.168.1.40", "port": 502}},
	     "config": {"serial": "A2234567890", "api_key": "k-1"}},
	    {"name": "Easee carport", "config": {"username": "fredrik@example.com", "password": "hunter2", "site_id": "12345678"}}
	  ],
	  "notifications": {"ntfy": {"topic": "ftw-fredrik"}, "webhook_url": "https://hooks.example.com/x"},
	  "api": {"token": "secret-token", "lan_auth": true},
	  "planner": {"mode": "planner_arbitrage", "horizon_h": 48, "comment": "see https://ftw.energy for 10.0.0.7"},
	  "by_name": {"sungrow": {"weight": 1}}
	}`
	var doc any
	if err := json.Unmarshal([]byte(raw), &doc); err != nil {
		t.Fatal(err)
	}
	out, err := json.Marshal(r.Config(doc))
	if err != nil {
		t.Fatal(err)
	}
	got := string(out)
	for _, bad := range []string{
		"Storgatan", "59.3", "18.0", "sungrow\"", "192.168", "A2234567890", "k-1", "Easee", "fredrik",
		"hunter2", "12345678", "hooks.example", "secret-token", "ftw.energy", "10.0.0.7", "ftw-fredrik",
		`"port"`,
	} {
		if strings.Contains(got, bad) {
			t.Errorf("config still holds %q:\n%s", bad, got)
		}
	}
	for _, keep := range []string{`"max_amps":25`, `"mode":"planner_arbitrage"`, `"horizon_h":48`, `"lan_auth":true`, r.ID("sungrow")} {
		if !strings.Contains(got, keep) {
			t.Errorf("config lost %q:\n%s", keep, got)
		}
	}
}

// The vectors hold lines and a config in and out, so a change to the rules
// shows as a diff. Regenerate with FTW_WRITE_VECTORS=1.
const redactionVectorsPath = "../../protocol/testdata/redaction-vectors.json"

type redactionVectors struct {
	Note   string          `json:"note"`
	KeyHex string          `json:"keyHex"`
	Names  []string        `json:"names"`
	Lines  []redactionLine `json:"lines"`
	Config struct {
		In  json.RawMessage `json:"in"`
		Out json.RawMessage `json:"out"`
	} `json:"config"`
}

type redactionLine struct {
	In  Line `json:"in"`
	Out Out  `json:"out"`
}

func vectorLines() []Line {
	return []Line{
		{TsMs: 1, Level: "WARN", Msg: "connected to Easee carport at 192.168.1.40:502", Attrs: "power_w=3000 lat=59.33"},
		{TsMs: 2, Level: "ERROR", Driver: "sungrow", Msg: "serial A2234567890 at fritz.box said hi", Attrs: "err=timeout reg=13000"},
		{TsMs: 3, Level: "WARN", Msg: "token: abc123 for fredrik@example.com", Attrs: "mode=self_consumption took=250ms"},
		{TsMs: 4, Level: "INFO", Msg: "planner fell back to planner_passive_arbitrage at 12:34:56", Attrs: "slots=96 reason=timeout"},
		{TsMs: 5, Level: "WARN", Msg: "vehicle VIN YV1XZK7V5P2012345 linked to Volvo EX30", Attrs: "soc=0.54 phone=46701234567"},
		{TsMs: 6, Level: "WARN", Msg: "bridge at vw.fredrik.me, then my-home.xyz and solar.ai:8443 timed out in planner.prefs.set", Attrs: "op=planner.prefs.set"},
	}
}

const vectorConfig = `{"site":{"name":"Storgatan 12","latitude":59.3,"fuse":{"max_amps":25,"phases":3}},` +
	`"drivers":[{"name":"sungrow","capabilities":{"modbus":{"host":"192.168.1.40","port":502,"unit_id":1}},` +
	`"config":{"password":"hunter2","account":5512345678,"bridge":"vw.fredrik.me"}}],` +
	`"api":{"token":"t","lan_auth":true},"planner":{"mode":"planner_arbitrage","pv_forecast_safety_k":0.3,"slots":[96,48]},` +
	`"battery":{"battery_capacity_wh":10000,"min_reserve_soc_pct":20}}`

func TestRedactionVectors(t *testing.T) {
	key := []byte("redaction vector key")
	r := New(key, []string{"Easee carport", "sungrow", "Storgatan 12", "Volvo EX30"})
	var doc any
	if err := json.Unmarshal([]byte(vectorConfig), &doc); err != nil {
		t.Fatal(err)
	}
	cfgOut, err := json.Marshal(r.Config(doc))
	if err != nil {
		t.Fatal(err)
	}
	var got redactionVectors
	got.Note = "docs/agents.md, Redaction on the box. Lines and a config in and out under keyHex and names."
	got.KeyHex = hex.EncodeToString(key)
	got.Names = []string{"Easee carport", "sungrow", "Storgatan 12", "Volvo EX30"}
	for _, l := range vectorLines() {
		got.Lines = append(got.Lines, redactionLine{In: l, Out: r.Log(l)})
	}
	got.Config.In = json.RawMessage(vectorConfig)
	got.Config.Out = cfgOut

	if os.Getenv("FTW_WRITE_VECTORS") != "" {
		raw, err := json.MarshalIndent(got, "", "  ")
		if err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(redactionVectorsPath, append(raw, '\n'), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	raw, err := os.ReadFile(redactionVectorsPath)
	if err != nil {
		t.Fatal(err)
	}
	var want redactionVectors
	if err := json.Unmarshal(raw, &want); err != nil {
		t.Fatal(err)
	}
	for i := range want.Lines {
		if i >= len(got.Lines) || got.Lines[i].Out != want.Lines[i].Out {
			t.Errorf("line %d: got %+v, want %+v", i, got.Lines[i].Out, want.Lines[i].Out)
		}
	}
	var a, b any
	_ = json.Unmarshal(want.Config.Out, &a)
	_ = json.Unmarshal(cfgOut, &b)
	if !reflect.DeepEqual(a, b) {
		t.Errorf("config\n got %s\nwant %s", cfgOut, want.Config.Out)
	}
}
