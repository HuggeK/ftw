package state

import (
	"context"
	"database/sql"
	"fmt"
	"path/filepath"
	"testing"
	"time"
)

func sourceTick(t *testing.T, s *Store, at int64, v HistorySources) {
	t.Helper()
	v.AtMS = at
	if v.Topology == "" {
		v.Topology = "grid+charger"
	}
	// Deliberately contradictory instantaneous Load: readers must use sources.
	p := HistoryPoint{TsMs: at, LoadW: 999999, Missing: MissingLoad | BalanceMismatch, JSON: `{"ev_w":0,"v2x_w":0}`, Sources: &v}
	dashboardTick(t, s, fmt.Sprint(at), &p)
}

func sourceStore(t *testing.T) *Store {
	t.Helper()
	s := freshStore(t)
	if err := s.EnableHistoryAggregation(); err != nil {
		t.Fatal(err)
	}
	return s
}

func sourceSeries(t *testing.T, s *Store, base int64, minutes []float64) {
	t.Helper()
	for i := int64(0); i <= int64(len(minutes))*6; i++ {
		minute := min(int(i/6), len(minutes)-1)
		sourceTick(t, s, base+i*10000, HistorySources{GridW: minutes[minute] + 2000, EVW: 2000})
	}
}

func TestSourceLoadBalancesAfterAggregation(t *testing.T) {
	s := sourceStore(t)
	base := time.Now().UTC().Truncate(24 * time.Hour).UnixMilli()
	sourceSeries(t, s, base, []float64{-1000, 3000, 1000, 1000, 1000})
	ctx := context.Background()
	minute, err := s.sourceLoadBuckets(ctx, base, base+300000, 60000)
	if err != nil || len(minute) != 5 {
		t.Fatal("minute evidence unavailable", err)
	}
	if minute[0].missing&MissingLoad == 0 {
		t.Fatal("negative entire minute became valid Load")
	}
	check := func(s *Store) {
		t.Helper()
		b, err := s.sourceLoadBuckets(ctx, base, base+300000, 300000)
		if err != nil || len(b) != 1 || b[0].missing&MissingLoad != 0 {
			t.Fatal("signed interval balance missing", err)
		}
		near(t, "interval Wh", b[0].wh, 1000.0/12)
		d, err := s.DailyEnergy(base, base+300000)
		if err != nil || d.Missing&MissingLoad != 0 {
			t.Fatal("daily balance missing", err)
		}
		near(t, "daily Wh", d.LoadWh, b[0].wh)
		web, err := s.LoadHistory(base, base+300000, 1)
		if err != nil || len(web) != 1 || web[0].Missing&MissingLoad != 0 {
			t.Fatal("web balance missing", err)
		}
		near(t, "web W", web[0].LoadW, 1000)
		native, _, err := s.LoadEnergyHistory(EnergyHistoryQuery{SinceMS: base, UntilMS: base + 300000, BucketMS: 300000, Limit: 10})
		if err != nil {
			t.Fatal(err)
		}
		var wh float64
		var n int
		for _, p := range native {
			if p.Flow == FlowConsumerUse {
				wh += p.EnergyWh
				n++
			}
		}
		if n != 1 {
			t.Fatal("native household missing or duplicated")
		}
		near(t, "native Wh", wh, b[0].wh)
	}
	check(s)
	path := s.mainDBPath
	if err := s.Close(); err != nil {
		t.Fatal(err)
	}
	reopened, err := Open(path)
	if err != nil {
		t.Fatal(err)
	}
	defer reopened.Close()
	check(reopened)
}

