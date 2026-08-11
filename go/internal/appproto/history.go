package appproto

import (
	"context"
	"errors"
	"fmt"
	"math"
	"time"
)

// History: the box's side of the tile protocol.
//
// The geometry here — resolutions, tile spans, strides, ids, the etag — is
// the same arithmetic as the app's src/lib/protocol/history.ts, because the
// two ends must compute identical tiles from identical inputs or the etag
// comparison silently degrades into "always refetch". Spans and retention
// come from contract/registry.yaml through contract_gen.go; nothing here is
// hand-written twice.
//
// Power series are derived from the energy ledger: a bucket's mean power is
// its energy divided by its width. That is an average, and it is presented as
// one — the chunk carries its step, and the app labels it. Sub-bucket wiggles
// are gone, which is the correct price for history that survives a restart.

// HistorySample is one stored bucket of one series, in mean watts.
type HistorySample struct {
	// StartMs is the bucket start, wall clock.
	StartMs int64
	// W is the mean power over the bucket, site-signed.
	W float64
}

// HistoryProvider answers windows of stored series.
//
// An interface so appproto never touches SQL, and so tests can serve
// synthetic ledgers. A nil provider means the box keeps no history and says
// so — the state this package shipped in first.
type HistoryProvider interface {
	// Window returns every requested series overlapping [fromMs, toMs), at the
	// stored bucket width stepMs. One call is one store read; asking once per
	// series made a four-column tile scan the same SQLite range four times.
	// Missing buckets and unknown series are absent from the result.
	Window(ctx context.Context, names []string, stepMs, fromMs, toMs int64) (map[string][]HistorySample, error)
}

// resolutionStepMs is the stored bucket width per resolution. This is the
// meaning of the resolution's name; the registry carries spans and retention.
var resolutionStepMs = map[Resolution]int64{
	ResN5m: 300_000,
	ResN1h: 3_600_000,
}

// resolutionOrder is fine to coarse: the order the box clamps along.
var resolutionOrder = []Resolution{ResN5m, ResN1h}

const (
	// defaultMaxPoints matches the app's DEFAULT_MAX_POINTS and is also the
	// server ceiling. A client may ask for fewer points, never more.
	defaultMaxPoints = 2000
	// Sixteen full 168-point columns occupy 10,752 bytes, leaving room for
	// metadata inside the 16 KiB bulk bucket while allowing additive fields.
	maxHistorySeries = 16
	historyTimeout   = 15 * time.Second
)

// missingSample marks a hole on the wire. Distinct from zero, which is a
// real reading. Same value as the app's MISSING_SAMPLE.
const missingSample = math.MinInt32

func retentionMsOf(res Resolution) int64 {
	return int64(ResolutionRetentionDays[res]) * 86_400_000
}

func pointsPerTile(res Resolution) int64 {
	return ResolutionTileSpanMs[res] / resolutionStepMs[res]
}

func tileStartFor(res Resolution, atMs int64) int64 {
	span := ResolutionTileSpanMs[res]
	return (atMs / span) * span
}

// tileIDFor matches the app's tileId(): res/stride/tileIndex.
func tileIDFor(res Resolution, stride, startMs int64) string {
	return fmt.Sprintf("%s/%d/%d", res, stride, startMs/ResolutionTileSpanMs[res])
}

// etagOf is FNV-1a over the packed block, eight hex digits — the same hash
// the app computes, because the whole point of an etag is that both ends
// agree on it.
func etagOf(data []byte) string {
	h := uint32(0x811c9dc5)
	for _, b := range data {
		h ^= uint32(b)
		h *= 0x01000193
	}
	return fmt.Sprintf("%08x", h)
}

type plannedTile struct {
	id      string
	startMs int64
	points  int64
}

type historyPlan struct {
	res    Resolution
	stride int64
	stepMs int64
	tiles  []plannedTile
}

