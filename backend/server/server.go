package server

import (
    "encoding/json"
    "fmt"
    "log"
    "net/http"
    "os"
    "path/filepath"
    "strconv"
    "sync"
    "sync/atomic"
    "time"
    "math/rand"
    "brt08/backend/model"
    "brt08/backend/sim"
    "brt08/backend/data"
)

// connControl holds per-stream tunables.
type connControl struct { speed atomic.Value; arrivalMult atomic.Value }

// Options configures the server instance.
type Options struct {
    PeriodID int
    PassengerCap int
    MorningTowardKivukoni bool
    DirBias float64
    SpatialGradient float64
    BaselineDemand float64
    DefaultSpeed float64
    DefaultArrivalFactor float64
    ReportPath string
}

type Server struct {
    Route *model.Route
    Fleet []*model.Bus
    Opt Options
    streamControls sync.Map // map[string]*connControl
}

func New(route *model.Route, fleet []*model.Bus, opt Options) *Server { return &Server{Route: route, Fleet: fleet, Opt: opt} }

// Serve registers HTTP handlers on default mux.
func (s *Server) Serve() {
    http.HandleFunc("/api/route", func(w http.ResponseWriter, r *http.Request) {
        w.Header().Set("Content-Type", "application/json"); w.Header().Set("Access-Control-Allow-Origin", "*")
        j, _ := json.Marshal(s.Route); w.Write(j)
    })
    http.HandleFunc("/api/control", s.handleControl)
    http.HandleFunc("/api/stream", s.handleStream)
}

func (s *Server) handleControl(w http.ResponseWriter, r *http.Request) {
    w.Header().Set("Access-Control-Allow-Origin", "*"); w.Header().Set("Access-Control-Allow-Headers", "Content-Type")
    if r.Method == http.MethodOptions { w.WriteHeader(204); return }
    if r.Method != http.MethodPost { http.Error(w, "method not allowed", http.StatusMethodNotAllowed); return }
    var req struct { ConnID string `json:"conn_id"`; Speed float64 `json:"speed"`; ArrivalFactor float64 `json:"arrival_factor"` }
    if err := json.NewDecoder(r.Body).Decode(&req); err != nil { http.Error(w, "bad json", 400); return }
    v, ok := s.streamControls.Load(req.ConnID); if !ok { http.Error(w, "connection not found", 404); return }
    c := v.(*connControl)
    if req.Speed != 0 { sp := req.Speed; if sp <= 0 { sp = 1 }; if sp < 0.1 { sp = 0.1 }; if sp > 10.0 { sp = 10.0 }; c.speed.Store(sp) }
    if req.ArrivalFactor != 0 { af := req.ArrivalFactor; if af <= 0 { af = 1 }; if af < 0.1 { af = 0.1 }; if af > 50.0 { af = 50.0 }; c.arrivalMult.Store(af) }
    w.WriteHeader(204)
}

