package protocol

import (
	"fmt"
	"math"
)

// History tiles: the geometry both ends compute.
//
// Resolutions, tile spans, strides, ids and the etag are the same arithmetic
// as the app's src/lib/protocol/history.ts, because the two ends must compute
// identical tiles from identical inputs or the etag comparison silently
// degrades into "always refetch". Spans and retention come from
// contract/registry.yaml through contract_gen.go; nothing here is hand-written
// twice. Deciding which tiles answer a query, and reading the store, belong to
// whoever serves the query.

// HistorySample is one stored bucket of one series, in mean watts.
type HistorySample struct {
	// StartMs is the bucket start, wall clock.
	StartMs int64
	// W is the mean power over the bucket, site-signed.
	W float64
}

// ResolutionStepMs is the stored bucket width per resolution. This is the
// meaning of the resolution's name; the registry carries spans and retention.
var ResolutionStepMs = map[Resolution]int64{
	ResN5m: 300_000,
	ResN1h: 3_600_000,
}

// MissingSample marks a hole on the wire. Distinct from zero, which is a real
// reading. Same value as the app's MISSING_SAMPLE.
const MissingSample = math.MinInt32

// PointsPerTile is how many stored buckets one tile spans at stride 1.
func PointsPerTile(res Resolution) int64 {
	return ResolutionTileSpanMs[res] / ResolutionStepMs[res]
}

// TileStartFor is the start of the epoch-aligned tile that contains atMs.
func TileStartFor(res Resolution, atMs int64) int64 {
	span := ResolutionTileSpanMs[res]
	return (atMs / span) * span
}

// TileIDFor matches the app's tileId(): res/stride/tileIndex.
func TileIDFor(res Resolution, stride, startMs int64) string {
	return fmt.Sprintf("%s/%d/%d", res, stride, startMs/ResolutionTileSpanMs[res])
}

// EtagOf is FNV-1a over the packed block, eight hex digits — the same hash the
// app computes, because the whole point of an etag is that both ends agree on
// it.
func EtagOf(data []byte) string {
	h := uint32(0x811c9dc5)
	for _, b := range data {
		h ^= uint32(b)
		h *= 0x01000193
	}
	return fmt.Sprintf("%08x", h)
}

// PackTile packs one tile from stored buckets: int32 LE columns, one block per
// series, MissingSample where the store has no bucket. windows holds each
// series' buckets at the resolution's stored width; buckets outside the tile
// are ignored. stride must divide PointsPerTile(res). The tile is partial,
// and never cached, while it ends after nowMs.
func PackTile(
	res Resolution,
	stride, startMs int64,
	series []string,
	windows map[string][]HistorySample,
	nowMs int64,
) HistChunk {
	stepMs := ResolutionStepMs[res]
	tileEnd := startMs + ResolutionTileSpanMs[res]
	points := PointsPerTile(res) / stride

	data := make([]byte, int64(len(series))*points*4)
	for s, name := range series {
		samples := windows[name]

		// Stored buckets fold into strided points by plain mean: the ledger's
		// buckets are equal width, so each carries equal weight.
		sums := make([]float64, points)
		counts := make([]int64, points)
		for _, sample := range samples {
			if sample.StartMs < startMs || sample.StartMs >= tileEnd {
				continue
			}
			p := (sample.StartMs - startMs) / (stepMs * stride)
			if p < 0 || p >= points {
				continue
			}
			sums[p] += sample.W
			counts[p]++
		}

		base := int64(s) * points * 4
		for p := int64(0); p < points; p++ {
			v := int32(MissingSample)
			if counts[p] > 0 {
				v = ClampSample(sums[p] / float64(counts[p]))
			}
			off := base + p*4
			u := uint32(v)
			data[off] = byte(u)
			data[off+1] = byte(u >> 8)
			data[off+2] = byte(u >> 16)
			data[off+3] = byte(u >> 24)
		}
	}

	return HistChunk{
		TileID:  TileIDFor(res, stride, startMs),
		Etag:    EtagOf(data),
		Res:     res,
		StartMs: startMs,
		StepMs:  stepMs * stride,
		Series:  series,
		Data:    data,
		Partial: tileEnd > nowMs,
	}
}

// ClampSample rounds a mean to the int32 the wire carries.
func ClampSample(v float64) int32 {
	r := math.Round(v)
	// One step inside the limits: MinInt32 itself means "missing", and a
	// real reading must never be mistaken for a hole.
	if r >= math.MaxInt32 {
		return math.MaxInt32
	}
	if r <= MissingSample+1 {
		return MissingSample + 1
	}
	return int32(r)
}
