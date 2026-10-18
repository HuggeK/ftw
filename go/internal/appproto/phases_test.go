package appproto

import (
	"math"
	"testing"
)

func amps(a ...float64) [3]*float64 {
	var out [3]*float64
	for i := range a {
		out[i] = &a[i]
	}
	return out
}

// The phases cross in tenths of an ampere with the sign the box gave them,
// and the fuse rides with them as their scale.
func TestPhasesAreTenthsOfAnAmpereOnTheWire(t *testing.T) {
	f := fieldValues(Snapshot{GridPhaseA: amps(-2.13, 4.76, -1.61), FuseA: 20}, nil)
	want := map[Fid]int64{FidGridL1Da: -21, FidGridL2Da: 48, FidGridL3Da: -16, FidGridFuseDa: 200}
	for fid, v := range want {
		if got, ok := f[fidKey(fid)]; !ok || got != v {
			t.Fatalf("field %d = %d (present %v), want %d", fid, got, ok, v)
		}
	}
}

// No phases, no fuse: the fuse alone is a scale with nothing on it, and a
// site whose meter gives no phases must not look like one with three idle
// ones.
func TestTheFuseTravelsOnlyWithPhases(t *testing.T) {
	f := fieldValues(Snapshot{FuseA: 20}, nil)
	for _, fid := range []Fid{FidGridL1Da, FidGridL2Da, FidGridL3Da, FidGridFuseDa} {
		if _, ok := f[fidKey(fid)]; ok {
			t.Fatalf("field %d went out from a meter with no phases", fid)
		}
	}

	nan := math.NaN()
	f = fieldValues(Snapshot{GridPhaseA: [3]*float64{&nan}, FuseA: 20}, nil)
	if _, ok := f[fidKey(FidGridFuseDa)]; ok {
		t.Fatal("a phase that is not a number counted as a phase")
	}

	f = fieldValues(Snapshot{GridPhaseA: amps(3.2)}, nil)
	if got := f[fidKey(FidGridL1Da)]; got != 32 {
		t.Fatalf("L1 = %d, want 32", got)
	}
	if _, ok := f[fidKey(FidGridL2Da)]; ok {
		t.Fatal("a one-phase site sent L2")
	}
}

// The phases are the site meter's readings, so their age is the meter's.
// The fuse is the box's config and has no age.
func TestThePhasesFollowTheGridMetersFreshness(t *testing.T) {
	dict := fieldDict("meter.p1", "inv", "bat")
	for fid, name := range map[Fid]string{FidGridL1Da: "grid_l1_da", FidGridL2Da: "grid_l2_da", FidGridL3Da: "grid_l3_da"} {
		def := dict[fidKey(fid)]
		if def.Name != name || def.Unit == nil || *def.Unit != "dA" || def.SrcID == nil || *def.SrcID != "meter.p1" {
			t.Fatalf("field %d = %+v, want %s in dA from meter.p1", fid, def, name)
		}
	}
	def := dict[fidKey(FidGridFuseDa)]
	if def.Name != "grid_fuse_da" || def.SrcID != nil {
		t.Fatalf("fuse = %+v, want grid_fuse_da with no source", def)
	}
}

// The extra fields cost lane 0 nothing: a delta carrying every field at the
// largest values a home could show still fills one 512-byte bucket.
func TestEveryFieldAtOnceFitsTheBucket(t *testing.T) {
	h, box, rec, clock := newRig(t)
	box.snap.GridPhaseA = amps(1, 1, 1)
	box.snap.FuseA = 20
	subscribe(t, h, rec)

	clock.uptimeMs += 1000
	box.snap.Mode = "planner_arbitrage"
	box.snap.GridW, box.snap.PVW, box.snap.BatteryW, box.snap.LoadW = -99_999, -99_999, -99_999, 99_999
	box.snap.BatterySoC, box.snap.EVWKnown, box.snap.EVW = 1, true, 99_999
	box.snap.GridPhaseA = amps(-999.9, -999.9, -999.9)
	box.snap.FuseA = 999.9
	if err := h.Tick(); err != nil {
		t.Fatal(err)
	}

	f := rec.only(t, MsgDelta)
	if f.size != 512 || f.flags&FlagTrunc != 0 {
		t.Fatalf("delta is %d bytes, truncated %v; want the bucket, whole", f.size, f.flags&FlagTrunc != 0)
	}
	d := body[Delta](t, f)
	for _, fid := range []Fid{FidGridL1Da, FidGridL2Da, FidGridL3Da, FidGridFuseDa} {
		if _, ok := d.Fields[fidKey(fid)]; !ok {
			t.Fatalf("field %d missing from %v", fid, d.Fields)
		}
	}
}

