package sim

import (
	"brt08/backend/data"
	"brt08/backend/model"
	"log"
	"math"
	"math/rand"
	"sync"
	"time"
)

// Control exposes per-connection tunables.
type Control interface {
	Speed() float64
	ArrivalFactor() float64
}

// StaticControl implements Control with fixed values.
type StaticControl struct {
	SpeedMult   float64
	ArrivalMult float64
}

func (s StaticControl) Speed() float64 {
	if s.SpeedMult <= 0 {
		return 1
	}
	if s.SpeedMult > 10 {
		return 10
	}
	return s.SpeedMult
}
func (s StaticControl) ArrivalFactor() float64 {
	if s.ArrivalMult <= 0 {
		return 1
	}
	if s.ArrivalMult > 50 {
		return 50
	}
	return s.ArrivalMult
}

// Runner coordinates the simulation and emits events on the returned channel.
// It returns a stop function to cancel, and a Wait that blocks for completion.
func StartRunner(route *model.Route, fleet []*model.Bus, engineSeed int64, lambda float64, opts struct {
	PeriodID              int
	PassengerCap          int
	MorningTowardKivukoni bool
	DirBias               float64
	SpatialGradient       float64
	BaselineDemand        float64
	TraceBusID            int
	ConnID                string
	Start                 time.Time
}, ctrl Control) (events <-chan Event, stop func(), wait func()) {
	ch := make(chan Event, 256)
	var wg sync.WaitGroup
	stopCh := make(chan struct{})
	var stopOnce sync.Once
	stop = func() { stopOnce.Do(func() { close(stopCh) }) }
	wait = func() { wg.Wait() }

	// internal helpers
	var mu sync.Mutex // protect engine, route queues, counters, and shared aggregates

	// Create a base RNG for schedule decisions
	baseRNG := rand.New(rand.NewSource(engineSeed ^ 0x539f0a17))

	// Create a dummy bus for the simulator utilities (poisson, passenger creation, counters)
	var dummy *model.Bus
	if len(fleet) > 0 && fleet[0] != nil {
		proto := fleet[0]
		dummy = &model.Bus{ID: 0, Type: proto.Type, RouteID: route.ID, CurrentStopID: proto.CurrentStopID, Direction: proto.Direction, AverageSpeedKmph: proto.AverageSpeedKmph}
	} else {
		bt := &model.BusType{ID: 1, Name: "Standard", Capacity: 60}
		dummy = &model.Bus{ID: 0, Type: bt, RouteID: route.ID, CurrentStopID: route.Stops[0].ID, Direction: "outbound", AverageSpeedKmph: 28}
	}
	engine := NewSimulator(route, dummy, engineSeed, lambda, opts.Start)
	engine.PeriodID = opts.PeriodID
	engine.TotalPassengerCap = opts.PassengerCap
	engine.MorningTowardKivukoni = opts.MorningTowardKivukoni
	engine.DirectionBiasFactor = opts.DirBias

	// Aggregates
	var cumServed int64
	var waitSumMin float64
	var waitCount int64
	busDistance := make(map[int]float64)

	// simulate time speed mapping (simulation seconds to real seconds)
	const simSecToReal = 0.2
	waitSim := func(simDur time.Duration) bool {
		remaining := simDur
		for remaining > 0 {
			// allow up to 500ms chunks of sim time between checks
			chunk := remaining
			if chunk > 500*time.Millisecond {
				chunk = 500 * time.Millisecond
			}
			cur := ctrl.Speed()
			if cur <= 0 {
				cur = 1
			}
			realSleep := time.Duration(float64(chunk) * simSecToReal / cur)
			select {
			case <-stopCh:
				return false
			case <-time.After(realSleep):
			}
			remaining -= chunk
		}
		return true
	}

	// Completion logic mirrors server
	isDone := func() bool {
		if opts.PassengerCap <= 0 {
			return false
		}
		mu.Lock()
		defer mu.Unlock()
		inSystem := 0
		for _, b := range fleet {
			inSystem += b.PassengersOnboard
		}
		for _, st := range route.Stops {
			inSystem += len(st.OutboundQueue) + len(st.InboundQueue)
		}
		if int64(opts.PassengerCap) <= cumServed && inSystem == 0 {
			return true
		}
		if engine.GeneratedPassengers >= opts.PassengerCap && inSystem == 0 {
			return true
		}
		if engine.GeneratedPassengers == int(cumServed) && inSystem == 0 {
			return true
		}
		return false
	}

	// Internal completion should NOT close stopCh (reserved for external cancel).
	// Use isDone() checks in loops to exit gracefully; keep stopCh only for external stop.
	signalStopIfDone := func() {}

	// Demand configuration
	mult := data.TimePeriodMultiplier[engine.PeriodID]
	if mult == 0 {
		mult = 1
	}
	totalTarget := opts.PassengerCap
	initialSeedFraction := 0.05
	seedTarget := 0
	if totalTarget > 0 {
		seedTarget = int(float64(totalTarget) * initialSeedFraction)
	}
	favOut, favIn := FavoredDirections(engine.PeriodID, opts.MorningTowardKivukoni)
	cfg := DemandConfig{FavoredOutbound: favOut, FavoredInbound: favIn, SpatialGradient: opts.SpatialGradient, BaselineDemand: opts.BaselineDemand, DirBias: opts.DirBias}

	// Initial seed
	if seedTarget > 0 {
		mu.Lock()
		SeedInitial(engine, route, opts.Start, seedTarget, totalTarget, cfg)
		mu.Unlock()
	}
	for _, st := range route.Stops {
		ch <- StopUpdateEvent{StopID: st.ID, OutboundQueue: len(st.OutboundQueue), InboundQueue: len(st.InboundQueue), Generated: engine.GeneratedPassengers, OutboundGenerated: engine.OutboundGenerated, InboundGenerated: engine.InboundGenerated}
	}

	// Emit init event
	ch <- InitEvent{Time: engine.Now, ConnID: opts.ConnID, Generated: engine.GeneratedPassengers, OutboundGen: engine.OutboundGenerated, InboundGen: engine.InboundGenerated, AvgWaitMin: 0.0, ArrivalFactor: ctrl.ArrivalFactor()}

	// Start generator goroutine if needed
	var genWg sync.WaitGroup
	genStarted := false
	if totalTarget == 0 || engine.GeneratedPassengers < totalTarget {
		genStarted = true
		genWg.Add(1)
		go func() {
			defer genWg.Done()
			simStep := 1 * time.Second
			genNow := opts.Start
			for {
				if totalTarget > 0 && engine.GeneratedPassengers >= totalTarget {
					return
				}
				if !waitSim(simStep) {
					return
				}
				mu.Lock()
				if totalTarget > 0 && engine.GeneratedPassengers >= totalTarget {
					mu.Unlock()
					return
				}
				genNow = genNow.Add(simStep) // advance generator clock in fixed steps
				stepMin := simStep.Minutes()
				arrMult := ctrl.ArrivalFactor()
				mean := lambda * float64(mult) * stepMin * arrMult
				count := engine.PoissonPublic(mean)
				if totalTarget > 0 {
					remaining := totalTarget - engine.GeneratedPassengers
					if remaining < 0 {
						remaining = 0
					}
					if count > remaining {
						count = remaining
					}
				}
				if count > 0 {
					updated := GenerateBatch(engine, route, count, genNow, totalTarget, cfg)
					for sid := range updated {
						st := route.GetStop(sid)
						if st != nil {
							ch <- StopUpdateEvent{StopID: sid, OutboundQueue: len(st.OutboundQueue), InboundQueue: len(st.InboundQueue), Generated: engine.GeneratedPassengers, OutboundGenerated: engine.OutboundGenerated, InboundGenerated: engine.InboundGenerated}
						}
					}
				}
				mu.Unlock()
			}
		}()
	}

	// choose initial directions based on period bias
	favOut = (engine.PeriodID == 2 && opts.MorningTowardKivukoni) || (engine.PeriodID == 5 && !opts.MorningTowardKivukoni)
	favIn = (engine.PeriodID == 2 && !opts.MorningTowardKivukoni) || (engine.PeriodID == 5 && opts.MorningTowardKivukoni)
	pOutbound := 0.5
	if favOut {
		pOutbound = engine.DirectionBiasFactor / (engine.DirectionBiasFactor + 1.0)
	} else if favIn {
		pOutbound = 1.0 / (engine.DirectionBiasFactor + 1.0)
	}
	for _, b := range fleet {
		if baseRNG.Float64() <= pOutbound {
			b.Direction = "outbound"
			b.CurrentStopID = route.Stops[0].ID
		} else {
			b.Direction = "inbound"
			b.CurrentStopID = route.Stops[len(route.Stops)-1].ID
		}
	}

	// Build launch schedule to spread buses along route
	routeDistance := route.TotalDistanceKM
	if routeDistance <= 0 {
		sum := 0.0
		for _, st := range route.Stops {
			sum += st.DistanceToNext
		}
		if sum > 0 {
			routeDistance = sum
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
			simDelay := time.Duration(simOffsetMin * float64(time.Minute))
			sched = append(sched, struct {
				bus      *model.Bus
				simDelay time.Duration
			}{bus: b, simDelay: simDelay})
		}
		return sched
	}
	busesOutbound := make([]*model.Bus, 0)
	busesInbound := make([]*model.Bus, 0)
	for _, b := range fleet {
		if b.Direction == "inbound" {
			busesInbound = append(busesInbound, b)
		} else {
			busesOutbound = append(busesOutbound, b)
		}
	}
	schedule := append(makeSchedule(busesOutbound), makeSchedule(busesInbound)...)

	// dwell computation mirrors server
	computeDwell := func(boardedN, alightedN int) time.Duration {
		base := 1200 * time.Millisecond
		per := time.Duration(300*time.Millisecond) * time.Duration(boardedN+alightedN)
		max := 4 * time.Second
		d := base + per
		if d > max {
			d = max
		}
		return d
	}

	// per-bus simulation
	wg.Add(len(schedule))
	for _, item := range schedule {
		bus := item.bus
		forward := bus.Direction == "outbound"
		go func(bu *model.Bus, fwd bool, simD time.Duration) {
			defer wg.Done()
			if !waitSim(simD) {
				return
			}
			cap := 0
			if bu.Type != nil {
				cap = bu.Type.Capacity
			}
			ch <- BusAddEvent{BusID: bu.ID, Direction: bu.Direction, AvgSpeedKmph: bu.AverageSpeedKmph, Capacity: cap}
			var lat, lng float64
			if bu.Direction == "inbound" {
				lat = route.Stops[len(route.Stops)-1].Latitude
				lng = route.Stops[len(route.Stops)-1].Longitude
			} else {
				lat = route.Stops[0].Latitude
				lng = route.Stops[0].Longitude
			}
			ch <- MoveEvent{BusID: bu.ID, Direction: bu.Direction, Lat: lat, Lng: lng, From: 0, To: bu.CurrentStopID, T: 0}

			dirForward := fwd
			traceThis := opts.TraceBusID > 0 && opts.TraceBusID == bu.ID
			for {
				select {
				case <-stopCh:
					return
				default:
				}
				if dirForward {
					for idx := 0; idx < len(route.Stops); idx++ {
						select {
						case <-stopCh:
							return
						default:
						}
						stop := route.Stops[idx]
						mu.Lock()
						ch <- ArriveEvent{BusID: bu.ID, Direction: bu.Direction, StopID: stop.ID, Time: engine.Now, BusOnboard: bu.PassengersOnboard, PassengersOnboard: bu.PassengersOnboard, Generated: engine.GeneratedPassengers, OutboundGenerated: engine.OutboundGenerated, InboundGenerated: engine.InboundGenerated}
						if traceThis {
							nextIdx := idx
							if bu.Direction == "outbound" {
								if idx < len(route.Stops)-1 {
									nextIdx = idx + 1
								}
							} else {
								if idx > 0 {
									nextIdx = idx - 1
								}
							}
							dist := math.Round(busDistance[bu.ID]*100) / 100
							log.Printf("buslog bus=%d stop_idx=%d next_idx=%d stop_id=%d dist_km=%.2f", bu.ID, idx, nextIdx, stop.ID, dist)
						}
						alighted := bu.AlightPassengersAtCurrentStop(engine.Now)
						if len(alighted) > 0 {
							cumServed += int64(len(alighted))
							ch <- AlightEvent{BusID: bu.ID, Direction: bu.Direction, StopID: stop.ID, Alighted: len(alighted), BusOnboard: bu.PassengersOnboard, PassengersOnboard: bu.PassengersOnboard, Generated: engine.GeneratedPassengers, OutboundGenerated: engine.OutboundGenerated, InboundGenerated: engine.InboundGenerated, ServedPassengers: cumServed}
						}
						mu.Unlock()
						if !waitSim(650 * time.Millisecond) {
							return
						}
						mu.Lock()
						engine.Now = engine.Now.Add(650 * time.Millisecond)
						mu.Unlock()
						mu.Lock()
						boarded := stop.BoardAtStop(bu, engine.Now)
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
							avg := 0.0
							if waitCount > 0 {
								avg = waitSumMin / float64(waitCount)
							}
							ch <- BoardEvent{BusID: bu.ID, Direction: bu.Direction, StopID: stop.ID, Boarded: len(boarded), BusOnboard: bu.PassengersOnboard, PassengersOnboard: bu.PassengersOnboard, StopOutbound: len(stop.OutboundQueue), StopInbound: len(stop.InboundQueue), Generated: engine.GeneratedPassengers, OutboundGenerated: engine.OutboundGenerated, InboundGenerated: engine.InboundGenerated, ServedPassengers: cumServed, AvgWaitMin: avg}
						}
						ch <- StopUpdateEvent{StopID: stop.ID, OutboundQueue: len(stop.OutboundQueue), InboundQueue: len(stop.InboundQueue), Generated: engine.GeneratedPassengers, OutboundGenerated: engine.OutboundGenerated, InboundGenerated: engine.InboundGenerated}
						dwell := computeDwell(len(boarded), len(alighted))
						mu.Unlock()
						if isDone() {
							return
						}
						if !waitSim(dwell) {
							return
						}
						mu.Lock()
						engine.Now = engine.Now.Add(dwell)
						mu.Unlock()
						if isDone() {
							return
						}
						if idx == len(route.Stops)-1 {
							break
						}
						next := route.Stops[idx+1]
						dist := stop.DistanceToNext
						travelMin := dist / bu.AverageSpeedKmph * 60
						travelDur := time.Duration(travelMin * float64(time.Minute))
						steps := int(travelDur / (800 * time.Millisecond))
						if steps < 1 {
							steps = 1
						}
						for sstep := 1; sstep <= steps; sstep++ {
							t := float64(sstep) / float64(steps)
							lat := stop.Latitude + (next.Latitude-stop.Latitude)*t
							lng := stop.Longitude + (next.Longitude-stop.Longitude)*t
							ch <- MoveEvent{BusID: bu.ID, Direction: bu.Direction, Lat: lat, Lng: lng, T: t, From: stop.ID, To: next.ID}
							stepSim := travelDur / time.Duration(steps)
							if !waitSim(stepSim) {
								return
							}
							mu.Lock()
							engine.Now = engine.Now.Add(stepSim)
							mu.Unlock()
							select {
							case <-stopCh:
								return
							default:
							}
						}
						mu.Lock()
						busDistance[bu.ID] += dist
						mu.Unlock()
						bu.CurrentStopID = next.ID
					}
					mu.Lock()
					alighted := bu.AlightPassengersAtCurrentStop(engine.Now)
					if len(alighted) > 0 {
						cumServed += int64(len(alighted))
						ch <- AlightEvent{BusID: bu.ID, Direction: bu.Direction, StopID: bu.CurrentStopID, Alighted: len(alighted), BusOnboard: bu.PassengersOnboard, PassengersOnboard: bu.PassengersOnboard, Generated: engine.GeneratedPassengers, Final: true, ServedPassengers: cumServed}
					}
					mu.Unlock()
					if isDone() {
						return
					}
					if !waitSim(3 * time.Second) {
						return
					}
					mu.Lock()
					engine.Now = engine.Now.Add(3 * time.Second)
					mu.Unlock()
					signalStopIfDone()
					bu.Direction = "inbound"
					dirForward = false
				} else { // inbound traversal
					for ridx := len(route.Stops) - 1; ridx >= 0; ridx-- {
						select {
						case <-stopCh:
							return
						default:
						}
						stop := route.Stops[ridx]
						mu.Lock()
						ch <- ArriveEvent{BusID: bu.ID, Direction: bu.Direction, StopID: stop.ID, Time: engine.Now, BusOnboard: bu.PassengersOnboard, PassengersOnboard: bu.PassengersOnboard, Generated: engine.GeneratedPassengers, OutboundGenerated: engine.OutboundGenerated, InboundGenerated: engine.InboundGenerated}
						if traceThis {
							nextIdx := ridx
							if bu.Direction == "outbound" {
								if ridx < len(route.Stops)-1 {
									nextIdx = ridx + 1
								}
							} else {
								if ridx > 0 {
									nextIdx = ridx - 1
								}
							}
							dist := math.Round(busDistance[bu.ID]*100) / 100
							log.Printf("buslog bus=%d stop_idx=%d next_idx=%d stop_id=%d dist_km=%.2f", bu.ID, ridx, nextIdx, stop.ID, dist)
						}
						alighted := bu.AlightPassengersAtCurrentStop(engine.Now)
						if len(alighted) > 0 {
							cumServed += int64(len(alighted))
							ch <- AlightEvent{BusID: bu.ID, Direction: bu.Direction, StopID: stop.ID, Alighted: len(alighted), BusOnboard: bu.PassengersOnboard, PassengersOnboard: bu.PassengersOnboard, Generated: engine.GeneratedPassengers, OutboundGenerated: engine.OutboundGenerated, InboundGenerated: engine.InboundGenerated, ServedPassengers: cumServed}
						}
						mu.Unlock()
						if !waitSim(650 * time.Millisecond) {
							return
						}
						mu.Lock()
						engine.Now = engine.Now.Add(650 * time.Millisecond)
						mu.Unlock()
						mu.Lock()
						boarded := stop.BoardAtStop(bu, engine.Now)
						if len(boarded) > 0 {
							var localSum2 float64
							for _, p := range boarded {
								if p.WaitDuration != nil {
									localSum2 += *p.WaitDuration
								}
							}
							if localSum2 > 0 {
								waitSumMin += localSum2
								waitCount += int64(len(boarded))
							}
							avg2 := 0.0
							if waitCount > 0 {
								avg2 = waitSumMin / float64(waitCount)
							}
							ch <- BoardEvent{BusID: bu.ID, Direction: bu.Direction, StopID: stop.ID, Boarded: len(boarded), BusOnboard: bu.PassengersOnboard, PassengersOnboard: bu.PassengersOnboard, StopOutbound: len(stop.OutboundQueue), StopInbound: len(stop.InboundQueue), Generated: engine.GeneratedPassengers, OutboundGenerated: engine.OutboundGenerated, InboundGenerated: engine.InboundGenerated, ServedPassengers: cumServed, AvgWaitMin: avg2}
						}
						ch <- StopUpdateEvent{StopID: stop.ID, OutboundQueue: len(stop.OutboundQueue), InboundQueue: len(stop.InboundQueue), Generated: engine.GeneratedPassengers, OutboundGenerated: engine.OutboundGenerated, InboundGenerated: engine.InboundGenerated}
						dwell := computeDwell(len(boarded), len(alighted))
						mu.Unlock()
						if isDone() {
							return
						}
						if !waitSim(dwell) {
							return
						}
						mu.Lock()
						engine.Now = engine.Now.Add(dwell)
						mu.Unlock()
						if isDone() {
							return
						}
						if ridx == 0 {
							break
						}
						prev := route.Stops[ridx-1]
						dist := prev.DistanceToNext
						travelMin := dist / bu.AverageSpeedKmph * 60
						travelDur := time.Duration(travelMin * float64(time.Minute))
						steps := int(travelDur / (800 * time.Millisecond))
						if steps < 1 {
							steps = 1
						}
						for sstep := 1; sstep <= steps; sstep++ {
							t := float64(sstep) / float64(steps)
							lat := stop.Latitude + (prev.Latitude-stop.Latitude)*t
							lng := stop.Longitude + (prev.Longitude-stop.Longitude)*t
							ch <- MoveEvent{BusID: bu.ID, Direction: bu.Direction, Lat: lat, Lng: lng, T: t, From: stop.ID, To: prev.ID}
							stepSim := travelDur / time.Duration(steps)
							if !waitSim(stepSim) {
								return
							}
							mu.Lock()
							engine.Now = engine.Now.Add(stepSim)
							mu.Unlock()
							select {
							case <-stopCh:
								return
							default:
							}
						}
						mu.Lock()
						busDistance[bu.ID] += dist
						mu.Unlock()
						bu.CurrentStopID = prev.ID
					}
					mu.Lock()
					alighted2 := bu.AlightPassengersAtCurrentStop(engine.Now)
					if len(alighted2) > 0 {
						cumServed += int64(len(alighted2))
						ch <- AlightEvent{BusID: bu.ID, Direction: bu.Direction, StopID: bu.CurrentStopID, Alighted: len(alighted2), BusOnboard: bu.PassengersOnboard, PassengersOnboard: bu.PassengersOnboard, Generated: engine.GeneratedPassengers, OutboundGenerated: engine.OutboundGenerated, InboundGenerated: engine.InboundGenerated, Final: true, ServedPassengers: cumServed}
					}
					mu.Unlock()
					if isDone() {
						return
					}
					if !waitSim(3 * time.Second) {
						return
					}
					mu.Lock()
					engine.Now = engine.Now.Add(3 * time.Second)
					mu.Unlock()
					signalStopIfDone()
					bu.Direction = "outbound"
					dirForward = true
				}
			}
		}(bus, forward, item.simDelay)
	}

	// Closing goroutine to finish, reposition, and emit final events
	go func() {
		// Wait for buses to finish their traversal
		wg.Wait()
		if genStarted && opts.PassengerCap > 0 {
			genWg.Wait()
		}

		// Reposition phase (if a cap was set)
		repositionStart := time.Now()
		if opts.PassengerCap > 0 {
			layoverIdxSet := make(map[int]struct{})
			for i, st := range route.Stops {
				if st.AllowLayover {
					layoverIdxSet[i] = struct{}{}
				}
			}
			layoverIdxSet[0] = struct{}{}
			layoverIdxSet[len(route.Stops)-1] = struct{}{}
			layoverIdxs := make([]int, 0, len(layoverIdxSet))
			for idx := range layoverIdxSet {
				layoverIdxs = append(layoverIdxs, idx)
			}
			ch <- RepositionStartEvent{Buses: len(fleet), LayoverIndices: layoverIdxs}

			var repWg sync.WaitGroup
			repWg.Add(len(fleet))
			for _, b := range fleet {
				bus := b
				go func() {
					defer repWg.Done()
					curIdx := -1
					for i, st := range route.Stops {
						if st.ID == bus.CurrentStopID {
							curIdx = i
							break
						}
					}
					if curIdx == -1 {
						return
					}
					forward := (bus.Direction == "outbound")
					// distance between indices
					kmBetweenIdx := func(i, j int) float64 {
						if i == j {
							return 0
						}
						d := 0.0
						if j > i {
							for k := i; k < j; k++ {
								d += route.Stops[k].DistanceToNext
							}
						} else {
							for k := i; k > j; k-- {
								d += route.Stops[k-1].DistanceToNext
							}
						}
						return d
					}
					bestIdx := -1
					bestKm := math.MaxFloat64
					for _, li := range layoverIdxs {
						if (forward && li > curIdx) || (!forward && li < curIdx) {
							dkm := kmBetweenIdx(curIdx, li)
							if dkm < bestKm {
								bestKm = dkm
								bestIdx = li
							}
						}
					}
					aheadFound := (bestIdx != -1)
					if !aheadFound {
						bestKm = math.MaxFloat64
						for _, li := range layoverIdxs {
							dkm := kmBetweenIdx(curIdx, li)
							if dkm < bestKm {
								bestKm = dkm
								bestIdx = li
							}
						}
					}
					ch <- RepositionBusEvent{BusID: bus.ID, FromIndex: curIdx, TargetIndex: bestIdx, CurrentStopID: route.Stops[curIdx].ID, AheadOnly: aheadFound}
					traceThis := opts.TraceBusID > 0 && opts.TraceBusID == bus.ID
					if bestIdx == -1 || bestIdx == curIdx {
						ch <- LayoverEvent{BusID: bus.ID, TerminalStopID: route.Stops[curIdx].ID}
						if traceThis {
							dist := math.Round(busDistance[bus.ID]*100) / 100
							log.Printf("buslog bus=%d layover stop_idx=%d next_idx=%d stop_id=%d dist_km=%.2f", bus.ID, curIdx, -1, route.Stops[curIdx].ID, dist)
						}
						return
					}
					step := 1
					if bestIdx < curIdx {
						step = -1
					}
					for idx := curIdx; idx != bestIdx; idx += step {
						from := route.Stops[idx]
						to := route.Stops[idx+step]
						dist := from.DistanceToNext
						if step == -1 {
							prev := route.Stops[idx-1]
							dist = prev.DistanceToNext
						}
						travelMin := dist / bus.AverageSpeedKmph * 60
						if travelMin < 0 {
							travelMin = 0
						}
						travelDur := time.Duration(travelMin * float64(time.Minute))
						steps := int(travelDur / (800 * time.Millisecond))
						if steps < 1 {
							steps = 1
						}
						for sstep := 1; sstep <= steps; sstep++ {
							t := float64(sstep) / float64(steps)
							lat := from.Latitude + (to.Latitude-from.Latitude)*t
							lng := from.Longitude + (to.Longitude-from.Longitude)*t
							ch <- MoveEvent{BusID: bus.ID, Direction: bus.Direction, Lat: lat, Lng: lng, T: t, From: from.ID, To: to.ID, Phase: "reposition"}
							stepSim := travelDur / time.Duration(steps)
							if !waitSim(stepSim) {
								return
							}
							mu.Lock()
							engine.Now = engine.Now.Add(stepSim)
							busDistance[bus.ID] += dist / float64(steps)
							mu.Unlock()
						}
						bus.CurrentStopID = to.ID
					}
					ch <- LayoverEvent{BusID: bus.ID, TerminalStopID: route.Stops[bestIdx].ID}
					if traceThis {
						dist := math.Round(busDistance[bus.ID]*100) / 100
						log.Printf("buslog bus=%d layover stop_idx=%d next_idx=%d stop_id=%d dist_km=%.2f", bus.ID, bestIdx, -1, route.Stops[bestIdx].ID, dist)
					}
				}()
			}
			repWg.Wait()
			ch <- RepositionCompleteEvent{ElapsedMs: time.Since(repositionStart).Milliseconds()}
		}

		avgFinal := 0.0
		if waitCount > 0 {
			avgFinal = waitSumMin / float64(waitCount)
		}
		if opts.PassengerCap > 0 && engine.GeneratedPassengers > opts.PassengerCap {
			engine.GeneratedPassengers = opts.PassengerCap
		}
		ch <- DoneEvent{Completed: true, Generated: engine.GeneratedPassengers, OutboundGenerated: engine.OutboundGenerated, InboundGenerated: engine.InboundGenerated, ServedPassengers: cumServed, AvgWaitMin: avgFinal, BusDistance: busDistance}
		close(ch)
	}()

	return ch, stop, wait
}
