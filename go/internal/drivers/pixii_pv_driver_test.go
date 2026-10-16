package drivers

import (
	"context"
	"testing"

	"github.com/srcfl/ftw/go/internal/telemetry"
)

func TestPixiiPVSunsetReadingsStayFresh(t *testing.T) {
	const name = "pixii-pv-test"
	const topic = "pixii/status/test-pixii/meter_ext6"
	tel := telemetry.NewStore()
	mqtt := &fakeMQTT{}
	d := newBundledMQTTDriver(t, "pixii_pv.lua", name, tel, mqtt, nil)

	steps := []struct {
		name       string
		topic      string
		payload    string
		wantW      float64
		wantUpdate bool
	}{
		{"missing before first reading", topic, `{"meter_source":"PV","meter_w1":0,"meter_w2":0}`, 0, false},
		{"day", topic, `{"meter_source":"PV","meter_w1":1000,"meter_w2":2000,"meter_w3":3000}`, -6000, true},
		{"sunset", topic, `{"meter_source":"PV","meter_w1":-1,"meter_w2":-2,"meter_w3":-3}`, 0, true},
		{"night", topic, `{"meter_source":"PV","meter_w1":-1,"meter_w2":-2,"meter_w3":-3}`, 0, true},
		{"mixed phases", topic, `{"meter_source":"PV","meter_w1":50,"meter_w2":-3,"meter_w3":-2}`, -45, true},
		{"missing phase", topic, `{"meter_source":"PV","meter_w1":0,"meter_w2":0}`, -45, false},
		{"invalid phase", topic, `{"meter_source":"PV","meter_w1":"bad","meter_w2":0,"meter_w3":0}`, -45, false},
		{"idle", "", "", -45, false},
		{"grid only", "pixii/status/test-pixii/meter", `{"meter_w":-750}`, -45, false},
		{"morning", topic, `{"meter_source":"PV","meter_w1":500,"meter_w2":500,"meter_w3":500}`, -1500, true},
	}
	for _, step := range steps {
		before := tel.Get(name, telemetry.DerPV)
		if step.topic != "" {
			mqtt.Push(step.topic, step.payload)
		}
		if _, err := d.Poll(context.Background()); err != nil {
			t.Fatalf("%s: poll: %v", step.name, err)
		}
		after := tel.Get(name, telemetry.DerPV)
		if before == nil && !step.wantUpdate {
			if after != nil {
				t.Fatalf("%s: emitted PV without complete power: %+v", step.name, after)
			}
			continue
		}
		if after == nil || after.RawW != step.wantW {
			t.Fatalf("%s: PV = %+v, want %.0f W", step.name, after, step.wantW)
		}
		if step.wantUpdate {
			if before != nil && !after.UpdatedAt.After(before.UpdatedAt) {
				t.Fatalf("%s: fresh MQTT did not refresh PV", step.name)
			}
		} else if !after.UpdatedAt.Equal(before.UpdatedAt) {
			t.Fatalf("%s: PV refreshed without a valid PV reading", step.name)
		}
	}
	if meter := tel.Get(name, telemetry.DerMeter); meter == nil || meter.RawW != -750 {
		t.Fatalf("grid export = %+v, want -750 W", meter)
	}
}