// When a delta must shrink, the depth goes before the core view: fields
// leave from the highest id, and the extra fields sit above the frozen set.
func TestTheExtraFieldsGiveWayFirst(t *testing.T) {
	h, _, _, _ := newRig(t)
	full := fieldValues(Snapshot{
		GridW: 1200, PVW: -3400, BatteryW: 900, LoadW: 3700,
		LoadWKnown: true,
		BatterySoC: 0.62, BatterySoCKnown: true, EVWKnown: true, EVW: 7360,
		GridPhaseA: amps(-21.3, 47.6, -16.1), FuseA: 20,
	}, nil)

	// The smallest bucket that still holds the frozen fields alone.
	frozenOnly := map[string]int64{}
	for k, v := range full {
		if fidOf(k) <= uint64(FidEvW) {
			frozenOnly[k] = v
		}
	}
	env, err := newEnvelope(MsgDelta, nil, Delta{Seq: 1, UptimeMs: 1, Fields: frozenOnly})
	if err != nil {
		t.Fatal(err)
	}
	raw, err := EncodeEnvelope(env)
	if err != nil {
		t.Fatal(err)
	}

	body := Delta{Seq: 1, UptimeMs: 1, Fields: full}
	kept, flags, err := h.fitDelta(&body, len(raw)+6)
	if err != nil {
		t.Fatal(err)
	}
	if flags&FlagTrunc == 0 {
		t.Fatal("a delta that did not fit went out without TRUNC")
	}
	if len(kept) != len(frozenOnly) {
		t.Fatalf("kept %v, want the frozen fields and nothing else", kept)
	}
	for k := range frozenOnly {
		if _, ok := kept[k]; !ok {
			t.Fatalf("frozen field %s was dropped while extra fields remained", k)
		}
	}
}

// A delta carries values, never an absence. A phase the meter stopped giving
// would stay on the phone at its last value, so the box resends the whole set
// in a snapshot, once.
func TestAFieldThatStopsComingResendsTheSnapshot(t *testing.T) {
	h, box, rec, _ := newRig(t)
	box.snap.GridPhaseA = amps(2.1, 4.8, 1.6)
	box.snap.FuseA = 20
	subscribe(t, h, rec)

	if err := h.Tick(); err != nil {
		t.Fatal(err)
	}
	if rec.has(MsgSnap) {
		t.Fatal("nothing changed and a snapshot went out")
	}

	box.snap.GridPhaseA = [3]*float64{}
	rec.reset()
	if err := h.Tick(); err != nil {
		t.Fatal(err)
	}
	snap := body[Snap](t, rec.only(t, MsgSnap))
	for _, fid := range []Fid{FidGridL1Da, FidGridL2Da, FidGridL3Da, FidGridFuseDa} {
		if _, ok := snap.Fields[fidKey(fid)]; ok {
			t.Fatalf("the snapshot still carries field %d", fid)
		}
	}
	// Lane 0 keeps its beat: the second's frame still goes.
	if f := rec.frames[len(rec.frames)-1]; f.lane != LaneControl || f.size != 512 {
		t.Fatalf("the second ended on lane %d with %d bytes", f.lane, f.size)
	}

	rec.reset()
	if err := h.Tick(); err != nil {
		t.Fatal(err)
	}
	if rec.has(MsgSnap) {
		t.Fatal("the snapshot went out again with nothing gone")
	}
}