// planHistoryQuery is the app's planQuery(), ported term for term. Both ends
// run it on the same inputs and must land on the same tiles.
func planHistoryQuery(res Resolution, fromMs, toMs int64, maxPoints int) historyPlan {
	pointCap := int64(maxPoints)
	if pointCap < 1 {
		pointCap = 1
	}

	// Transfer is whole tiles, so the count that must fit under the cap is
	// the tile-aligned span, not the requested one.
	alignedSpan := func(candidate Resolution) int64 {
		span := ResolutionTileSpanMs[candidate]
		first := tileStartFor(candidate, fromMs)
		last := tileStartFor(candidate, max(fromMs, toMs-1))
		return last + span - first
	}

	rank := func(r Resolution) int {
		for i, candidate := range resolutionOrder {
			if candidate == r {
				return i
			}
		}
		return 0
	}

	// Clamp to a coarser store before aggregating: coarser stored data is
	// real data, while an aggregate of finer data is an average of it.
	chosen := resolutionOrder[len(resolutionOrder)-1]
	for _, candidate := range resolutionOrder {
		if rank(candidate) < rank(res) {
			continue
		}
		chosen = candidate
		if alignedSpan(candidate)/resolutionStepMs[candidate] <= pointCap {
			break
		}
	}

	// The stride must divide the tile's point count evenly, or a tile stops
	// being a whole number of points and every boundary needs special
	// handling. Constraining it to a divisor keeps tiles independent.
	base := resolutionStepMs[chosen]
	total := alignedSpan(chosen)
	perTile := pointsPerTile(chosen)
	stride := perTile
	for k := int64(1); k <= perTile; k++ {
		if perTile%k != 0 {
			continue
		}
		if total/(base*k) <= pointCap {
			stride = k
			break
		}
	}

	span := ResolutionTileSpanMs[chosen]
	first := tileStartFor(chosen, fromMs)
	last := tileStartFor(chosen, max(fromMs, toMs-1))

	var tiles []plannedTile
	for start := first; start <= last; start += span {
		tiles = append(tiles, plannedTile{
			id:      tileIDFor(chosen, stride, start),
			startMs: start,
			points:  perTile / stride,
		})
	}

	return historyPlan{res: chosen, stride: stride, stepMs: base * stride, tiles: tiles}
}

// --------------------------------------------------------------------------
// Serving a query
// --------------------------------------------------------------------------

type historyRequest struct {
	generation uint64
	id         uint32
	series     []string
	plan       historyPlan
	have       map[string]string
	gaps       []HistGap
	nowMs      int64
}

