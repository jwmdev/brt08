package main

import (
	"encoding/json"
	"flag"
	"fmt"
	"log"
	"net/http"
	"os"
	"strconv"
	"sync"
	"time"
	"brt08/backend/model"
	"brt08/backend/sim"
	"brt08/backend/data"
)

func main() {
	periodID := flag.Int("period", 2, "time period id influencing demand (1..6)")
	passengerCap := flag.Int("passenger_cap", 0, "total passengers to generate (0 = unlimited / legacy unlimited mode)")
	morningTowardKivukoni := flag.Bool("morning_toward_kivukoni", true, "morning peak favored direction toward Kivukoni (outbound)")
	dirBias := flag.Float64("dir_bias", 1.4, "directional bias factor (>1 favor favored direction)")
	spatialGradient := flag.Float64("spatial_gradient", 0.8, "strength of spatial gradient (0-1) concentrating demand near origin of favored direction")
	baselineDemand := flag.Float64("baseline_demand", 0.3, "baseline fraction of demand when gradient applies (0-1)")
	flag.Parse()
	// Load route file
	f, err := os.Open("data/kimara_kivukoni_stops.json")
	if err != nil {
		panic(err)
	}
	defer f.Close()
	route, err := model.LoadRouteFromReader(f, 100)
	if err != nil {
		panic(err)
	}

	bt := &model.BusType{ID: 1, Name: "Standard 12m", Capacity: 70, CostPerKm: 1.75}
	// Template buses (one per direction)
	busOutbound := &model.Bus{ID: 1, Type: bt, RouteID: route.ID, CurrentStopID: route.Stops[0].ID, Direction: "outbound", AverageSpeedKmph: 28.0}
	busInbound := &model.Bus{ID: 2, Type: bt, RouteID: route.ID, CurrentStopID: route.Stops[len(route.Stops)-1].ID, Direction: "inbound", AverageSpeedKmph: 28.0}

	http.HandleFunc("/api/route", func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		w.Header().Set("Access-Control-Allow-Origin", "*")
		j, _ := json.Marshal(route)
		w.Write(j)
	})

	http.HandleFunc("/api/stream", func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/event-stream")
		w.Header().Set("Cache-Control", "no-cache")
		w.Header().Set("Connection", "keep-alive")
		w.Header().Set("Access-Control-Allow-Origin", "*")
		flusher, ok := w.(http.Flusher)
		if !ok { http.Error(w, "stream unsupported", 500); return }

			// Per-connection bus clones
			outBus := &model.Bus{ID: busOutbound.ID, Type: busOutbound.Type, RouteID: route.ID, CurrentStopID: route.Stops[0].ID, Direction: "outbound", AverageSpeedKmph: busOutbound.AverageSpeedKmph}
			inBus := &model.Bus{ID: busInbound.ID, Type: busInbound.Type, RouteID: route.ID, CurrentStopID: route.Stops[len(route.Stops)-1].ID, Direction: "inbound", AverageSpeedKmph: busInbound.AverageSpeedKmph}
			start := time.Now()
			lambda := 1.2
			if qs := r.URL.Query().Get("lambda"); qs != "" { if v, err := strconv.ParseFloat(qs, 64); err == nil && v > 0 { lambda = v } }
			engine := sim.NewSimulator(route, outBus, time.Now().UnixNano(), lambda, start)
			engine.PeriodID = *periodID
			engine.TotalPassengerCap = *passengerCap
			engine.MorningTowardKivukoni = *morningTowardKivukoni
			engine.DirectionBiasFactor = *dirBias

		// Serialize all writes to ResponseWriter; Go net/http forbids concurrent writes.
		var writeMu sync.Mutex
		flush := func(event string, payload any) {
			writeMu.Lock()
			b, _ := json.Marshal(payload)
			fmt.Fprintf(w, "event: %s\n", event)
			fmt.Fprintf(w, "data: %s\n\n", b)
			flusher.Flush()
			writeMu.Unlock()
		}

		// Global per-connection stats
		var cumServed int64 = 0
		var waitSumMin float64 = 0
		var waitCount int64 = 0

		// Shared lock for stop queues and time
		var mu sync.Mutex

		// Stop condition signaling
		stopCh := make(chan struct{})
		var stopOnce sync.Once
		isDone := func() bool {
			if *passengerCap <= 0 { return false }
			mu.Lock()
			defer mu.Unlock()
			if engine.GeneratedPassengers < *passengerCap { return false }
			if outBus.PassengersOnboard > 0 || inBus.PassengersOnboard > 0 { return false }
			for _, s := range route.Stops {
				if len(s.OutboundQueue) > 0 || len(s.InboundQueue) > 0 { return false }
			}
			return true
		}
		signalStopIfDone := func() {
			if isDone() { stopOnce.Do(func(){ close(stopCh) }) }
		}

				// Shared lock declared above
				// generator waitgroup pointer accessible after simulation loops
				var genWgPtr *sync.WaitGroup
				mult := data.TimePeriodMultiplier[engine.PeriodID]
				if mult == 0 { mult = 1 }

				// Background passenger generator: creates passengers gradually until target reached.
				totalTarget := *passengerCap // semantics: exact total to generate (if 0 => unlimited legacy behavior)
				initialSeedFraction := 0.05 // small initial presence (5%) so first stop not empty
				seedTarget := 0
				if totalTarget > 0 { seedTarget = int(float64(totalTarget) * initialSeedFraction) }
				favoredOutbound := (engine.PeriodID == 2 && *morningTowardKivukoni) || (engine.PeriodID == 5 && !*morningTowardKivukoni)
				favoredInbound := (engine.PeriodID == 2 && !*morningTowardKivukoni) || (engine.PeriodID == 5 && *morningTowardKivukoni)

				// Helper to compute spatial gradient weight for a stop index (outbound direction origin index i)
				gradientWeightOutbound := func(i int) float64 {
					if *spatialGradient <= 0 { return 1.0 }
					if !favoredOutbound { // unfavored: slightly downscale
						return 1.0 / (*dirBias)
					}
					nStops := float64(len(route.Stops)-1)
					if nStops <= 1 { return 1.0 }
					pos := float64(i)
					norm := 1.0 - pos/(nStops-1.0) // 1 at index 0 tapering to 0
					if norm < 0 { norm = 0 }
					if norm > 1 { norm = 1 }
					base := *baselineDemand
					if base < 0 { base = 0 }
					if base > 1 { base = 1 }
					return base + (*spatialGradient)*norm
				}
				gradientWeightInbound := func(i int) float64 { // i is index from 0..len-1 (origin for inbound)
					if *spatialGradient <= 0 { return 1.0 }
					if !favoredInbound { return 1.0 / (*dirBias) }
					nStops := float64(len(route.Stops)-1)
					if nStops <= 1 { return 1.0 }
					// favored origin is last stop index nStops
					pos := float64(len(route.Stops)-1 - i)
					norm := 1.0 - pos/(nStops-1.0)
					if norm < 0 { norm = 0 }
					if norm > 1 { norm = 1 }
					base := *baselineDemand
					if base < 0 { base = 0 }
					if base > 1 { base = 1 }
					return base + (*spatialGradient)*norm
				}

				// Small initial seed
				if seedTarget > 0 {
					for engine.GeneratedPassengers < seedTarget {
						// Alternate directions roughly using bias
						dir := "outbound"
						// Probability outbound
						pOutbound := 0.5
						if favoredOutbound { pOutbound = engine.DirectionBiasFactor / (engine.DirectionBiasFactor + 1.0) } else if favoredInbound { pOutbound = 1.0 / (engine.DirectionBiasFactor + 1.0) }
						if engine.RNG.Float64() >= pOutbound { dir = "inbound" }
						if dir == "outbound" {
							// choose origin weighted
							weights := make([]float64, len(route.Stops)-1)
							sum := 0.0
							for i := 0; i < len(route.Stops)-1; i++ { w := gradientWeightOutbound(i); weights[i] = w; sum += w }
							r := engine.RNG.Float64()*sum
							cum := 0.0
							originIdx := 0
							for i, w := range weights { cum += w; if r <= cum { originIdx = i; break } }
							destIdx := originIdx + 1 + engine.RNG.Intn(len(route.Stops)-originIdx-1)
							origin := route.Stops[originIdx]
							dest := route.Stops[destIdx]
							arrTime := start.Add(-time.Duration(engine.RNG.Float64()*2*float64(time.Minute)))
							p := engine.NewPassengerPublic(origin.ID, dest.ID, arrTime)
							p.Direction = "outbound"
							origin.EnqueuePassenger(p, "outbound", arrTime)
							engine.GeneratedPassengers++; engine.OutboundGenerated++
						} else { // inbound
							weights := make([]float64, len(route.Stops)-1)
							sum := 0.0
							// inbound origins indices 1..len-1
							for i := 1; i < len(route.Stops); i++ { w := gradientWeightInbound(i); weights[i-1] = w; sum += w }
							r := engine.RNG.Float64()*sum
							cum := 0.0
							originIdxGlobal := 1
							for k, w := range weights { cum += w; if r <= cum { originIdxGlobal = k+1; break } }
							destIdx := engine.RNG.Intn(originIdxGlobal) // 0..originIdxGlobal-1
							origin := route.Stops[originIdxGlobal]
							dest := route.Stops[destIdx]
							arrTime := start.Add(-time.Duration(engine.RNG.Float64()*2*float64(time.Minute)))
							p := engine.NewPassengerPublic(origin.ID, dest.ID, arrTime)
							p.Direction = "inbound"
							origin.EnqueuePassenger(p, "inbound", arrTime)
							engine.GeneratedPassengers++; engine.InboundGenerated++
						}
					}
				}

				// Emit initial state for all stops after seeding
				for _, st := range route.Stops {
					flush("stop_update", map[string]any{"stop_id": st.ID, "outbound_queue": len(st.OutboundQueue), "inbound_queue": len(st.InboundQueue), "generated_passengers": engine.GeneratedPassengers, "outbound_generated": engine.OutboundGenerated, "inbound_generated": engine.InboundGenerated})
				}
				log.Printf("Passenger generator starting: target=%d initial_seed=%d bias_factor=%.2f favored_outbound=%v favored_inbound=%v spatial_gradient=%.2f baseline=%.2f\n", totalTarget, seedTarget, engine.DirectionBiasFactor, favoredOutbound, favoredInbound, *spatialGradient, *baselineDemand)

				// Start background generator if we still have quota
				var genWg sync.WaitGroup
				genWgPtr = &genWg
				if totalTarget == 0 || engine.GeneratedPassengers < totalTarget {
					genWg.Add(1)
					go func() {
						defer genWg.Done()
						for {
							if totalTarget > 0 && engine.GeneratedPassengers >= totalTarget { return }
							// sleep random 200-800ms to stagger arrivals
							time.Sleep(time.Duration(200+engine.RNG.Intn(600)) * time.Millisecond)
							mu.Lock()
							// Re-check inside lock
							if totalTarget > 0 && engine.GeneratedPassengers >= totalTarget { mu.Unlock(); return }
							dir := "outbound"
							pOutbound := 0.5
							if favoredOutbound { pOutbound = engine.DirectionBiasFactor / (engine.DirectionBiasFactor + 1.0) } else if favoredInbound { pOutbound = 1.0 / (engine.DirectionBiasFactor + 1.0) }
							if engine.RNG.Float64() >= pOutbound { dir = "inbound" }
							if dir == "outbound" {
								weights := make([]float64, len(route.Stops)-1)
								sum := 0.0
								for i := 0; i < len(route.Stops)-1; i++ { w := gradientWeightOutbound(i); weights[i] = w; sum += w }
								r := engine.RNG.Float64()*sum
								cum := 0.0
								originIdx := 0
								for i, w := range weights { cum += w; if r <= cum { originIdx = i; break } }
								destIdx := originIdx + 1 + engine.RNG.Intn(len(route.Stops)-originIdx-1)
								origin := route.Stops[originIdx]
								dest := route.Stops[destIdx]
								arrTime := engine.Now // simulation time snapshot
								p := engine.NewPassengerPublic(origin.ID, dest.ID, arrTime)
								p.Direction = "outbound"
								origin.EnqueuePassenger(p, "outbound", arrTime)
								engine.GeneratedPassengers++; engine.OutboundGenerated++
								flush("stop_update", map[string]any{"stop_id": origin.ID, "outbound_queue": len(origin.OutboundQueue), "inbound_queue": len(origin.InboundQueue), "generated_passengers": engine.GeneratedPassengers, "outbound_generated": engine.OutboundGenerated, "inbound_generated": engine.InboundGenerated})
							} else {
								weights := make([]float64, len(route.Stops)-1)
								sum := 0.0
								for i := 1; i < len(route.Stops); i++ { w := gradientWeightInbound(i); weights[i-1] = w; sum += w }
								r := engine.RNG.Float64()*sum
								cum := 0.0
								originIdxGlobal := 1
								for k, w := range weights { cum += w; if r <= cum { originIdxGlobal = k+1; break } }
								destIdx := engine.RNG.Intn(originIdxGlobal)
								origin := route.Stops[originIdxGlobal]
								dest := route.Stops[destIdx]
								arrTime := engine.Now
								p := engine.NewPassengerPublic(origin.ID, dest.ID, arrTime)
								p.Direction = "inbound"
								origin.EnqueuePassenger(p, "inbound", arrTime)
								engine.GeneratedPassengers++; engine.InboundGenerated++
								flush("stop_update", map[string]any{"stop_id": origin.ID, "outbound_queue": len(origin.OutboundQueue), "inbound_queue": len(origin.InboundQueue), "generated_passengers": engine.GeneratedPassengers, "outbound_generated": engine.OutboundGenerated, "inbound_generated": engine.InboundGenerated})
							}
							mu.Unlock()
						}
					}()
				}

			engine.Now = start
			flush("init", map[string]any{"time": engine.Now, "buses": []any{outBus, inBus}, "message": "started", "generated_passengers": engine.GeneratedPassengers, "outbound_generated": engine.OutboundGenerated, "inbound_generated": engine.InboundGenerated, "served_passengers": cumServed, "avg_wait_min": 0.0})
			flush("move", map[string]any{"bus_id": outBus.ID, "direction": outBus.Direction, "lat": route.Stops[0].Latitude, "lng": route.Stops[0].Longitude, "from": 0, "to": route.Stops[0].ID, "t": 0})
			flush("move", map[string]any{"bus_id": inBus.ID, "direction": inBus.Direction, "lat": route.Stops[len(route.Stops)-1].Latitude, "lng": route.Stops[len(route.Stops)-1].Longitude, "from": 0, "to": route.Stops[len(route.Stops)-1].ID, "t": 0})

			var wg sync.WaitGroup
			wg.Add(2)

			simulate := func(bus *model.Bus, forward bool) {
				defer wg.Done()
					computeDwell := func(boardedN, alightedN int) time.Duration {
						// Base dwell plus small increment per passenger, capped.
						base := 1200 * time.Millisecond
						per := time.Duration(300 * time.Millisecond) * time.Duration(boardedN+alightedN)
						max := 4 * time.Second
						d := base + per
						if d > max { d = max }
						return d
					}
				// terminal dwell before turning around
				terminalDwell := 3 * time.Second
				dirForward := forward
				for { // loop indefinitely ping-ponging direction
					select { case <-stopCh: return; default: }
				if dirForward {
					for idx := 0; idx < len(route.Stops); idx++ {
						select { case <-stopCh: return; default: }
						stop := route.Stops[idx]
						// Stage 1: arrive + alight
						mu.Lock()
							flush("arrive", map[string]any{"bus_id": bus.ID, "direction": bus.Direction, "stop_id": stop.ID, "outbound_queue": len(stop.OutboundQueue), "inbound_queue": len(stop.InboundQueue), "time": engine.Now, "bus_onboard": bus.PassengersOnboard, "passengers_onboard": bus.PassengersOnboard, "generated_passengers": engine.GeneratedPassengers, "outbound_generated": engine.OutboundGenerated, "inbound_generated": engine.InboundGenerated})
							alighted := bus.AlightPassengersAtCurrentStop(engine.Now)
							if len(alighted) > 0 {
								cumServed += int64(len(alighted))
								flush("alight", map[string]any{"bus_id": bus.ID, "direction": bus.Direction, "stop_id": stop.ID, "alighted": len(alighted), "bus_onboard": bus.PassengersOnboard, "passengers_onboard": bus.PassengersOnboard, "generated_passengers": engine.GeneratedPassengers, "outbound_generated": engine.OutboundGenerated, "inbound_generated": engine.InboundGenerated, "served_passengers": cumServed})
							}
						mu.Unlock()
						// Small pause so onboard count visibly updates before boarding
						time.Sleep(650 * time.Millisecond)
						// advance simulation time by the pause duration
						mu.Lock()
						engine.Now = engine.Now.Add(650 * time.Millisecond)
						mu.Unlock()

						// Stage 2: board
						mu.Lock()
							boarded := stop.BoardAtStop(bus, engine.Now)
							if len(boarded) > 0 {
								// accumulate global wait stats (minutes)
								var localSum float64
								for _, p := range boarded { if p.WaitDuration != nil { localSum += *p.WaitDuration } }
								if localSum > 0 { waitSumMin += localSum; waitCount += int64(len(boarded)) }
								avg := 0.0
								if waitCount > 0 { avg = waitSumMin / float64(waitCount) }
								flush("board", map[string]any{"bus_id": bus.ID, "direction": bus.Direction, "stop_id": stop.ID, "boarded": len(boarded), "bus_onboard": bus.PassengersOnboard, "passengers_onboard": bus.PassengersOnboard, "stop_outbound": len(stop.OutboundQueue), "stop_inbound": len(stop.InboundQueue), "generated_passengers": engine.GeneratedPassengers, "outbound_generated": engine.OutboundGenerated, "inbound_generated": engine.InboundGenerated, "served_passengers": cumServed, "avg_wait_min": avg})
							}
							flush("stop_update", map[string]any{"stop_id": stop.ID, "outbound_queue": len(stop.OutboundQueue), "inbound_queue": len(stop.InboundQueue), "generated_passengers": engine.GeneratedPassengers, "outbound_generated": engine.OutboundGenerated, "inbound_generated": engine.InboundGenerated})
							// compute dwell inside lock (using counts), then unlock before sleeping
							dwell := computeDwell(len(boarded), len(alighted))
							mu.Unlock()
						signalStopIfDone()
							var avgWait float64
							if len(boarded) > 0 {
								var sum float64
								for _, p := range boarded { if p.WaitDuration != nil { sum += *p.WaitDuration } }
								avgWait = sum / float64(len(boarded))
							}
							log.Printf("STOP %d %s | alight=%d board=%d onboard=%d dwell=%v avg_wait=%.2fmin\n", stop.ID, stop.Name, len(alighted), len(boarded), bus.PassengersOnboard, dwell, avgWait)
							flush("dwell", map[string]any{"bus_id": bus.ID, "direction": bus.Direction, "stop_id": stop.ID, "alighted": len(alighted), "boarded": len(boarded), "bus_onboard": bus.PassengersOnboard, "dwell_ms": dwell.Milliseconds(), "generated_passengers": engine.GeneratedPassengers, "outbound_generated": engine.OutboundGenerated, "inbound_generated": engine.InboundGenerated})
							time.Sleep(dwell)
							// advance simulation time by dwell duration
							mu.Lock()
							engine.Now = engine.Now.Add(dwell)
							mu.Unlock()
							signalStopIfDone()
						if idx == len(route.Stops)-1 { break }
						next := route.Stops[idx+1]
						dist := stop.DistanceToNext
						travelMin := dist / bus.AverageSpeedKmph * 60
						travelDur := time.Duration(travelMin * float64(time.Minute))
						steps := int(travelDur / (800 * time.Millisecond))
						if steps < 1 { steps = 1 }
						for sstep := 1; sstep <= steps; sstep++ {
							t := float64(sstep) / float64(steps)
							lat := stop.Latitude + (next.Latitude-stop.Latitude)*t
							lng := stop.Longitude + (next.Longitude-stop.Longitude)*t
							flush("move", map[string]any{"bus_id": bus.ID, "direction": bus.Direction, "lat": lat, "lng": lng, "t": t, "from": stop.ID, "to": next.ID})
							time.Sleep(120 * time.Millisecond)
							// advance simulation time proportionally during travel
							mu.Lock()
							engine.Now = engine.Now.Add(travelDur / time.Duration(steps))
							mu.Unlock()
							select { case <-stopCh: return; default: }
						}
						bus.CurrentStopID = next.ID
					}
					// final alight at terminal, then turnaround after a short dwell
					mu.Lock()
					alighted := bus.AlightPassengersAtCurrentStop(engine.Now)
					if len(alighted) > 0 { cumServed += int64(len(alighted)); flush("alight", map[string]any{"bus_id": bus.ID, "direction": bus.Direction, "stop_id": bus.CurrentStopID, "alighted": len(alighted), "bus_onboard": bus.PassengersOnboard, "passengers_onboard": bus.PassengersOnboard, "generated_passengers": engine.GeneratedPassengers, "final": true, "served_passengers": cumServed}) }
					mu.Unlock()
					// pause at terminal
					time.Sleep(terminalDwell)
					// advance simulation time by terminal dwell
					mu.Lock()
					engine.Now = engine.Now.Add(terminalDwell)
					mu.Unlock()
					signalStopIfDone()
					// switch direction for next leg
					bus.Direction = "inbound"
					dirForward = false
				} else { // inbound (reverse)
						for ridx := len(route.Stops)-1; ridx >= 0; ridx-- {
							select { case <-stopCh: return; default: }
						stop := route.Stops[ridx]
						// Stage 1: arrive + alight (inbound)
						mu.Lock()
							flush("arrive", map[string]any{"bus_id": bus.ID, "direction": bus.Direction, "stop_id": stop.ID, "outbound_queue": len(stop.OutboundQueue), "inbound_queue": len(stop.InboundQueue), "time": engine.Now, "bus_onboard": bus.PassengersOnboard, "passengers_onboard": bus.PassengersOnboard, "generated_passengers": engine.GeneratedPassengers, "outbound_generated": engine.OutboundGenerated, "inbound_generated": engine.InboundGenerated})
							alighted := bus.AlightPassengersAtCurrentStop(engine.Now)
							if len(alighted) > 0 { cumServed += int64(len(alighted)); flush("alight", map[string]any{"bus_id": bus.ID, "direction": bus.Direction, "stop_id": stop.ID, "alighted": len(alighted), "bus_onboard": bus.PassengersOnboard, "passengers_onboard": bus.PassengersOnboard, "generated_passengers": engine.GeneratedPassengers, "outbound_generated": engine.OutboundGenerated, "inbound_generated": engine.InboundGenerated, "served_passengers": cumServed}) }
						mu.Unlock()
						// Pause so onboard decrease is visible before boarding
						time.Sleep(650 * time.Millisecond)
						mu.Lock()
						engine.Now = engine.Now.Add(650 * time.Millisecond)
						mu.Unlock()

						// Stage 2: board (inbound)
						mu.Lock()
							boarded := stop.BoardAtStop(bus, engine.Now)
							if len(boarded) > 0 {
								var localSum2 float64
								for _, p := range boarded { if p.WaitDuration != nil { localSum2 += *p.WaitDuration } }
								if localSum2 > 0 { waitSumMin += localSum2; waitCount += int64(len(boarded)) }
								avg2 := 0.0
								if waitCount > 0 { avg2 = waitSumMin / float64(waitCount) }
								flush("board", map[string]any{"bus_id": bus.ID, "direction": bus.Direction, "stop_id": stop.ID, "boarded": len(boarded), "bus_onboard": bus.PassengersOnboard, "passengers_onboard": bus.PassengersOnboard, "stop_outbound": len(stop.OutboundQueue), "stop_inbound": len(stop.InboundQueue), "generated_passengers": engine.GeneratedPassengers, "outbound_generated": engine.OutboundGenerated, "inbound_generated": engine.InboundGenerated, "served_passengers": cumServed, "avg_wait_min": avg2})
							}
							flush("stop_update", map[string]any{"stop_id": stop.ID, "outbound_queue": len(stop.OutboundQueue), "inbound_queue": len(stop.InboundQueue), "generated_passengers": engine.GeneratedPassengers, "outbound_generated": engine.OutboundGenerated, "inbound_generated": engine.InboundGenerated})
							dwell := computeDwell(len(boarded), len(alighted))
							mu.Unlock()
						signalStopIfDone()
							var avgWait2 float64
							if len(boarded) > 0 {
								var sum2 float64
								for _, p := range boarded { if p.WaitDuration != nil { sum2 += *p.WaitDuration } }
								avgWait2 = sum2 / float64(len(boarded))
							}
							log.Printf("STOP %d %s | alight=%d board=%d onboard=%d dwell=%v avg_wait=%.2fmin\n", stop.ID, stop.Name, len(alighted), len(boarded), bus.PassengersOnboard, dwell, avgWait2)
							flush("dwell", map[string]any{"bus_id": bus.ID, "direction": bus.Direction, "stop_id": stop.ID, "alighted": len(alighted), "boarded": len(boarded), "bus_onboard": bus.PassengersOnboard, "dwell_ms": dwell.Milliseconds(), "generated_passengers": engine.GeneratedPassengers, "outbound_generated": engine.OutboundGenerated, "inbound_generated": engine.InboundGenerated})
							time.Sleep(dwell)
							mu.Lock()
							engine.Now = engine.Now.Add(dwell)
							mu.Unlock()
							signalStopIfDone()
						if ridx == 0 { break }
						prev := route.Stops[ridx-1]
						// Distance from prev to current stored in prev.DistanceToNext; for reverse use prev.DistanceToNext
						dist := prev.DistanceToNext
						travelMin := dist / bus.AverageSpeedKmph * 60
						travelDur := time.Duration(travelMin * float64(time.Minute))
						steps := int(travelDur / (800 * time.Millisecond))
						if steps < 1 { steps = 1 }
						for sstep := 1; sstep <= steps; sstep++ {
							t := float64(sstep) / float64(steps)
							lat := stop.Latitude + (prev.Latitude-stop.Latitude)*t
							lng := stop.Longitude + (prev.Longitude-stop.Longitude)*t
							flush("move", map[string]any{"bus_id": bus.ID, "direction": bus.Direction, "lat": lat, "lng": lng, "t": t, "from": stop.ID, "to": prev.ID})
							time.Sleep(120 * time.Millisecond)
							mu.Lock()
							engine.Now = engine.Now.Add(travelDur / time.Duration(steps))
							mu.Unlock()
							select { case <-stopCh: return; default: }
						}
						bus.CurrentStopID = prev.ID
					}
					// final alight at terminal, then turnaround
					mu.Lock()
					alighted2 := bus.AlightPassengersAtCurrentStop(engine.Now)
					if len(alighted2) > 0 { cumServed += int64(len(alighted2)); flush("alight", map[string]any{"bus_id": bus.ID, "direction": bus.Direction, "stop_id": bus.CurrentStopID, "alighted": len(alighted2), "bus_onboard": bus.PassengersOnboard, "passengers_onboard": bus.PassengersOnboard, "generated_passengers": engine.GeneratedPassengers, "outbound_generated": engine.OutboundGenerated, "inbound_generated": engine.InboundGenerated, "final": true, "served_passengers": cumServed}) }
					mu.Unlock()
					time.Sleep(terminalDwell)
					mu.Lock()
					engine.Now = engine.Now.Add(terminalDwell)
					mu.Unlock()
					signalStopIfDone()
					bus.Direction = "outbound"
					dirForward = true
				}
				} // end for ping-pong
			}

			go simulate(outBus, true)
			go simulate(inBus, false)

			// Wait for simulate goroutines to finish (stopCh closed), then ensure generator finished
			wg.Wait()
			if genWgPtr != nil && *passengerCap > 0 { genWgPtr.Wait() }
			avgFinal := 0.0
			if waitCount > 0 { avgFinal = waitSumMin / float64(waitCount) }
			flush("done", map[string]any{"completed": true, "generated_passengers": engine.GeneratedPassengers, "outbound_generated": engine.OutboundGenerated, "inbound_generated": engine.InboundGenerated, "served_passengers": cumServed, "avg_wait_min": avgFinal})
	})

	log.Println("Serving on :8080")
	log.Fatal(http.ListenAndServe(":8080", nil))
}

// engineGenerateArrivals adds new passengers to downstream stops during an interval and flushes stop updates.
func engineGenerateArrivals(engine *sim.Simulator, start, end time.Time, fromIndex int, flush func(string, any)) {
	durMinutes := end.Sub(start).Minutes()
	if durMinutes <= 0 { return }
	for i := fromIndex; i < len(engine.Route.Stops)-1; i++ { // exclude last stop
		stop := engine.Route.Stops[i]
		mean := engine.LambdaPerMinute * durMinutes
		cnt := engine.PoissonPublic(mean)
		if cnt == 0 { continue }
		for j := 0; j < cnt; j++ {
			destIdx := i + 1 + engine.RNG.Intn(len(engine.Route.Stops)-i-1)
			dest := engine.Route.Stops[destIdx]
			t := start.Add(time.Duration(engine.RNG.Float64()*durMinutes*float64(time.Minute)))
			p := engine.NewPassengerPublic(stop.ID, dest.ID, t)
			stop.EnqueuePassenger(p, "outbound", t)
		}
		flush("stop_update", map[string]any{"stop_id": stop.ID, "outbound_queue": len(stop.OutboundQueue)})
	}
}