func TestSourceLoadKeepsUnknownDistinctFromZero(t *testing.T) {
	for _, kind := range []string{"zero", "missing EV", "write gap", "topology", "partial tail", "time reversal", "duplicate"} {
		t.Run(kind, func(t *testing.T) {
			s := sourceStore(t)
			base := time.Now().UTC().Truncate(24 * time.Hour).UnixMilli()
			for i := int64(0); i <= 6; i++ {
				if kind == "write gap" && i == 3 {
					continue
				}
				if kind == "partial tail" && i > 1 {
					break
				}
				v := HistorySources{}
				if kind == "missing EV" && i == 3 {
					v.Missing = MissingEV
				}
				if kind == "topology" && i >= 3 {
					v.Topology = "new meter+charger"
				}
				if kind == "time reversal" && i == 3 {
					sourceTick(t, s, base+5000, v)
				}
				sourceTick(t, s, base+i*10000, v)
				if kind == "duplicate" {
					sourceTick(t, s, base+i*10000, v)
				}
			}
			b, err := s.sourceLoadBuckets(context.Background(), base, base+60000, 60000)
			if err != nil || len(b) != 1 {
				t.Fatal(err)
			}
			// A brief unknown between known receipts is bridged, but only as a
			// marked estimate. Other missing evidence stays unknown.
			bridged := kind == "missing EV" || kind == "write gap" || kind == "topology"
			known := b[0].missing&MissingLoad == 0
			if known != (kind == "zero" || kind == "duplicate" || bridged) {
				t.Fatal("missing evidence classified as measured zero", kind)
			}
			if (b[0].missing&EstimatedLoad != 0) != bridged {
				t.Fatal("bridge marker wrong", kind)
			}
			if known {
				near(t, "zero Wh", b[0].wh, 0)
			}
		})
	}
}

// A snapshot holding an inferred value, such as PV read as 0 W in twilight,
// marks every interval it bounds and no other, so storage keeps it apart from
// measurement.
func TestSourceLoadMarksIntervalsOfAnEstimatedSnapshot(t *testing.T) {
	s := sourceStore(t)
	base := time.Now().UTC().Truncate(24 * time.Hour).UnixMilli()
	for at := base; at <= base+180000; at += 5000 {
		v := HistorySources{GridW: 1000}
		if at > base+60000 && at <= base+120000 {
			v.Missing = EstimatedLoad
		}
		sourceTick(t, s, at, v)
	}
	b, err := s.sourceLoadBuckets(context.Background(), base, base+180000, 60000)
	if err != nil || len(b) != 3 {
		t.Fatal(err, len(b))
	}
	for i, estimated := range []bool{false, true, true} {
		if b[i].missing&MissingPower != 0 || (b[i].missing&EstimatedLoad != 0) != estimated {
			t.Fatalf("minute %d missing %b, want estimated=%v", i, b[i].missing, estimated)
		}
		near(t, "minute Wh", b[i].wh, 1000.0/60)
	}
}

func TestSourceLoadUsesSnapshotTimeAndMarksRestartBridge(t *testing.T) {
	path := filepath.Join(t.TempDir(), "state.db")
	s, err := Open(path)
	if err != nil {
		t.Fatal(err)
	}
	if err := s.EnableHistoryAggregation(); err != nil {
		t.Fatal(err)
	}
	base := time.Now().UTC().Truncate(24 * time.Hour).UnixMilli()
	for i := int64(0); i < 3; i++ {
		p := HistoryPoint{TsMs: base + i*10000, JSON: `{}`, Sources: &HistorySources{AtMS: base + 20000 + i*10000, Topology: "grid", GridW: 1000}}
		dashboardTick(t, s, fmt.Sprint(i), &p)
	}
	var first, covered int64
	if err := s.history.QueryRow(`SELECT MIN(first_ms),SUM(covered_ms) FROM history_source_energy`).Scan(&first, &covered); err != nil {
		t.Fatal(err)
	}
	if first != base+20000 || covered != 20000 {
		t.Fatal("source power assigned before observation")
	}
	s = reopenSourceStore(t, s)
	sourceTick(t, s, base+50000, HistorySources{Topology: "grid", GridW: 1000})
	var missing HistoryMissing
	if err := s.history.QueryRow(`SELECT SUM(covered_ms),(SELECT missing FROM history_quality WHERE kind='source_energy') FROM history_source_energy`).Scan(&covered, &missing); err != nil {
		t.Fatal(err)
	}
	if covered != 30000 || missing != EstimatedLoad {
		t.Fatal("brief stop not bridged as a marked estimate", covered, missing)
	}
	s = reopenSourceStore(t, s)
	sourceTick(t, s, base+50000+maxSourceBridgeMS+1, HistorySources{Topology: "grid", GridW: 1000})
	if err := s.history.QueryRow(`SELECT SUM(covered_ms) FROM history_source_energy`).Scan(&covered); err != nil {
		t.Fatal(err)
	}
	if covered != 30000 {
		t.Fatal("integrated across a long stop")
	}
}

