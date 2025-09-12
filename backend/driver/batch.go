package driver

import (
	"brt08/backend/data"
	"brt08/backend/model"
	"brt08/backend/sim"
	"container/heap"
	"fmt"
	"log"
	"math"
	"math/rand"
	"os"
	"path/filepath"
	"time"
)

// Options mirrors server.Options for reuse in headless mode.
type Options struct {
	PeriodID              int
	PassengerCap          int
	MorningTowardKivukoni bool
	DirBias               float64
	SpatialGradient       float64
	BaselineDemand        float64
	ArrivalFactor         float64
	ReportPath            string
	Seed                  int64
	Trace                 bool
	TraceBusID            int
}

type Summary struct {
	Generated     int
	Served        int64
	AvgWaitMin    float64
	BusDistance   map[int]float64
	TotalDistance float64
	TotalCost     float64
}

// Timing constants mirrored from SSE to ensure identical semantics.
// In batch mode these only affect simulated time progression (no real sleeps).
const (
	preBoardPause = 650 * time.Millisecond
	travelStep    = 800 * time.Millisecond
	terminalPause = 3 * time.Second
)

// Internal event and priority queue for bus arrivals (package scope for Go method declarations)
type evt struct {
	t       time.Time
	bus     *model.Bus
	stopIdx int
}

type eventPQ []evt

func (p eventPQ) Len() int           { return len(p) }
func (p eventPQ) Less(i, j int) bool { return p[i].t.Before(p[j].t) }
func (p eventPQ) Swap(i, j int)      { p[i], p[j] = p[j], p[i] }
func (p *eventPQ) Push(x any)        { *p = append(*p, x.(evt)) }
func (p *eventPQ) Pop() any          { old := *p; n := len(old); v := old[n-1]; *p = old[:n-1]; return v }

func clampFactor(v float64) float64 {
	if v < 0.1 {
		return 0.1
	}
	if v > 50.0 {
		return 50.0
	}
	return v
}

