package sim

import (
    "time"
    "brt08/backend/model"
)

// DemandConfig encapsulates parameters that shape passenger generation.
type DemandConfig struct {
    FavoredOutbound bool
    FavoredInbound  bool
    SpatialGradient float64
    BaselineDemand  float64
    DirBias         float64
}

// FavoredDirections computes favored directions for a given period and morning flag.
func FavoredDirections(periodID int, morningTowardKivukoni bool) (bool, bool) {
    favOut := (periodID == 2 && morningTowardKivukoni) || (periodID == 5 && !morningTowardKivukoni)
    favIn := (periodID == 2 && !morningTowardKivukoni) || (periodID == 5 && morningTowardKivukoni)
    return favOut, favIn
}

// gradient weights for spatial concentration along the corridor.
func gradientWeightOutbound(i, n int, spatialGradient, baseline, dirBias float64, favoredOutbound bool) float64 {
    if spatialGradient <= 0 { return 1.0 }
    if !favoredOutbound { return 1.0 / dirBias }
    if n <= 1 { return 1.0 }
    pos := float64(i)
    norm := 1.0 - pos/float64(n-1) // 1 at origin tapering to 0
    if norm < 0 { norm = 0 }
    if norm > 1 { norm = 1 }
    if baseline < 0 { baseline = 0 }
    if baseline > 1 { baseline = 1 }
    return baseline + spatialGradient*norm
}

func gradientWeightInbound(i, n int, spatialGradient, baseline, dirBias float64, favoredInbound bool) float64 {
    if spatialGradient <= 0 { return 1.0 }
    if !favoredInbound { return 1.0 / dirBias }
    if n <= 1 { return 1.0 }
    // favored origin is last stop index (n-1)
    pos := float64((n-1) - i)
    norm := 1.0 - pos/float64(n-1)
    if norm < 0 { norm = 0 }
    if norm > 1 { norm = 1 }
    if baseline < 0 { baseline = 0 }
    if baseline > 1 { baseline = 1 }
    return baseline + spatialGradient*norm
}

// SeedInitial populates a small number of initial passengers before streaming; returns how many seeded.
// Caller must ensure synchronization as this mutates route queues and engine counters.
func SeedInitial(engine *Simulator, route *model.Route, start time.Time, seedTarget, totalTarget int, cfg DemandConfig) int {
    seeded := 0
    if seedTarget <= 0 { return 0 }
    nStops := len(route.Stops)
    for engine.GeneratedPassengers < seedTarget && (totalTarget == 0 || engine.GeneratedPassengers < totalTarget) {
        // Direction choice with bias
        dir := "outbound"
        pOutbound := 0.5
        if cfg.FavoredOutbound { pOutbound = cfg.DirBias / (cfg.DirBias + 1.0) } else if cfg.FavoredInbound { pOutbound = 1.0 / (cfg.DirBias + 1.0) }
        if engine.RNG.Float64() >= pOutbound { dir = "inbound" }
        if dir == "outbound" {
            weights := make([]float64, nStops-1)
            sum := 0.0
            for i := 0; i < nStops-1; i++ { w := gradientWeightOutbound(i, nStops, cfg.SpatialGradient, cfg.BaselineDemand, cfg.DirBias, cfg.FavoredOutbound); weights[i] = w; sum += w }
            r := engine.RNG.Float64()*sum
            cum := 0.0
            originIdx := 0
            for i, w := range weights { cum += w; if r <= cum { originIdx = i; break } }
            destIdx := originIdx + 1 + engine.RNG.Intn(nStops-originIdx-1)
            origin := route.Stops[originIdx]
            dest := route.Stops[destIdx]
            arrTime := start.Add(-time.Duration(engine.RNG.Float64()*2*float64(time.Minute)))
            p := engine.NewPassengerPublic(origin.ID, dest.ID, arrTime)
            p.Direction = "outbound"
            origin.EnqueuePassenger(p, "outbound", arrTime)
            engine.GeneratedPassengers++; engine.OutboundGenerated++
            seeded++
        } else {
            weights := make([]float64, nStops-1)
            sum := 0.0
            for i := 1; i < nStops; i++ { w := gradientWeightInbound(i, nStops, cfg.SpatialGradient, cfg.BaselineDemand, cfg.DirBias, cfg.FavoredInbound); weights[i-1] = w; sum += w }
            r := engine.RNG.Float64()*sum
            cum := 0.0
            originIdxGlobal := 1
            for k, w := range weights { cum += w; if r <= cum { originIdxGlobal = k+1; break } }
            destIdx := engine.RNG.Intn(originIdxGlobal)
            origin := route.Stops[originIdxGlobal]
            dest := route.Stops[destIdx]
            arrTime := start.Add(-time.Duration(engine.RNG.Float64()*2*float64(time.Minute)))
            p := engine.NewPassengerPublic(origin.ID, dest.ID, arrTime)
            p.Direction = "inbound"
            origin.EnqueuePassenger(p, "inbound", arrTime)
            engine.GeneratedPassengers++; engine.InboundGenerated++
            seeded++
        }
    }
    return seeded
}