func TestSourceLoadKeepsOlderWriterPeriod(t *testing.T) {
	s := sourceStore(t)
	base := time.Now().UTC().Truncate(24 * time.Hour).UnixMilli()
	sourceSeries(t, s, base, []float64{1000})
	// Simulate rollback: its writer has no Sources field. A later new writer
	// must keep this separately recorded minute without double-counting it.
	for i := int64(0); i <= 6; i++ {
		p := HistoryPoint{TsMs: base + 120000 + i*10000, GridW: 1000, LoadW: 1000, JSON: `{"ev_w":0,"v2x_w":0}`}
		dashboardTick(t, s, "old-"+fmt.Sprint(i), &p)
	}
	sourceSeries(t, s, base+240000, []float64{1000})
	d, err := s.DailyEnergy(base, base+300000)
	if err != nil {
		t.Fatal(err)
	}
	// Two source minutes and the older writer's minute (its opening interval
	// follows the older integration policy and is deliberately queried alone).
	old, err := s.DailyEnergy(base+120000, base+180000)
	if err != nil {
		t.Fatal(err)
	}
	near(t, "older writer Wh", old.LoadWh, 1000.0/60)
	if d.LoadWh < 3*1000.0/60-1e-8 {
		t.Fatal("rollback history hidden by source method")
	}
}

func TestSourceLoadTariffBalanceAndIndependentGridCost(t *testing.T) {
	s := sourceStore(t)
	base := time.Now().UTC().Truncate(24 * time.Hour).UnixMilli()
	sourceSeries(t, s, base, []float64{-1000, 3000})
	slots := []priceSlot{{StartMs: base, EndMs: base + 60000, TotalOre: 100}, {StartMs: base + 60000, EndMs: base + 120000, TotalOre: 200}}
	c, err := s.integrateHistoryRange(context.Background(), base, base+120000, slots, ExportPricing{})
	if err != nil {
		t.Fatal(err)
	}
	near(t, "valid tariff baseline", c.BaselineHouseOre, 50*200.0/1000)
	near(t, "independent import cost", c.ImportCostOre, (1000.0/60)*.1+(5000.0/60)*.2)
	if c.PricedCoveredMs != 60000 {
		t.Fatal("negative tariff balance counted as priced household use")
	}
	whole := []priceSlot{{StartMs: base, EndMs: base + 120000, TotalOre: 100}}
	c, err = s.integrateHistoryRange(context.Background(), base, base+120000, whole, ExportPricing{})
	if err != nil {
		t.Fatal(err)
	}
	near(t, "common tariff balance", c.BaselineHouseOre, (2000.0/60)*.1)
}