func (h *Handler) onHistQuery(ctx context.Context, env Envelope) error {
	_ = ctx // Work derives from the handler lifetime after validation below.
	if env.ID == nil {
		// A response nobody can route is a response nobody asked for.
		return nil
	}
	if h.cfg.History == nil {
		return h.sendError(env.ID, ErrorBody{
			Code:      ErrUnavailable,
			Retryable: ErrorRetryable[ErrUnavailable],
			Args:      map[string]any{"subsystem": "history"},
		})
	}

	var q HistQuery
	if err := Unmarshal(env.B, &q); err != nil {
		h.log.Warn("undecodable hist.query dropped", "err", err)
		return nil
	}
	nowMs := h.cfg.Clock.Now().UnixMilli()
	if _, ok := ResolutionTileSpanMs[q.Res]; !ok || len(q.Series) == 0 ||
		len(q.Series) > maxHistorySeries || q.FromMs < 0 || q.ToMs <= q.FromMs || q.FromMs >= nowMs {
		return h.sendError(env.ID, ErrorBody{
			Code:      ErrUnknownOp,
			Retryable: false,
			Args:      map[string]any{"t": MsgHistQuery},
		})
	}
	seenSeries := make(map[string]bool, len(q.Series))
	series := make([]string, 0, len(q.Series))
	for _, name := range q.Series {
		badName := name == "" || len(name) > 64
		for _, r := range name {
			if r < 0x20 || r == 0x7f {
				badName = true
				break
			}
		}
		if badName || seenSeries[name] {
			return h.sendError(env.ID, ErrorBody{
				Code:      ErrUnknownOp,
				Retryable: false,
				Args:      map[string]any{"t": MsgHistQuery, "field": "series"},
			})
		}
		// Unknown names stay in the column list and receive MISSING samples.
		// That lets a newer app ask an older box for one additive series without
		// losing the four fields both builds understand.
		seenSeries[name] = true
		series = append(series, name)
	}

	maxPoints := defaultMaxPoints
	if q.MaxPoints != nil && *q.MaxPoints > 0 {
		maxPoints = int(min(*q.MaxPoints, int64(defaultMaxPoints)))
	}

	// History cannot contain future rows, and nothing older than the coarsest
	// store's retention can be served. Clamp before planning so an authenticated
	// but broken client cannot make a loop spanning years of empty tiles.
	toMs := min(q.ToMs, nowMs)
	oldestMs := max(int64(0), nowMs-retentionMsOf(ResN1h))
	fromMs := max(q.FromMs, oldestMs)
	if toMs <= fromMs {
		gaps := []HistGap{{FromMs: q.FromMs, ToMs: min(q.ToMs, oldestMs), Reason: GapEvicted}}
		h.enqueueHistory(historyRequest{
			id: *env.ID, series: series, plan: historyPlan{res: q.Res}, gaps: gaps, nowMs: nowMs,
		})
		return nil
	}

	plan := planHistoryQuery(q.Res, fromMs, toMs, maxPoints)
	retainedFrom := max(int64(0), nowMs-retentionMsOf(plan.res))
	if fromMs < retainedFrom {
		fromMs = retainedFrom
		if toMs <= fromMs {
			gaps := []HistGap{{FromMs: q.FromMs, ToMs: min(q.ToMs, retainedFrom), Reason: GapEvicted}}
			h.enqueueHistory(historyRequest{
				id: *env.ID, series: series, plan: historyPlan{res: plan.res}, gaps: gaps, nowMs: nowMs,
			})
			return nil
		}
		plan = planHistoryQuery(q.Res, fromMs, toMs, maxPoints)
	}

	planned := make(map[string]bool, len(plan.tiles))
	for _, tile := range plan.tiles {
		planned[tile.id] = true
	}
	have := make(map[string]string, min(len(q.Have), len(plan.tiles)))
	for _, ref := range q.Have {
		if planned[ref.TileID] {
			have[ref.TileID] = ref.Etag
		}
	}

	// Ranges the retention window has already eaten are named, not served as
	// silence — the app says "evicted" instead of drawing a suspicious hole.
	var gaps []HistGap
	if q.FromMs < retainedFrom {
		gaps = append(gaps, HistGap{
			FromMs: q.FromMs,
			ToMs:   min(retainedFrom, q.ToMs),
			Reason: GapEvicted,
		})
	}
	if gaps == nil {
		gaps = []HistGap{}
	}

	req := historyRequest{
		id: *env.ID, series: series, plan: plan, have: have, gaps: gaps, nowMs: nowMs,
	}
	h.enqueueHistory(req)
	return nil
}

func (h *Handler) enqueueHistory(req historyRequest) {
	h.histMu.Lock()
	h.histGeneration++
	req.generation = h.histGeneration
	if h.histCancel != nil {
		h.histCancel()
	}
	replaced := h.histPending
	h.histPending = &req
	start := !h.histRunning
	if start {
		h.histRunning = true
	}
	h.histMu.Unlock()

	if replaced != nil {
		_ = h.sendHistoryError(replaced.id, "superseded")
	}
	if start {
		go h.runHistory()
	}
}