// GenerateBatch creates up to 'count' passengers according to cfg and returns set of updated stop IDs.
// Caller must ensure synchronization.
func GenerateBatch(engine *Simulator, route *model.Route, count int, now time.Time, totalTarget int, cfg DemandConfig) map[int]struct{} {
    updatedStops := make(map[int]struct{})
    if count <= 0 { return updatedStops }
    nStops := len(route.Stops)
    pOutbound := 0.5
    if cfg.FavoredOutbound { pOutbound = cfg.DirBias / (cfg.DirBias + 1.0) } else if cfg.FavoredInbound { pOutbound = 1.0 / (cfg.DirBias + 1.0) }
    for i := 0; i < count; i++ {
        if totalTarget > 0 && engine.GeneratedPassengers >= totalTarget { break }
        dir := "outbound"
        if engine.RNG.Float64() >= pOutbound { dir = "inbound" }
        if dir == "outbound" {
            weights := make([]float64, nStops-1)
            sum := 0.0
            for si := 0; si < nStops-1; si++ { w := gradientWeightOutbound(si, nStops, cfg.SpatialGradient, cfg.BaselineDemand, cfg.DirBias, cfg.FavoredOutbound); weights[si] = w; sum += w }
            r := engine.RNG.Float64()*sum
            cum := 0.0
            originIdx := 0
            for si, w := range weights { cum += w; if r <= cum { originIdx = si; break } }
            destIdx := originIdx + 1 + engine.RNG.Intn(nStops-originIdx-1)
            origin := route.Stops[originIdx]
            dest := route.Stops[destIdx]
            p := engine.NewPassengerPublic(origin.ID, dest.ID, now)
            p.Direction = "outbound"
            origin.EnqueuePassenger(p, "outbound", now)
            engine.GeneratedPassengers++; engine.OutboundGenerated++
            updatedStops[origin.ID] = struct{}{}
        } else {
            weights := make([]float64, nStops-1)
            sum := 0.0
            for si := 1; si < nStops; si++ { w := gradientWeightInbound(si, nStops, cfg.SpatialGradient, cfg.BaselineDemand, cfg.DirBias, cfg.FavoredInbound); weights[si-1] = w; sum += w }
            r := engine.RNG.Float64()*sum
            cum := 0.0
            originIdxGlobal := 1
            for k, w := range weights { cum += w; if r <= cum { originIdxGlobal = k+1; break } }
            destIdx := engine.RNG.Intn(originIdxGlobal)
            origin := route.Stops[originIdxGlobal]
            dest := route.Stops[destIdx]
            p := engine.NewPassengerPublic(origin.ID, dest.ID, now)
            p.Direction = "inbound"
            origin.EnqueuePassenger(p, "inbound", now)
            engine.GeneratedPassengers++; engine.InboundGenerated++
            updatedStops[origin.ID] = struct{}{}
        }
    }
    return updatedStops
}