func TestSourceLoadRetentionBackupAndDayBoundary(t *testing.T) {
	s := sourceStore(t)
	base := time.Now().UTC().Truncate(24 * time.Hour).Add(-15 * time.Minute).UnixMilli()
	minutes := make([]float64, 30)
	for i := range minutes {
		minutes[i] = 1000
	}
	sourceSeries(t, s, base, minutes)
	// A closed-day cache can exist while the last delayed receipt commits.
	day := time.UnixMilli(base).UTC().Format("2006-01-02")
	if err := s.SaveDailyEnergy(day, DayEnergy{LoadWh: 1, Intervals: 1}); err != nil {
		t.Fatal(err)
	}
	sourceTick(t, s, base+1800000+10000, HistorySources{GridW: 3000, EVW: 2000})
	if _, hit, err := s.LoadDailyEnergy(day); err != nil || hit {
		t.Fatal("affected closed-day cache retained stale Load", err)
	}
	for _, bounds := range [][2]int64{{base, base + 900000}, {base + 900000, base + 1800000}} {
		d, err := s.DailyEnergy(bounds[0], bounds[1])
		if err != nil {
			t.Fatal(err)
		}
		near(t, "day boundary Wh", d.LoadWh, 250)
	}
	if err := s.rollSiteEnergy(context.Background(), base+1800000); err != nil {
		t.Fatal(err)
	}
	b, err := s.sourceLoadBuckets(context.Background(), base, base+1800000, 900000)
	if err != nil || len(b) != 2 {
		t.Fatal("retained source quarters missing", err)
	}
	for _, v := range b {
		if v.missing&MissingLoad != 0 {
			t.Fatal("retention lost coverage")
		}
		near(t, "retained Wh", v.wh, 250)
	}
	for _, asset := range []string{"", ObservedConsumerAssetID} {
		p, truncated, err := s.LoadEnergyHistory(EnergyHistoryQuery{AssetID: asset, SinceMS: base + 17123, UntilMS: base + 1800000, BucketMS: 300000, Limit: 10})
		if err != nil || truncated || len(p) != 2 {
			t.Fatal("retained source history lost whole intervals", err)
		}
		for _, v := range p {
			if v.BucketLenMS != 900000 || v.BucketStartMS%900000 != 0 {
				t.Fatal("retained source interval was split")
			}
			near(t, "retained native Wh", v.EnergyWh, 250)
		}
	}
	backup := filepath.Join(t.TempDir(), "restored.db")
	if err := s.copyStateForBackup(context.Background(), backup); err != nil {
		t.Fatal(err)
	}
	if err := s.exportHistoryToSQLite(context.Background(), backup); err != nil {
		t.Fatal(err)
	}
	db, err := openRaw(backup)
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	for _, table := range []string{sourceEnergyTable, "history_source_cursor", "history_source_periods"} {
		a, na, err := sourceTableDigest(s.history, table)
		if err != nil {
			t.Fatal(err)
		}
		b, nb, err := sourceTableDigest(db, table)
		if err != nil {
			t.Fatal(err)
		}
		if na != nb || a != b {
			t.Fatal("backup changed source evidence")
		}
	}
	if err := db.Close(); err != nil {
		t.Fatal(err)
	}
	restored, err := Open(backup)
	if err != nil {
		t.Fatal(err)
	}
	defer restored.Close()
	d, err := restored.DailyEnergy(base, base+1800000)
	if err != nil {
		t.Fatal(err)
	}
	near(t, "restored household Wh", d.LoadWh, 500)
}

func sourceTableDigest(db *sql.DB, table string) (string, int64, error) {
	rows, err := db.Query(`SELECT * FROM ` + table + historyOrder(table))
	if err != nil {
		return "", 0, err
	}
	defer rows.Close()
	return hashHistoryRows(rows)
}

func TestSourceLoadBackupRefusesIncompleteMethod(t *testing.T) {
	s := sourceStore(t)
	if _, err := s.history.Exec(`DROP TABLE history_source_periods`); err != nil {
		t.Fatal(err)
	}
	if err := s.exportHistoryToSQLite(context.Background(), filepath.Join(t.TempDir(), "history.db")); err == nil {
		t.Fatal("export accepted incomplete source method")
	}
}

func TestSourceLoadTruncationWithoutLegacyLedger(t *testing.T) {
	s := sourceStore(t)
	base := time.Now().UTC().Truncate(24 * time.Hour).UnixMilli()
	minutes := make([]float64, 15)
	for i := range minutes {
		minutes[i] = 1000
	}
	sourceSeries(t, s, base, minutes)
	p, truncated, err := s.LoadEnergyHistory(EnergyHistoryQuery{SinceMS: base, UntilMS: base + 900000, BucketMS: 300000, Limit: 1})
	if err != nil || !truncated || len(p) != 1 {
		t.Fatal("source-only query lost truncation metadata", err)
	}
}