// Run executes a fast, headless simulation (no SSE, no sleeps) and returns a summary.
// Notes:
// - Requires PassengerCap > 0; generates all passengers upfront using current demand config.
// - Buses start immediately at their terminal and operate until all passengers are served.
// Run mirrors the SSE simulation logic exactly, but executes in fast-forward (no sleeps, no SSE output).
// Only difference from SSE is wall-clock time (this is fast), not simulation results.
func Run(route *model.Route, fleet []*model.Bus, opt Options) (Summary, error) {
	if route == nil || len(route.Stops) == 0 {
		return Summary{}, fmt.Errorf("route not loaded")
	}
	if opt.PassengerCap <= 0 {
		return Summary{}, fmt.Errorf("batch driver requires -passenger_cap > 0")
	}

	// Clone fleet to avoid mutating caller's instances
	buses := make([]*model.Bus, 0, len(fleet))
	for _, b := range fleet {
		if b == nil {
			continue
		}
		copy := &model.Bus{ID: b.ID, Type: b.Type, RouteID: b.RouteID, CurrentStopID: b.CurrentStopID, Direction: b.Direction, AverageSpeedKmph: b.AverageSpeedKmph}
		buses = append(buses, copy)
	}
	if len(buses) == 0 {
		// fallback default two buses
		bt := &model.BusType{ID: 1, Name: "Standard 12m", Capacity: 70, CostPerKm: 1.75}
		buses = []*model.Bus{
			{ID: 1, Type: bt, RouteID: route.ID, CurrentStopID: route.Stops[0].ID, Direction: "outbound", AverageSpeedKmph: 28.0},
			{ID: 2, Type: bt, RouteID: route.ID, CurrentStopID: route.Stops[len(route.Stops)-1].ID, Direction: "inbound", AverageSpeedKmph: 28.0},
		}
	}

	start := time.Now()
	baseSeed := opt.Seed
	if baseSeed == 0 {
		baseSeed = time.Now().UnixNano()
	}
	baseRNG := rand.New(rand.NewSource(baseSeed))
	lambda := 1.2 // base arrivals per corridor per minute (same default as SSE)
	// Dummy bus for simulator
	dummy := &model.Bus{ID: 0, Type: buses[0].Type, RouteID: route.ID, CurrentStopID: buses[0].CurrentStopID, Direction: buses[0].Direction, AverageSpeedKmph: buses[0].AverageSpeedKmph}
	engine := sim.NewSimulator(route, dummy, baseSeed+1, lambda, start)
	engine.PeriodID = opt.PeriodID
	engine.TotalPassengerCap = opt.PassengerCap
	engine.MorningTowardKivukoni = opt.MorningTowardKivukoni
	engine.DirectionBiasFactor = opt.DirBias
	engine.Now = start

	// Assign initial directions
	favOut, favIn := sim.FavoredDirections(engine.PeriodID, opt.MorningTowardKivukoni)
	pOutbound := 0.5
	if favOut {
		pOutbound = engine.DirectionBiasFactor / (engine.DirectionBiasFactor + 1.0)
	} else if favIn {
		pOutbound = 1.0 / (engine.DirectionBiasFactor + 1.0)
	}
	for _, b := range buses {
		if baseRNG.Float64() <= pOutbound {
			b.Direction = "outbound"
			b.CurrentStopID = route.Stops[0].ID
		} else {
			b.Direction = "inbound"
			b.CurrentStopID = route.Stops[len(route.Stops)-1].ID
		}
	}

	// Demand configuration
	cfg := sim.DemandConfig{FavoredOutbound: favOut, FavoredInbound: favIn, SpatialGradient: opt.SpatialGradient, BaselineDemand: opt.BaselineDemand, DirBias: opt.DirBias}
	mult := data.TimePeriodMultiplier[engine.PeriodID]
	if mult == 0 {
		mult = 1
	}

	// Initial seed (5% of cap)
	totalTarget := opt.PassengerCap
	seedTarget := 0
	if totalTarget > 0 {
		seedTarget = int(float64(totalTarget) * 0.05)
	}
	if seedTarget > 0 {
		sim.SeedInitial(engine, route, start, seedTarget, totalTarget, cfg)
	}

	// Stats
	var cumServed int64
	var waitSumMin float64
	var waitCount int64
	busDistance := make(map[int]float64)
	// Helper to compute in-system passengers and stop condition like SSE
	inSystemCount := func() int {
		inSystem := 0
		for _, b := range buses {
			inSystem += b.PassengersOnboard
		}
		for _, s := range route.Stops {
			inSystem += len(s.OutboundQueue) + len(s.InboundQueue)
		}
		return inSystem
	}
	isDone := func() bool {
		if opt.PassengerCap <= 0 {
			return false
		}
		inSystem := inSystemCount()
		if int64(opt.PassengerCap) <= cumServed && inSystem == 0 {
			return true
		}
		if engine.GeneratedPassengers >= opt.PassengerCap && inSystem == 0 {
			return true
		}
		if engine.GeneratedPassengers == int(cumServed) && inSystem == 0 {
			return true
		}
		return false
	}

	computeDwell := func(boardedN, alightedN int) time.Duration {
		// Same as SSE computeDwell
		base := 1200 * time.Millisecond
		per := time.Duration(300*time.Millisecond) * time.Duration(boardedN+alightedN)
		max := 4 * time.Second
		d := base + per
		if d > max {
			d = max
		}
		return d
	}

	// Helper to get stop by id and its index
	getIdx := func(stopID int) int {
		for i, s := range route.Stops {
			if s.ID == stopID {
				return i
			}
		}
		return -1
	}

	// Initialize positions if needed
	for _, b := range buses {
		if getIdx(b.CurrentStopID) == -1 {
			if b.Direction == "outbound" {
				b.CurrentStopID = route.Stops[0].ID
			} else {
				b.CurrentStopID = route.Stops[len(route.Stops)-1].ID
			}
		}
	}

	// Scheduling identical to SSE: compute headways per direction with jitter
	routeDistance := route.TotalDistanceKM
	if routeDistance <= 0 {
		sum := 0.0
		for _, s := range route.Stops {
			sum += s.DistanceToNext
		}
		if sum > 0 {
			routeDistance = sum
		}
	}
	busesOutbound := make([]*model.Bus, 0)
	busesInbound := make([]*model.Bus, 0)
	for _, b := range buses {
		if b.Direction == "inbound" {
			busesInbound = append(busesInbound, b)
		} else {
			busesOutbound = append(busesOutbound, b)
		}
	}
	makeSchedule := func(list []*model.Bus) []struct {
		bus      *model.Bus
		simDelay time.Duration
	} {
		n := len(list)
		if n == 0 {
			return nil
		}
		var avgV float64
		for _, b := range list {
			avgV += b.AverageSpeedKmph
		}
		avgV /= float64(n)
		if avgV <= 0 {
			avgV = 25
		}
		tripMin := routeDistance / avgV * 60.0
		headwayMin := tripMin / float64(n)
		if headwayMin < 0.5 {
			headwayMin = 0.5
		}
		if headwayMin > 15 {
			headwayMin = 15
		}
		sched := make([]struct {
			bus      *model.Bus
			simDelay time.Duration
		}, 0, n)
		for i, b := range list {
			base := float64(i) * headwayMin
			jitter := (baseRNG.Float64()*0.4 - 0.2) * headwayMin
			simOffsetMin := base + jitter
			if simOffsetMin < 0 {
				simOffsetMin = 0
			}
			sched = append(sched, struct {
				bus      *model.Bus
				simDelay time.Duration
			}{bus: b, simDelay: time.Duration(simOffsetMin * float64(time.Minute))})
		}
		return sched
	}
	schedule := append(makeSchedule(busesOutbound), makeSchedule(busesInbound)...)

	// Priority queue of bus arrival events
	q := &eventPQ{}
	heap.Init(q)
	// Seed initial arrival events according to schedule
	for _, it := range schedule {
		b := it.bus
		idx := getIdx(b.CurrentStopID)
		if idx < 0 {
			if b.Direction == "outbound" {
				idx = 0
			} else {
				idx = len(route.Stops) - 1
			}
		}
		heap.Push(q, evt{t: start.Add(it.simDelay), bus: b, stopIdx: idx})
	}

	// Passenger generator: advance in 1s steps up to target time (no sleeps)
	lastGen := start
	advanceGenTo := func(t time.Time) {
		if engine.TotalPassengerCap > 0 && engine.GeneratedPassengers >= engine.TotalPassengerCap {
			lastGen = t
			return
		}
		for lastGen.Before(t) {
			step := lastGen.Add(1 * time.Second)
			if step.After(t) {
				step = t
			}
			stepMin := step.Sub(lastGen).Minutes()
			mean := lambda * float64(mult) * stepMin * clampFactor(opt.ArrivalFactor)
			count := engine.PoissonPublic(mean)
			if engine.TotalPassengerCap > 0 {
				remain := engine.TotalPassengerCap - engine.GeneratedPassengers
				if remain < 0 {
					remain = 0
				}
				if count > remain {
					count = remain
				}
			}
			if count > 0 {
				updated := sim.GenerateBatch(engine, route, count, lastGen, engine.TotalPassengerCap, cfg)
				if opt.Trace {
					fmt.Printf("[trace] gen t=%s +%d stops=%d total=%d\n", step.Format(time.RFC3339Nano), count, len(updated), engine.GeneratedPassengers)
				}
			}
			lastGen = step
		}
	}

	// Track last visited stop index per bus (for accurate reposition start)
	lastIdx := make(map[int]int)

	// Event loop
	for q.Len() > 0 {
		ev := heap.Pop(q).(evt)
		// Generate passengers up to this event time
		if ev.t.After(lastGen) {
			advanceGenTo(ev.t)
		}
		// Advance simulation time
		engine.Now = ev.t
		bus := ev.bus
		idx := ev.stopIdx
		st := route.Stops[idx]
		lastIdx[bus.ID] = idx
		if opt.TraceBusID > 0 && opt.TraceBusID == bus.ID {
			nextIdx := idx
			if bus.Direction == "outbound" {
				if idx < len(route.Stops)-1 {
					nextIdx = idx + 1
				}
			} else {
				if idx > 0 {
					nextIdx = idx - 1
				}
			}
			fmt.Printf("buslog bus=%d stop_idx=%d next_idx=%d stop_id=%d dist_km=%.2f\n", bus.ID, idx, nextIdx, st.ID, math.Round(busDistance[bus.ID]*100)/100)
		}
		// Arrive: alight
		alighted := bus.AlightPassengersAtCurrentStop(engine.Now)
		if len(alighted) > 0 {
			cumServed += int64(len(alighted))
		}
		// Short pause before boarding (same as SSE preBoardPause)
		boardTime := engine.Now.Add(preBoardPause)
		if boardTime.After(lastGen) {
			advanceGenTo(boardTime)
		}
		engine.Now = boardTime
		// Board
		boarded := st.BoardAtStop(bus, engine.Now)
		if len(boarded) > 0 {
			var localSum float64
			for _, p := range boarded {
				if p.WaitDuration != nil {
					localSum += *p.WaitDuration
				}
			}
			if localSum > 0 {
				waitSumMin += localSum
				waitCount += int64(len(boarded))
			}
		}
		// quiet board trace
		dwell := computeDwell(len(boarded), len(alighted))
		depart := engine.Now.Add(dwell)
		if depart.After(lastGen) {
			advanceGenTo(depart)
		}
		engine.Now = depart
		// quiet dwell trace
		if isDone() {
			break
		}
		// Move to next (chunked with mid-segment termination like SSE)
		if bus.Direction == "outbound" {
			if idx == len(route.Stops)-1 {
				// terminal pause then flip (matches SSE terminal handling)
				turn := engine.Now.Add(terminalPause)
				if turn.After(lastGen) {
					advanceGenTo(turn)
				}
				engine.Now = turn
				bus.Direction = "inbound"
				if opt.TraceBusID > 0 && opt.TraceBusID == bus.ID {
					fmt.Printf("[trace] terminal_flip t=%s bus=%d new_dir=%s\n", engine.Now.Format(time.RFC3339Nano), bus.ID, bus.Direction)
				}
				// schedule next arrival at same terminal index (start inbound) immediately
				if isDone() {
					// Generate passengers up to this event time
				}
				heap.Push(q, evt{t: engine.Now, bus: bus, stopIdx: idx})
			} else {
				next := route.Stops[idx+1]
				dist := st.DistanceToNext
				travelMin := dist / bus.AverageSpeedKmph * 60
				travelDur := time.Duration(travelMin * float64(time.Minute))
				steps := int(travelDur / travelStep)
				if steps < 1 {
					steps = 1
				}
				stepDur := travelDur / time.Duration(steps)
				completed := true
				for sstep := 0; sstep < steps; sstep++ {
					t := engine.Now.Add(stepDur)
					if t.After(lastGen) {
						advanceGenTo(t)
					}
					engine.Now = t
					// quiet move trace
					if isDone() {
						completed = false
						break
					}
				}
				if completed {
					busDistance[bus.ID] += dist
					bus.CurrentStopID = next.ID
					heap.Push(q, evt{t: engine.Now, bus: bus, stopIdx: idx + 1})
				}
			}
		} else {
			if idx == 0 {
				turn := engine.Now.Add(terminalPause)
				if turn.After(lastGen) {
					advanceGenTo(turn)
				}
				engine.Now = turn
				bus.Direction = "outbound"
				if opt.TraceBusID > 0 && opt.TraceBusID == bus.ID {
					fmt.Printf("[trace] terminal_flip t=%s bus=%d new_dir=%s\n", engine.Now.Format(time.RFC3339Nano), bus.ID, bus.Direction)
				}
				if isDone() {
					break
				}
				heap.Push(q, evt{t: engine.Now, bus: bus, stopIdx: idx})
			} else {
				prev := route.Stops[idx-1]
				dist := route.Stops[idx-1].DistanceToNext
				travelMin := dist / bus.AverageSpeedKmph * 60
				travelDur := time.Duration(travelMin * float64(time.Minute))
				steps := int(travelDur / travelStep)
				if steps < 1 {
					steps = 1
				}
				stepDur := travelDur / time.Duration(steps)
				completed := true
				for sstep := 0; sstep < steps; sstep++ {
					t := engine.Now.Add(stepDur)
					if t.After(lastGen) {
						advanceGenTo(t)
					}
					engine.Now = t
					// quiet move trace
					if isDone() {
						completed = false
						break
					}
				}
				if completed {
					busDistance[bus.ID] += dist
					bus.CurrentStopID = prev.ID
					heap.Push(q, evt{t: engine.Now, bus: bus, stopIdx: idx - 1})
				}
			}
		}
		// stop condition (post event boundary)
		if isDone() {
			break
		}
	}

	// Reposition (layover) phase: direction-aware to nearest allowed layover ahead; add distances; update engine.Now monotonically
	layoverIdxSet := make(map[int]struct{})
	for i, s := range route.Stops {
		if s.AllowLayover {
			layoverIdxSet[i] = struct{}{}
		}
	}
	layoverIdxSet[0] = struct{}{}
	layoverIdxSet[len(route.Stops)-1] = struct{}{}
	layoverIdxs := make([]int, 0, len(layoverIdxSet))
	for i := range layoverIdxSet {
		layoverIdxs = append(layoverIdxs, i)
	}

	idxOf := func(stopID int) int {
		for i, s := range route.Stops {
			if s.ID == stopID {
				return i
			}
		}
		return -1
	}
	// helper: km path distance between stop indices (inclusive-exclusive on segments)
	kmBetweenIdx := func(i, j int) float64 {
		if i == j {
			return 0
		}
		d := 0.0
		if j > i {
			// forward along increasing indices
			for k := i; k < j; k++ {
				d += route.Stops[k].DistanceToNext
			}
		} else {
			// backward along decreasing indices
			for k := i; k > j; k-- {
				d += route.Stops[k-1].DistanceToNext
			}
		}
		return d
	}

	for _, bus := range buses {
		curIdx, ok := lastIdx[bus.ID]
		if !ok {
			curIdx = idxOf(bus.CurrentStopID)
		}
		if curIdx < 0 {
			continue
		}
		forward := (bus.Direction == "outbound")
		// Prefer nearest ahead by km
		bestIdx := -1
		bestKm := math.MaxFloat64
		if opt.TraceBusID > 0 && opt.TraceBusID == bus.ID {
			fmt.Printf("[trace] reposition_select bus=%d cur_idx=%d dir=%s candidates=", bus.ID, curIdx, map[bool]string{true: "outbound", false: "inbound"}[forward])
			for ci, li := range layoverIdxs {
				_ = ci
				fmt.Printf("%d:", li)
			}
			fmt.Printf("\n")
		}
		for _, li := range layoverIdxs {
			if (forward && li > curIdx) || (!forward && li < curIdx) {
				dkm := kmBetweenIdx(curIdx, li)
				if dkm < bestKm {
					bestKm = dkm
					bestIdx = li
				}
				if opt.TraceBusID > 0 && opt.TraceBusID == bus.ID {
					fmt.Printf("[trace] ahead_candidate idx=%d km=%.3f\n", li, dkm)
				}
			}
		}
		if bestIdx == -1 { // fallback: nearest overall by km
			bestKm = math.MaxFloat64
			for _, li := range layoverIdxs {
				dkm := kmBetweenIdx(curIdx, li)
				if dkm < bestKm {
					bestKm = dkm
					bestIdx = li
				}
				if opt.TraceBusID > 0 && opt.TraceBusID == bus.ID {
					fmt.Printf("[trace] fallback_candidate idx=%d km=%.3f\n", li, dkm)
				}
			}
		}
		if opt.TraceBusID > 0 && opt.TraceBusID == bus.ID {
			fmt.Printf("[trace] reposition_choice bus=%d best_idx=%d best_km=%.3f\n", bus.ID, bestIdx, bestKm)
		}
		if bestIdx == -1 || bestIdx == curIdx {
			continue
		}
		step := 1
		if bestIdx < curIdx {
			step = -1
		}
		for i := curIdx; i != bestIdx; i += step {
			var dist float64
			if step == 1 {
				dist = route.Stops[i].DistanceToNext
			} else {
				dist = route.Stops[i-1].DistanceToNext
			}
			// Advance simulated time by travel duration for completeness
			travelMin := dist / bus.AverageSpeedKmph * 60
			travelDur := time.Duration(travelMin * float64(time.Minute))
			steps := int(travelDur / travelStep)
			if steps < 1 {
				steps = 1
			}
			stepDur := travelDur / time.Duration(steps)
			for sstep := 0; sstep < steps; sstep++ {
				engine.Now = engine.Now.Add(stepDur)
				// Credit distance gradually like SSE reposition move events
				busDistance[bus.ID] += dist / float64(steps)
				// quiet reposition move trace
			}
		}
		bus.CurrentStopID = route.Stops[bestIdx].ID
		if opt.TraceBusID > 0 && opt.TraceBusID == bus.ID {
			aheadOnly := ((forward && bestIdx > curIdx) || (!forward && bestIdx < curIdx))
			_ = aheadOnly // reserved for potential future logging parity
			fmt.Printf("buslog bus=%d layover stop_idx=%d next_idx=%d stop_id=%d dist_km=%.2f\n", bus.ID, bestIdx, -1, route.Stops[bestIdx].ID, math.Round(busDistance[bus.ID]*100)/100)
		}
	}

	avgWait := 0.0
	if waitCount > 0 {
		avgWait = waitSumMin / float64(waitCount)
	}
	// Clamp generated to cap defensively
	if engine.GeneratedPassengers > opt.PassengerCap {
		engine.GeneratedPassengers = opt.PassengerCap
	}

	round2 := func(x float64) float64 { return math.Round(x*100) / 100 }
	sum := Summary{Generated: engine.GeneratedPassengers, Served: cumServed, AvgWaitMin: avgWait, BusDistance: busDistance}
	// Compute totals as the sum of displayed per-bus values (rounded), so rows and totals align across drivers
	for _, b := range buses {
		d := round2(busDistance[b.ID])
		sum.TotalDistance += d
		if b.Type != nil {
			sum.TotalCost += round2(float64(b.Type.CostPerKm) * d)
		}
	}

	// Optional CSV report
	if opt.ReportPath != "" {
		ts := time.Now().Format("20060102-150405")
		outPath := opt.ReportPath
		if fi, err := os.Stat(outPath); err == nil && fi.IsDir() {
			outPath = filepath.Join(outPath, fmt.Sprintf("report-%s.csv", ts))
		} else if outPath != "" {
			ext := filepath.Ext(outPath)
			base := outPath[:len(outPath)-len(ext)]
			outPath = fmt.Sprintf("%s-%s%s", base, ts, ext)
		}
		if f, err := os.Create(outPath); err == nil {
			defer f.Close()
			fmt.Fprintln(f, "section,bus_id,direction,type,avg_speed_kmph,distance_km,cost,generated,served,avg_wait_min,buses_count,timestamp")
			for _, b := range buses {
				d := round2(busDistance[b.ID])
				c := 0.0
				typeName := ""
				if b.Type != nil {
					c = round2(float64(b.Type.CostPerKm) * d)
					typeName = b.Type.Name
				}
				fmt.Fprintf(f, "bus,%d,%s,%s,%.1f,%.2f,%.2f,,,,,%s\n", b.ID, b.Direction, typeName, b.AverageSpeedKmph, d, c, ts)
			}
			fmt.Fprintf(f, "summary,,,,,,%.2f,%d,%d,%.2f,%d,%s\n", sum.TotalCost, sum.Generated, sum.Served, sum.AvgWaitMin, len(buses), ts)
			log.Printf("CSV report written to %s", outPath)
		} else {
			log.Printf("report: create failed: %v", err)
		}
	}

	// Console report
	fmt.Println("=== Simulation Report (batch) ===")
	fmt.Printf("Buses on route: %d\n", len(buses))
	fmt.Printf("Passengers generated: %d\n", sum.Generated)
	fmt.Printf("Passengers served: %d\n", sum.Served)
	fmt.Printf("Average wait: %.2f minutes\n", sum.AvgWaitMin)
	for _, b := range buses {
		d := round2(busDistance[b.ID])
		c := 0.0
		name := ""
		if b.Type != nil {
			c = round2(float64(b.Type.CostPerKm) * d)
			name = b.Type.Name
		}
		fmt.Printf("Bus %d (%s, %s) distance=%.2f km cost=%.2f\n", b.ID, b.Direction, name, d, c)
	}
	fmt.Printf("Total distance: %.2f km\n", sum.TotalDistance)
	fmt.Printf("Total operating cost: %.2f\n", sum.TotalCost)
	return sum, nil
}