func (h *Handler) runHistory() {
	for {
		h.histMu.Lock()
		if h.histPending == nil {
			h.histRunning = false
			h.histCancel = nil
			h.histActive = 0
			h.histMu.Unlock()
			return
		}
		req := *h.histPending
		h.histPending = nil
		if req.generation != h.histGeneration {
			h.histMu.Unlock()
			_ = h.sendHistoryError(req.id, "superseded")
			continue
		}
		ctx, cancel := context.WithTimeout(h.ctx, historyTimeout)
		h.histActive = req.generation
		h.histCancel = cancel
		h.histMu.Unlock()

		err := h.serveHistory(ctx, req)
		cancel()

		h.histMu.Lock()
		superseded := req.generation != h.histGeneration
		if h.histActive == req.generation {
			h.histActive = 0
			h.histCancel = nil
		}
		sessionDone := h.ctx.Err() != nil
		h.histMu.Unlock()

		if sessionDone {
			continue
		}
		switch {
		case superseded && err != nil:
			err = h.sendHistoryError(req.id, "superseded")
		case errors.Is(err, context.DeadlineExceeded):
			err = h.sendHistoryError(req.id, "timeout")
		case err != nil:
			err = h.sendHistoryError(req.id, "provider")
		}
		if err != nil {
			h.log.Warn("history query failed", "err", err)
		}
	}
}

func (h *Handler) sendHistoryError(id uint32, reason string) error {
	if h.ctx.Err() != nil {
		return nil
	}
	return h.sendError(&id, ErrorBody{
		Code:      ErrUnavailable,
		Retryable: ErrorRetryable[ErrUnavailable],
		Args:      map[string]any{"subsystem": "history", "reason": reason},
	})
}

func (h *Handler) serveHistory(ctx context.Context, req historyRequest) error {
	for _, tile := range req.plan.tiles {
		if err := ctx.Err(); err != nil {
			return err
		}
		chunk, err := h.buildTile(ctx, req.series, req.plan, tile, req.nowMs)
		if err != nil {
			return err
		}
		if err := ctx.Err(); err != nil {
			return err
		}
		// A closed tile the app already holds, byte for byte, is not resent.
		// Partial tiles always go: their content moves with the clock, which
		// is exactly why the app never caches them.
		if !chunk.Partial && req.have[chunk.TileID] == chunk.Etag {
			continue
		}
		if err := h.sendBulk(MsgHistChunk, &req.id, chunk); err != nil {
			return err
		}
	}

	if err := ctx.Err(); err != nil {
		return err
	}
	return h.sendBulk(MsgHistEnd, &req.id, HistEnd{ResActual: req.plan.res, Gaps: req.gaps})
}

// buildTile reads the provider and packs one tile: int32 LE columns, one
// block per series, MISSING where the store has no bucket.
func (h *Handler) buildTile(
	ctx context.Context,
	series []string,
	plan historyPlan,
	tile plannedTile,
	nowMs int64,
) (HistChunk, error) {
	stepMs := resolutionStepMs[plan.res]
	tileEnd := tile.startMs + ResolutionTileSpanMs[plan.res]
	points := tile.points

	windows, err := h.cfg.History.Window(ctx, series, stepMs, tile.startMs, tileEnd)
	if err != nil {
		return HistChunk{}, err
	}

	data := make([]byte, int64(len(series))*points*4)
	for s, name := range series {
		samples := windows[name]

		// Stored buckets fold into strided points by plain mean: the ledger's
		// buckets are equal width, so each carries equal weight.
		sums := make([]float64, points)
		counts := make([]int64, points)
		for _, sample := range samples {
			if sample.StartMs < tile.startMs || sample.StartMs >= tileEnd {
				continue
			}
			p := (sample.StartMs - tile.startMs) / (stepMs * plan.stride)
			if p < 0 || p >= points {
				continue
			}
			sums[p] += sample.W
			counts[p]++
		}

		base := int64(s) * points * 4
		for p := int64(0); p < points; p++ {
			v := int32(missingSample)
			if counts[p] > 0 {
				v = clampSample(sums[p] / float64(counts[p]))
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
		TileID:  tile.id,
		Etag:    etagOf(data),
		Res:     plan.res,
		StartMs: tile.startMs,
		StepMs:  plan.stepMs,
		Series:  series,
		Data:    data,
		Partial: tileEnd > nowMs,
	}, nil
}

func clampSample(v float64) int32 {
	r := math.Round(v)
	// One step inside the limits: MinInt32 itself means "missing", and a
	// real reading must never be mistaken for a hole.
	if r >= math.MaxInt32 {
		return math.MaxInt32
	}
	if r <= missingSample+1 {
		return missingSample + 1
	}
	return int32(r)
}
