package main

import (
	"context"
	"math"
	"path/filepath"
	"testing"
	"time"

	"github.com/srcfl/ftw/go/internal/config"
	"github.com/srcfl/ftw/go/internal/state"
	"github.com/srcfl/ftw/go/internal/telemetry"
)

// Göran's box, 19 October 2026: SMA was silent for 145 s at 02:22 local time,
// 0 W on both sides, and the day's Home use became unknown. In Västerås that
// day the sun sets at 17:38 and civil dusk is at about 18:20.
func TestDarkPVByTimeOfDay(t *testing.T) {
	stockholm := time.FixedZone("CEST", 2*3600)
	sun := historySun{Lat: 59.6, Lon: 16.5, Placed: true}
	night := time.Date(2026, 10, 19, 2, 22, 0, 0, stockholm)
	for _, c := range []struct {
		name    string
		at      time.Time
		sun     historySun
		missing state.HistoryMissing
		applied bool
		want    state.HistoryMissing
	}{
		{name: "night", at: night, sun: sun, missing: state.MissingPV, applied: true},
		{name: "after dusk", at: time.Date(2026, 10, 19, 18, 45, 0, 0, stockholm), sun: sun, missing: state.MissingPV, applied: true},
		{name: "twilight", at: time.Date(2026, 10, 19, 17, 45, 0, 0, stockholm), sun: sun, missing: state.MissingPV, applied: true, want: state.EstimatedLoad},
		{name: "noon", at: time.Date(2026, 10, 19, 12, 0, 0, 0, stockholm), sun: sun, missing: state.MissingPV, want: state.MissingPV},
		{name: "before sunset", at: time.Date(2026, 10, 19, 17, 30, 0, 0, stockholm), sun: sun, missing: state.MissingPV, want: state.MissingPV},
		{name: "no pin", at: night, missing: state.MissingPV, want: state.MissingPV},
		// Other flows, and a configuration fault, keep their own evidence.
		{name: "battery also unknown", at: night, sun: sun, missing: state.MissingPV | state.MissingBattery, applied: true, want: state.MissingBattery},
		{name: "PV array without a measurement source", at: night, sun: sun, missing: state.MissingPV | state.MissingLoad, applied: true, want: state.MissingLoad},
	} {
		p := &state.HistorySources{AtMS: c.at.UnixMilli(), PVW: -300, Missing: c.missing}
		applied := darkPV(p, c.sun)
		if applied != c.applied || p.Missing != c.want || applied && p.PVW != 0 {
			t.Errorf("%s: applied=%v missing=%b PV=%v W, want applied=%v missing=%b", c.name, applied, p.Missing, p.PVW, c.applied, c.want)
		}
	}
	for _, c := range []struct {
		lat, lon float64
		ok       bool
	}{{0, 0, false}, {unsetPinLat, unsetPinLon, false}, {59.6, 16.5, true}} {
		if got := historySunOf(siteLocation(&config.Config{Weather: &config.Weather{Latitude: c.lat, Longitude: c.lon}})).Placed; got != c.ok {
			t.Errorf("pin %v,%v placed=%v, want %v", c.lat, c.lon, got, c.ok)
		}
	}
}

// A full night on the equator: 20:00–04:00 UTC at 15° W is past civil dusk in
// every season. The inverter reports 0 W for ten minutes, then sleeps, and a
// minute later the watchdog takes it offline. With a pin every snapshot is
// fully known and of one topology, so the writer records measured minutes that
// train the load models. Without one, the writer holds PV's last value as an
// estimate, and the topology changes when the device drops out. Home use is
// known in both.
func TestSleepingInverterNightIsMeasured(t *testing.T) {
	const tick, hour = int64(5000), int64(3600000)
	for _, c := range []struct {
		name     string
		sun      historySun
		measured bool
	}{
		{"with a pin", historySun{Lat: 1, Lon: -15, Placed: true}, true},
		{"without a pin", historySun{}, false},
	} {
		t.Run(c.name, func(t *testing.T) {
			st, err := state.Open(filepath.Join(t.TempDir(), "state.db"))
			if err != nil {
				t.Fatal(err)
			}
			t.Cleanup(func() { _ = st.Close() })
			if err := st.EnableHistoryAggregation(); err != nil {
				t.Fatal(err)
			}
			if _, err := st.RegisterDevice(state.Device{DriverName: "sma", Make: "SMA", Serial: "PV-7"}); err != nil {
				t.Fatal(err)
			}
			identity := testEnergyIdentity(st)
			ctrl := tickPersistControl{SiteMeterDriver: "grid"}
			tel := telemetry.NewStore()
			tel.Update("grid", telemetry.DerMeter, 1000, nil, nil)
			tel.RecordDriverSuccess("grid")
			tel.Update("sma", telemetry.DerPV, 0, nil, nil)
			tel.RecordDriverSuccess("sma")
			base := time.Now().UTC().Truncate(24*time.Hour).UnixMilli() - 28*hour
			topologies := map[string]bool{}
			for at := base; at <= base+8*hour; at += tick {
				off := at - base
				if off == 11*60000 {
					tel.DriverHealthMut("sma").SetOffline()
				}
				r := telemetry.ForecastReading{
					At:    time.UnixMilli(at),
					Known: map[telemetry.DerType]bool{telemetry.DerMeter: true, telemetry.DerPV: true, telemetry.DerBattery: true, telemetry.DerEV: true, telemetry.DerV2X: true},
					Sources: map[string]telemetry.ForecastSource{
						"grid:meter": {Flow: telemetry.ForecastFlow{Driver: "grid", DerType: telemetry.DerMeter}},
						"sma:pv":     {Flow: telemetry.ForecastFlow{Driver: "sma", DerType: telemetry.DerPV}},
					},
					GridW: 1000,
				}
				if off > 10*60000 {
					r.Known[telemetry.DerPV], r.Reason = false, "offline:sma:pv"
				}
				v := historySources(r)
				dark := darkPV(v, c.sun)
				v.Topology += historyDevices(st, tel, identity, buildEnergyObservations(st, tel, ctrl, state.HistoryPoint{}, identity), dark)
				topologies[v.Topology] = true
				if known := v.Missing == 0; known != (c.measured || off <= 10*60000) {
					t.Fatalf("%d min: snapshot missing %b", off/60000, v.Missing)
				}
				if err := st.RecordTickWithEnergy(state.HistoryPoint{TsMs: at, Sources: v}, nil, nil); err != nil {
					t.Fatal(err)
				}
			}
			if err := st.FlushHistory(context.Background()); err != nil {
				t.Fatal(err)
			}
			if one := len(topologies) == 1; one != c.measured {
				t.Fatalf("%d topologies over the night", len(topologies))
			}
			d, err := st.DailyEnergy(base, base+8*hour)
			if err != nil {
				t.Fatal(err)
			}
			if d.Missing&state.MissingLoad != 0 || math.Abs(d.LoadWh-8000) > 1e-6 {
				t.Fatalf("Home use %v Wh, missing %b; want 8000 Wh", d.LoadWh, d.Missing)
			}
		})
	}
}