func TestSourceLoadMidMinuteTransitionKeepsLegacyPrefix(t *testing.T) {
	s := sourceStore(t)
	base := time.Now().UTC().Truncate(24 * time.Hour).UnixMilli()
	for i := int64(0); i <= 3; i++ {
		p := HistoryPoint{TsMs: base + i*10000, GridW: 1000, LoadW: 1000, Missing: MissingSoC, JSON: `{}`}
		dashboardTick(t, s, "legacy-prefix-"+fmt.Sprint(i), &p)
	}
	for i := int64(3); i <= 12; i++ {
		sourceTick(t, s, base+i*10000, HistorySources{GridW: 1000})
	}
	d, err := s.DailyEnergy(base, base+120000)
	if err != nil {
		t.Fatal(err)
	}
	near(t, "source and legacy Wh", d.LoadWh, 1000.0/30)
	near(t, "source and legacy import", d.ImportWh, 1000.0/30)
	var legacyLoad float64
	if err := s.history.QueryRow(`SELECT SUM(load_wh) FROM (SELECT load_wh FROM history_site_energy UNION ALL SELECT load_wh FROM history_site_energy_partial)`).Scan(&legacyLoad); err != nil {
		t.Fatal(err)
	}
	near(t, "unchanged prefix", legacyLoad, 1000.0/120)
}

func TestSourceLoadV2XDischargeIsNotNegativeEVCharge(t *testing.T) {
	s := sourceStore(t)
	base := time.Now().UTC().Truncate(24 * time.Hour).UnixMilli()
	for i := int64(0); i <= 6; i++ {
		sourceTick(t, s, base+i*10000, HistorySources{GridW: -1000, V2XW: -2000})
	}
	c, err := s.integrateHistoryRange(context.Background(), base, base+60000, []priceSlot{{StartMs: base, EndMs: base + 60000, TotalOre: 100}}, ExportPricing{})
	if err != nil {
		t.Fatal(err)
	}
	near(t, "V2X house Wh", c.LoadWh, 1000.0/60)
	near(t, "EV charging Wh", c.EVWh, 0)
}

func TestSourceLoadKeepsObservedConsumerAssetReads(t *testing.T) {
	s := sourceStore(t)
	base := time.Now().UTC().Truncate(24 * time.Hour).UnixMilli()
	sourceSeries(t, s, base, []float64{-1000, 3000, 1000, 1000, 1000})
	assets, err := s.EnergyAssets()
	if err != nil {
		t.Fatal(err)
	}
	found := false
	for _, a := range assets {
		if a.AssetID == ObservedConsumerAssetID && a.Kind == AssetObservedConsumer && a.ReadOnly && a.LastSeenMS == base+300000 {
			found = true
		}
	}
	if !found {
		t.Fatal("household source vanished from asset discovery")
	}
	for _, asset := range []string{"", ObservedConsumerAssetID} {
		p, _, err := s.LoadEnergyHistory(EnergyHistoryQuery{AssetID: asset, SinceMS: base, UntilMS: base + 300000, BucketMS: 300000, Limit: 1})
		if err != nil || len(p) != 1 {
			t.Fatal("household asset history missing", err)
		}
		near(t, "household asset Wh", p[0].EnergyWh, 1000.0/12)
	}
}

func TestSourceLoadLimitCountsStoredBucketsAcrossGaps(t *testing.T) {
	s := sourceStore(t)
	base := time.Now().UTC().Truncate(24 * time.Hour).UnixMilli()
	first := base + 3*3600000
	second := first + 3600000
	sourceSeries(t, s, first, []float64{1000, 1000, 1000, 1000, 1000})
	sourceSeries(t, s, second, []float64{1000, 1000, 1000, 1000, 1000})
	for _, asset := range []string{"", ObservedConsumerAssetID} {
		for _, limit := range []int{1, 2} {
			p, truncated, err := s.LoadEnergyHistory(EnergyHistoryQuery{AssetID: asset, SinceMS: base, UntilMS: second + 300000, BucketMS: 300000, Limit: limit})
			if err != nil || len(p) != limit || truncated != (limit == 1) {
				t.Fatalf("limit=%d: %d points, truncated=%v: %v", limit, len(p), truncated, err)
			}
			if p[0].BucketStartMS != first || limit == 2 && p[1].BucketStartMS != second {
				t.Fatal("empty prefix or internal gap consumed the result limit")
			}
		}
	}
}