func (s *Server) handleStream(w http.ResponseWriter, r *http.Request) {
    w.Header().Set("Content-Type", "text/event-stream"); w.Header().Set("Cache-Control", "no-cache"); w.Header().Set("Connection", "keep-alive"); w.Header().Set("Access-Control-Allow-Origin", "*")
    flusher, ok := w.(http.Flusher); if !ok { http.Error(w, "stream unsupported", 500); return }

    // Per-connection clones
    baseRNG := rand.New(rand.NewSource(time.Now().UnixNano()))
    connBuses := make([]*model.Bus, 0, len(s.Fleet))
    for _, proto := range s.Fleet { b := &model.Bus{ID: proto.ID, Type: proto.Type, RouteID: proto.RouteID, CurrentStopID: proto.CurrentStopID, Direction: proto.Direction, AverageSpeedKmph: proto.AverageSpeedKmph}; connBuses = append(connBuses, b) }
    start := time.Now()
    lambda := 1.2; if qs := r.URL.Query().Get("lambda"); qs != "" { if v, err := strconv.ParseFloat(qs, 64); err == nil && v > 0 { lambda = v } }
    connID := fmt.Sprintf("%d-%d", time.Now().UnixNano(), rand.Int63())
    ctrl := &connControl{}
    initSpeed := s.Opt.DefaultSpeed; if qs := r.URL.Query().Get("speed"); qs != "" { if v, err := strconv.ParseFloat(qs, 64); err == nil && v > 0 { initSpeed = v } }
    if initSpeed < 0.1 { initSpeed = 0.1 }; if initSpeed > 10.0 { initSpeed = 10.0 }
    ctrl.speed.Store(initSpeed)
    initArr := s.Opt.DefaultArrivalFactor; if qs := r.URL.Query().Get("arrival_factor"); qs != "" { if v, err := strconv.ParseFloat(qs, 64); err == nil && v > 0 { initArr = v } }
    if initArr < 0.1 { initArr = 0.1 }; if initArr > 50.0 { initArr = 50.0 }
    ctrl.arrivalMult.Store(initArr)
    s.streamControls.Store(connID, ctrl); defer s.streamControls.Delete(connID)

    // dummy engine for utilities
    var dummy *model.Bus
    if len(connBuses) > 0 { dummy = &model.Bus{ID:0, Type: connBuses[0].Type, RouteID: s.Route.ID, CurrentStopID: connBuses[0].CurrentStopID, Direction: connBuses[0].Direction, AverageSpeedKmph: connBuses[0].AverageSpeedKmph} } else { bt := &model.BusType{ID:1, Name:"Standard", Capacity:60}; dummy = &model.Bus{ID:0, Type:bt, RouteID:s.Route.ID, CurrentStopID:s.Route.Stops[0].ID, Direction:"outbound", AverageSpeedKmph:28} }
    engine := sim.NewSimulator(s.Route, dummy, time.Now().UnixNano(), lambda, start)
    engine.PeriodID = s.Opt.PeriodID; engine.TotalPassengerCap = s.Opt.PassengerCap; engine.MorningTowardKivukoni = s.Opt.MorningTowardKivukoni; engine.DirectionBiasFactor = s.Opt.DirBias

    // Serialize writer
    var writeMu sync.Mutex
    flush := func(event string, payload any) { writeMu.Lock(); b, _ := json.Marshal(payload); fmt.Fprintf(w, "event: %s\n", event); fmt.Fprintf(w, "data: %s\n\n", b); flusher.Flush(); writeMu.Unlock() }

    var cumServed int64 = 0
    var waitSumMin float64 = 0
    var waitCount int64 = 0

    simSecToReal := 0.2
    waitSim := func(simDur time.Duration) {
        remaining := simDur
        for remaining > 0 { chunk := remaining; if chunk > 500*time.Millisecond { chunk = 500*time.Millisecond }; cur := ctrl.speed.Load().(float64); if cur <= 0 { cur = 1 }; realSleep := time.Duration(float64(chunk) * simSecToReal / cur); time.Sleep(realSleep); remaining -= chunk }
    }
    busDistance := make(map[int]float64)
    var mu sync.Mutex

    stopCh := make(chan struct{}); var stopOnce sync.Once
    isDone := func() bool {
        if s.Opt.PassengerCap <= 0 { return false }
        mu.Lock(); defer mu.Unlock()
        inSystem := 0
        for _, b := range connBuses { inSystem += b.PassengersOnboard }
        for _, st := range s.Route.Stops { inSystem += len(st.OutboundQueue) + len(st.InboundQueue) }
        if int64(s.Opt.PassengerCap) <= cumServed && inSystem == 0 { return true }
        if engine.GeneratedPassengers >= s.Opt.PassengerCap && inSystem == 0 { return true }
        if engine.GeneratedPassengers == int(cumServed) && inSystem == 0 { return true }
        return false
    }
    signalStopIfDone := func() { if isDone() { stopOnce.Do(func(){ close(stopCh) }) } }

    var genWgPtr *sync.WaitGroup
    mult := data.TimePeriodMultiplier[engine.PeriodID]; if mult == 0 { mult = 1 }

    totalTarget := s.Opt.PassengerCap
    initialSeedFraction := 0.05
    seedTarget := 0; if totalTarget > 0 { seedTarget = int(float64(totalTarget)*initialSeedFraction) }
    favOut, favIn := sim.FavoredDirections(engine.PeriodID, s.Opt.MorningTowardKivukoni)
    cfg := sim.DemandConfig{FavoredOutbound: favOut, FavoredInbound: favIn, SpatialGradient: s.Opt.SpatialGradient, BaselineDemand: s.Opt.BaselineDemand, DirBias: s.Opt.DirBias}

    // initial seed
    if seedTarget > 0 { mu.Lock(); sim.SeedInitial(engine, s.Route, start, seedTarget, totalTarget, cfg); mu.Unlock() }
    for _, st := range s.Route.Stops { flush("stop_update", map[string]any{"stop_id": st.ID, "outbound_queue": len(st.OutboundQueue), "inbound_queue": len(st.InboundQueue), "generated_passengers": engine.GeneratedPassengers, "outbound_generated": engine.OutboundGenerated, "inbound_generated": engine.InboundGenerated}) }
    log.Printf("Passenger generator starting: target=%d initial_seed=%d bias_factor=%.2f favored_outbound=%v favored_inbound=%v spatial_gradient=%.2f baseline=%.2f\n", totalTarget, seedTarget, engine.DirectionBiasFactor, favOut, favIn, s.Opt.SpatialGradient, s.Opt.BaselineDemand)

    // generator
    var genWg sync.WaitGroup; genWgPtr = &genWg
    if totalTarget == 0 || engine.GeneratedPassengers < totalTarget {
        genWg.Add(1)
        go func(){ defer genWg.Done(); simStep := 1*time.Second; for { if totalTarget > 0 && engine.GeneratedPassengers >= totalTarget { return }; waitSim(simStep); mu.Lock(); if totalTarget > 0 && engine.GeneratedPassengers >= totalTarget { mu.Unlock(); return }
            stepMin := simStep.Minutes(); arrMult := 1.0; if v := ctrl.arrivalMult.Load(); v != nil { arrMult = v.(float64) }
            mean := lambda * float64(mult) * stepMin * arrMult; count := engine.PoissonPublic(mean); if totalTarget > 0 { remaining := totalTarget - engine.GeneratedPassengers; if remaining < 0 { remaining = 0 }; if count > remaining { count = remaining } }
            if count > 0 { updated := sim.GenerateBatch(engine, s.Route, count, engine.Now, totalTarget, cfg); for sid := range updated { st := s.Route.GetStop(sid); if st != nil { flush("stop_update", map[string]any{"stop_id": sid, "outbound_queue": len(st.OutboundQueue), "inbound_queue": len(st.InboundQueue), "generated_passengers": engine.GeneratedPassengers, "outbound_generated": engine.OutboundGenerated, "inbound_generated": engine.InboundGenerated}) } } }
            mu.Unlock() } }()
    }

    engine.Now = start
    flush("init", map[string]any{"time": engine.Now, "buses": []any{}, "message": "started", "conn_id": connID, "generated_passengers": engine.GeneratedPassengers, "outbound_generated": engine.OutboundGenerated, "inbound_generated": engine.InboundGenerated, "served_passengers": cumServed, "avg_wait_min": 0.0, "arrival_factor": ctrl.arrivalMult.Load().(float64)})

    var wg sync.WaitGroup
    // Simulation inner function copied from main (kept here to keep main thin). For brevity, refer to original for full details.
    computeDwell := func(boardedN, alightedN int) time.Duration { base := 1200*time.Millisecond; per := time.Duration(300*time.Millisecond) * time.Duration(boardedN+alightedN); max := 4*time.Second; d := base + per; if d > max { d = max }; return d }

    simulate := func(bus *model.Bus, forward bool) {
        defer wg.Done()
        dirForward := forward
        for {
            select { case <-stopCh: return; default: }
            if dirForward {
                for idx := 0; idx < len(s.Route.Stops); idx++ {
                    select { case <-stopCh: return; default: }
                    stop := s.Route.Stops[idx]
                    mu.Lock();
                    flush("arrive", map[string]any{"bus_id": bus.ID, "direction": bus.Direction, "stop_id": stop.ID, "outbound_queue": len(stop.OutboundQueue), "inbound_queue": len(stop.InboundQueue), "time": engine.Now, "bus_onboard": bus.PassengersOnboard, "passengers_onboard": bus.PassengersOnboard, "generated_passengers": engine.GeneratedPassengers, "outbound_generated": engine.OutboundGenerated, "inbound_generated": engine.InboundGenerated})
                    alighted := bus.AlightPassengersAtCurrentStop(engine.Now); if len(alighted) > 0 { cumServed += int64(len(alighted)); flush("alight", map[string]any{"bus_id": bus.ID, "direction": bus.Direction, "stop_id": stop.ID, "alighted": len(alighted), "bus_onboard": bus.PassengersOnboard, "passengers_onboard": bus.PassengersOnboard, "generated_passengers": engine.GeneratedPassengers, "outbound_generated": engine.OutboundGenerated, "inbound_generated": engine.InboundGenerated, "served_passengers": cumServed}) }
                    mu.Unlock()
                    waitSim(650*time.Millisecond); mu.Lock(); engine.Now = engine.Now.Add(650*time.Millisecond); mu.Unlock()
                    mu.Lock(); boarded := stop.BoardAtStop(bus, engine.Now); if len(boarded) > 0 { var localSum float64; for _, p := range boarded { if p.WaitDuration != nil { localSum += *p.WaitDuration } }; if localSum > 0 { waitSumMin += localSum; waitCount += int64(len(boarded)) }; avg := 0.0; if waitCount > 0 { avg = waitSumMin / float64(waitCount) }; flush("board", map[string]any{"bus_id": bus.ID, "direction": bus.Direction, "stop_id": stop.ID, "boarded": len(boarded), "bus_onboard": bus.PassengersOnboard, "passengers_onboard": bus.PassengersOnboard, "stop_outbound": len(stop.OutboundQueue), "stop_inbound": len(stop.InboundQueue), "generated_passengers": engine.GeneratedPassengers, "outbound_generated": engine.OutboundGenerated, "inbound_generated": engine.InboundGenerated, "served_passengers": cumServed, "avg_wait_min": avg}) }
                    flush("stop_update", map[string]any{"stop_id": stop.ID, "outbound_queue": len(stop.OutboundQueue), "inbound_queue": len(stop.InboundQueue), "generated_passengers": engine.GeneratedPassengers, "outbound_generated": engine.OutboundGenerated, "inbound_generated": engine.InboundGenerated})
                    dwell := computeDwell(len(boarded), len(alighted)); mu.Unlock(); signalStopIfDone(); waitSim(dwell); mu.Lock(); engine.Now = engine.Now.Add(dwell); mu.Unlock(); signalStopIfDone()
                    if idx == len(s.Route.Stops)-1 { break }
                    next := s.Route.Stops[idx+1]; dist := stop.DistanceToNext; travelMin := dist / bus.AverageSpeedKmph * 60; travelDur := time.Duration(travelMin * float64(time.Minute)); steps := int(travelDur / (800*time.Millisecond)); if steps < 1 { steps = 1 }
                    for sstep := 1; sstep <= steps; sstep++ { t := float64(sstep)/float64(steps); lat := stop.Latitude + (next.Latitude-stop.Latitude)*t; lng := stop.Longitude + (next.Longitude-stop.Longitude)*t; flush("move", map[string]any{"bus_id": bus.ID, "direction": bus.Direction, "lat": lat, "lng": lng, "t": t, "from": stop.ID, "to": next.ID}); stepSim := travelDur / time.Duration(steps); waitSim(stepSim); mu.Lock(); engine.Now = engine.Now.Add(stepSim); mu.Unlock(); select { case <-stopCh: return; default: } }
                    mu.Lock(); busDistance[bus.ID] += dist; mu.Unlock(); bus.CurrentStopID = next.ID
                }
                mu.Lock(); alighted := bus.AlightPassengersAtCurrentStop(engine.Now); if len(alighted) > 0 { cumServed += int64(len(alighted)); flush("alight", map[string]any{"bus_id": bus.ID, "direction": bus.Direction, "stop_id": bus.CurrentStopID, "alighted": len(alighted), "bus_onboard": bus.PassengersOnboard, "passengers_onboard": bus.PassengersOnboard, "generated_passengers": engine.GeneratedPassengers, "final": true, "served_passengers": cumServed}) }; mu.Unlock(); waitSim(3*time.Second); mu.Lock(); engine.Now = engine.Now.Add(3*time.Second); mu.Unlock(); signalStopIfDone(); bus.Direction = "inbound"; dirForward = false
            } else {
                for ridx := len(s.Route.Stops)-1; ridx >= 0; ridx-- {
                    select { case <-stopCh: return; default: }
                    stop := s.Route.Stops[ridx]
                    mu.Lock(); flush("arrive", map[string]any{"bus_id": bus.ID, "direction": bus.Direction, "stop_id": stop.ID, "outbound_queue": len(stop.OutboundQueue), "inbound_queue": len(stop.InboundQueue), "time": engine.Now, "bus_onboard": bus.PassengersOnboard, "passengers_onboard": bus.PassengersOnboard, "generated_passengers": engine.GeneratedPassengers, "outbound_generated": engine.OutboundGenerated, "inbound_generated": engine.InboundGenerated}); alighted := bus.AlightPassengersAtCurrentStop(engine.Now); if len(alighted) > 0 { cumServed += int64(len(alighted)); flush("alight", map[string]any{"bus_id": bus.ID, "direction": bus.Direction, "stop_id": stop.ID, "alighted": len(alighted), "bus_onboard": bus.PassengersOnboard, "passengers_onboard": bus.PassengersOnboard, "generated_passengers": engine.GeneratedPassengers, "outbound_generated": engine.OutboundGenerated, "inbound_generated": engine.InboundGenerated, "served_passengers": cumServed}) }; mu.Unlock();
                    waitSim(650*time.Millisecond); mu.Lock(); engine.Now = engine.Now.Add(650*time.Millisecond); mu.Unlock();
                    mu.Lock(); boarded := stop.BoardAtStop(bus, engine.Now); if len(boarded) > 0 { var localSum2 float64; for _, p := range boarded { if p.WaitDuration != nil { localSum2 += *p.WaitDuration } }; if localSum2 > 0 { waitSumMin += localSum2; waitCount += int64(len(boarded)) }; avg2 := 0.0; if waitCount > 0 { avg2 = waitSumMin / float64(waitCount) }; flush("board", map[string]any{"bus_id": bus.ID, "direction": bus.Direction, "stop_id": stop.ID, "boarded": len(boarded), "bus_onboard": bus.PassengersOnboard, "passengers_onboard": bus.PassengersOnboard, "stop_outbound": len(stop.OutboundQueue), "stop_inbound": len(stop.InboundQueue), "generated_passengers": engine.GeneratedPassengers, "outbound_generated": engine.OutboundGenerated, "inbound_generated": engine.InboundGenerated, "served_passengers": cumServed, "avg_wait_min": avg2}) }
                    flush("stop_update", map[string]any{"stop_id": stop.ID, "outbound_queue": len(stop.OutboundQueue), "inbound_queue": len(stop.InboundQueue), "generated_passengers": engine.GeneratedPassengers, "outbound_generated": engine.OutboundGenerated, "inbound_generated": engine.InboundGenerated})
                    dwell := computeDwell(len(boarded), len(alighted)); mu.Unlock(); signalStopIfDone(); waitSim(dwell); mu.Lock(); engine.Now = engine.Now.Add(dwell); mu.Unlock(); signalStopIfDone()
                    if ridx == 0 { break }
                    prev := s.Route.Stops[ridx-1]; dist := prev.DistanceToNext; travelMin := dist / bus.AverageSpeedKmph * 60; travelDur := time.Duration(travelMin * float64(time.Minute)); steps := int(travelDur / (800*time.Millisecond)); if steps < 1 { steps = 1 }
                    for sstep := 1; sstep <= steps; sstep++ { t := float64(sstep)/float64(steps); lat := stop.Latitude + (prev.Latitude-stop.Latitude)*t; lng := stop.Longitude + (prev.Longitude-stop.Longitude)*t; flush("move", map[string]any{"bus_id": bus.ID, "direction": bus.Direction, "lat": lat, "lng": lng, "t": t, "from": stop.ID, "to": prev.ID}); stepSim := travelDur / time.Duration(steps); waitSim(stepSim); mu.Lock(); engine.Now = engine.Now.Add(stepSim); mu.Unlock(); select { case <-stopCh: return; default: } }
                    mu.Lock(); busDistance[bus.ID] += dist; mu.Unlock(); bus.CurrentStopID = prev.ID
                }
                mu.Lock(); alighted2 := bus.AlightPassengersAtCurrentStop(engine.Now); if len(alighted2) > 0 { cumServed += int64(len(alighted2)); flush("alight", map[string]any{"bus_id": bus.ID, "direction": bus.Direction, "stop_id": bus.CurrentStopID, "alighted": len(alighted2), "bus_onboard": bus.PassengersOnboard, "passengers_onboard": bus.PassengersOnboard, "generated_passengers": engine.GeneratedPassengers, "outbound_generated": engine.OutboundGenerated, "inbound_generated": engine.InboundGenerated, "final": true, "served_passengers": cumServed}) }; mu.Unlock(); waitSim(3*time.Second); mu.Lock(); engine.Now = engine.Now.Add(3*time.Second); mu.Unlock(); signalStopIfDone(); bus.Direction = "outbound"; dirForward = true
            }
        }
    }

    // choose initial directions
    favOut = (engine.PeriodID == 2 && s.Opt.MorningTowardKivukoni) || (engine.PeriodID == 5 && !s.Opt.MorningTowardKivukoni)
    favIn = (engine.PeriodID == 2 && !s.Opt.MorningTowardKivukoni) || (engine.PeriodID == 5 && s.Opt.MorningTowardKivukoni)
    pOutbound := 0.5; if favOut { pOutbound = engine.DirectionBiasFactor / (engine.DirectionBiasFactor + 1.0) } else if favIn { pOutbound = 1.0 / (engine.DirectionBiasFactor + 1.0) }
    for _, b := range connBuses { if baseRNG.Float64() <= pOutbound { b.Direction = "outbound"; b.CurrentStopID = s.Route.Stops[0].ID } else { b.Direction = "inbound"; b.CurrentStopID = s.Route.Stops[len(s.Route.Stops)-1].ID } }

    // schedule
    routeDistance := s.Route.TotalDistanceKM; if routeDistance <= 0 { sum := 0.0; for _, st := range s.Route.Stops { sum += st.DistanceToNext }; if sum > 0 { routeDistance = sum } }
    makeSchedule := func(list []*model.Bus) []struct{ bus *model.Bus; simDelay time.Duration } {
        n := len(list); if n == 0 { return nil }
        var avgV float64; for _, b := range list { avgV += b.AverageSpeedKmph }; avgV /= float64(n); if avgV <= 0 { avgV = 25 }
        tripMin := routeDistance / avgV * 60.0; headwayMin := tripMin / float64(n); if headwayMin < 0.5 { headwayMin = 0.5 }; if headwayMin > 15 { headwayMin = 15 }
        sched := make([]struct{ bus *model.Bus; simDelay time.Duration }, 0, n)
        for i, b := range list { base := float64(i) * headwayMin; jitter := (baseRNG.Float64()*0.4 - 0.2) * headwayMin; simOffsetMin := base + jitter; if simOffsetMin < 0 { simOffsetMin = 0 }; simDelay := time.Duration(simOffsetMin * float64(time.Minute)); sched = append(sched, struct{ bus *model.Bus; simDelay time.Duration }{bus: b, simDelay: simDelay}) }
        return sched
    }
    busesOutbound := make([]*model.Bus, 0); busesInbound := make([]*model.Bus, 0); for _, b := range connBuses { if b.Direction == "inbound" { busesInbound = append(busesInbound, b) } else { busesOutbound = append(busesOutbound, b) } }
    schedule := append(makeSchedule(busesOutbound), makeSchedule(busesInbound)...)
    for _, item := range schedule { bus := item.bus; forward := bus.Direction == "outbound"; wg.Add(1); go func(bu *model.Bus, fwd bool, simD time.Duration){ waitSim(simD); cap := 0; if bu.Type != nil { cap = bu.Type.Capacity }; flush("bus_add", map[string]any{"bus_id": bu.ID, "direction": bu.Direction, "avg_speed_kmph": bu.AverageSpeedKmph, "capacity": cap}); log.Printf("Bus %d added to route (%s), avg_speed=%.1f km/h", bu.ID, bu.Direction, bu.AverageSpeedKmph); var lat, lng float64; if bu.Direction == "inbound" { lat = s.Route.Stops[len(s.Route.Stops)-1].Latitude; lng = s.Route.Stops[len(s.Route.Stops)-1].Longitude } else { lat = s.Route.Stops[0].Latitude; lng = s.Route.Stops[0].Longitude }; flush("move", map[string]any{"bus_id": bu.ID, "direction": bu.Direction, "lat": lat, "lng": lng, "from": 0, "to": bu.CurrentStopID, "t": 0}); simulate(bu, fwd) }(bus, forward, item.simDelay) }

    wg.Wait(); if genWgPtr != nil && s.Opt.PassengerCap > 0 { genWgPtr.Wait() }

    // reposition (same as in main)
    repositionStart := time.Now()
    if s.Opt.PassengerCap > 0 {
        layoverIdxSet := make(map[int]struct{}); for i, st := range s.Route.Stops { if st.AllowLayover { layoverIdxSet[i] = struct{}{} } }; layoverIdxSet[0] = struct{}{}; layoverIdxSet[len(s.Route.Stops)-1] = struct{}{}
        layoverIdxs := make([]int, 0, len(layoverIdxSet)); for idx := range layoverIdxSet { layoverIdxs = append(layoverIdxs, idx) }
        flush("reposition_start", map[string]any{"buses": len(connBuses), "layover_indices": layoverIdxs})
        var repWg sync.WaitGroup; repWg.Add(len(connBuses))
        for _, b := range connBuses { bus := b; go func(){ defer repWg.Done(); curIdx := -1; for i, st := range s.Route.Stops { if st.ID == bus.CurrentStopID { curIdx = i; break } }; if curIdx == -1 { return }
            forward := (bus.Direction == "outbound"); bestIdx := -1; bestDist := 1<<30; for _, li := range layoverIdxs { if forward && li > curIdx { d := li - curIdx; if d < bestDist { bestDist = d; bestIdx = li } }; if !forward && li < curIdx { d := curIdx - li; if d < bestDist { bestDist = d; bestIdx = li } } }; aheadFound := (bestIdx != -1); if !aheadFound { bestDist = 1<<30; for _, li := range layoverIdxs { d := li - curIdx; if d < 0 { d = -d }; if d < bestDist { bestDist = d; bestIdx = li } } }
            flush("reposition_bus", map[string]any{"bus_id": bus.ID, "from_index": curIdx, "target_index": bestIdx, "current_stop_id": s.Route.Stops[curIdx].ID, "ahead_only": aheadFound}); if bestIdx == -1 || bestIdx == curIdx { flush("layover", map[string]any{"bus_id": bus.ID, "terminal_stop_id": s.Route.Stops[curIdx].ID}); return }
            step := 1; if bestIdx < curIdx { step = -1 }
            for idx := curIdx; idx != bestIdx; idx += step { from := s.Route.Stops[idx]; to := s.Route.Stops[idx+step]; dist := from.DistanceToNext; if step == -1 { prev := s.Route.Stops[idx-1]; dist = prev.DistanceToNext }
                travelMin := dist / bus.AverageSpeedKmph * 60; if travelMin < 0 { travelMin = 0 }; travelDur := time.Duration(travelMin * float64(time.Minute)); steps := int(travelDur / (800*time.Millisecond)); if steps < 1 { steps = 1 }
                for sstep := 1; sstep <= steps; sstep++ { t := float64(sstep)/float64(steps); lat := from.Latitude + (to.Latitude-from.Latitude)*t; lng := from.Longitude + (to.Longitude-from.Longitude)*t; flush("move", map[string]any{"bus_id": bus.ID, "direction": bus.Direction, "lat": lat, "lng": lng, "t": t, "from": from.ID, "to": to.ID, "phase": "reposition"}); stepSim := travelDur / time.Duration(steps); waitSim(stepSim); mu.Lock(); engine.Now = engine.Now.Add(stepSim); busDistance[bus.ID] += dist/float64(steps); mu.Unlock() }
                bus.CurrentStopID = to.ID }
            flush("layover", map[string]any{"bus_id": bus.ID, "terminal_stop_id": s.Route.Stops[bestIdx].ID}) }() }
        repWg.Wait(); flush("reposition_complete", map[string]any{"elapsed_ms": time.Since(repositionStart).Milliseconds()})
    }

    avgFinal := 0.0; if waitCount > 0 { avgFinal = waitSumMin / float64(waitCount) }
    if s.Opt.PassengerCap > 0 && engine.GeneratedPassengers > s.Opt.PassengerCap { engine.GeneratedPassengers = s.Opt.PassengerCap }
    flush("done", map[string]any{"completed": true, "generated_passengers": engine.GeneratedPassengers, "outbound_generated": engine.OutboundGenerated, "inbound_generated": engine.InboundGenerated, "served_passengers": cumServed, "avg_wait_min": avgFinal})

    if s.Opt.ReportPath != "" {
        ts := time.Now().Format("20060102-150405"); outPath := s.Opt.ReportPath
        if fi, err := os.Stat(outPath); err == nil && fi.IsDir() { outPath = filepath.Join(outPath, fmt.Sprintf("report-%s.csv", ts)) } else if outPath != "" { ext := filepath.Ext(outPath); base := outPath[:len(outPath)-len(ext)]; outPath = fmt.Sprintf("%s-%s%s", base, ts, ext) }
        f, err := os.Create(outPath); if err != nil { log.Printf("report: create failed: %v", err) } else { defer f.Close(); fmt.Fprintln(f, "section,bus_id,direction,type,avg_speed_kmph,distance_km,cost,generated,served,avg_wait_min,buses_count,timestamp"); 
            for _, b := range connBuses { d := busDistance[b.ID]; c := 0.0; typeName := ""; if b.Type != nil { c = float64(b.Type.CostPerKm) * d; typeName = b.Type.Name }; fmt.Fprintf(f, "bus,%d,%s,%s,%.1f,%.3f,%.2f,,,,,%s\n", b.ID, b.Direction, typeName, b.AverageSpeedKmph, d, c, ts) }
            totalCost := 0.0; for _, b := range connBuses { if b.Type != nil { totalCost += float64(b.Type.CostPerKm) * busDistance[b.ID] } }
            fmt.Fprintf(f, "summary,,,,,,%.2f,%d,%d,%.2f,%d,%s\n", totalCost, engine.GeneratedPassengers, cumServed, avgFinal, len(connBuses), ts); log.Printf("CSV report written to %s", outPath) }
    }

    // Console report
    totalCost := 0.0; totalDist := 0.0; fmt.Println("=== Simulation Report ==="); fmt.Printf("Buses on route: %d\n", len(connBuses)); fmt.Printf("Passengers generated: %d\n", engine.GeneratedPassengers); fmt.Printf("Passengers served: %d\n", cumServed); fmt.Printf("Average wait: %.2f minutes\n", avgFinal)
    for _, b := range connBuses { d := busDistance[b.ID]; c := 0.0; if b.Type != nil { c = float64(b.Type.CostPerKm) * d }; totalDist += d; totalCost += c; name := ""; if b.Type != nil { name = b.Type.Name }; fmt.Printf("Bus %d (%s, %s) distance=%.2f km cost=%.2f\n", b.ID, b.Direction, name, d, c) }
    fmt.Printf("Total distance: %.2f km\n", totalDist); fmt.Printf("Total operating cost: %.2f\n", totalCost)
}