func TestSourceLoadLimitMergesLegacyBeforeRanking(t *testing.T) {
	s := sourceStore(t)
	base := time.Now().UTC().Truncate(24 * time.Hour).UnixMilli()
	sourceSeries(t, s, base, []float64{-1000, -1000, -1000, -1000, -1000})
	dashboardTick(t, s, "legacy-period", &HistoryPoint{TsMs: base + 600000, GridW: 1000, LoadW: 1000, JSON: `{}`})
	sourceSeries(t, s, base+900000, []float64{1000, 1000, 1000, 1000, 1000})
	for _, at := range []int64{base, base + 600000} {
		insertLedgerEntryTest(t, s, ObservedConsumerAssetID, FlowConsumerUse, at, 300000, 100, "power_telemetry", "integrated", "legacy", 5)
	}
	insertLedgerEntryTest(t, s, "meter", FlowGridImport, base+600000, 300000, 100, "hardware_counter", "measured", "counter", 1)
	for _, asset := range []string{"", ObservedConsumerAssetID} {
		p, truncated, err := s.LoadEnergyHistory(EnergyHistoryQuery{AssetID: asset, SinceMS: base, UntilMS: base + 1200000, BucketMS: 300000, Limit: 1})
		want := 1
		if asset == "" {
			want = 2
		}
		if err != nil || !truncated || len(p) != want {
			t.Fatalf("legacy/source merge: %d points, truncated=%v: %v", len(p), truncated, err)
		}
		for _, v := range p {
			if v.BucketStartMS != base+600000 {
				t.Fatal("overridden consumer bucket hid a later legacy bucket")
			}
		}
	}
}

func TestSourceLoadLimitFiltersAggregatedLegacyOverlap(t *testing.T) {
	s := sourceStore(t)
	base := time.Now().UTC().Truncate(24 * time.Hour).UnixMilli()
	sourceSeries(t, s, base+300000, []float64{1000, 1000, 1000, 1000, 1000})
	insertLedgerEntryTest(t, s, ObservedConsumerAssetID, FlowConsumerUse, base, 300000, 100, "power_telemetry", "integrated", "legacy", 5)
	insertLedgerEntryTest(t, s, ObservedConsumerAssetID, FlowConsumerUse, base+900000, 900000, 100, "power_telemetry", "integrated", "legacy", 5)
	p, truncated, err := s.LoadEnergyHistory(EnergyHistoryQuery{AssetID: ObservedConsumerAssetID, SinceMS: base, UntilMS: base + 1800000, BucketMS: 900000, Limit: 1})
	if err != nil || truncated || len(p) != 1 || p[0].BucketStartMS != base+900000 {
		t.Fatal("aggregated overlap consumed the result limit", err)
	}
}

func TestSourceLoadAlignsArbitraryRequestTimes(t *testing.T) {
	s := sourceStore(t)
	base := time.Now().UTC().Truncate(24 * time.Hour).UnixMilli()
	minutes := make([]float64, 30)
	for i := range minutes {
		minutes[i] = 1000
	}
	sourceSeries(t, s, base, minutes)
	insertLedgerEntryTest(t, s, "meter", FlowGridImport, base, 300000, 100, "hardware_counter", "measured", "counter", 1)
	for _, asset := range []string{"", ObservedConsumerAssetID} {
		for _, width := range []int64{300000, 420000, 900000} {
			p, truncated, err := s.LoadEnergyHistory(EnergyHistoryQuery{AssetID: asset, SinceMS: base + 137123, UntilMS: base + 1800000, BucketMS: width, Limit: 10})
			if err != nil || truncated {
				t.Fatal("unaligned history request failed", err)
			}
			consumer := 0
			for _, v := range p {
				actualWidth := ((width + 300000 - 1) / 300000) * 300000
				if v.BucketStartMS%300000 != 0 || v.BucketLenMS != actualWidth {
					t.Fatal("history flows use different, split-minute intervals")
				}
				if v.Flow == FlowConsumerUse {
					consumer++
				}
			}
			actualWidth := ((width + 300000 - 1) / 300000) * 300000
			if consumer != int(1800000/actualWidth) {
				t.Fatal("arbitrary request timestamp hid complete source intervals")
			}
		}
	}
}
