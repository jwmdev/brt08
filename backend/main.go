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
	"sync/atomic"
	"time"
	"math/rand"
	"path/filepath"
	"brt08/backend/model"
	"brt08/backend/sim"
	"brt08/backend/data"
)

// per-connection control state
type connControl struct {
    speed atomic.Value // stores float64
	arrivalMult atomic.Value // stores float64 multiplier for arrival rate
}

var streamControls sync.Map // map[string]*connControl

func main() {
	periodID := flag.Int("period", 2, "time period id influencing demand (1..6)")
	passengerCap := flag.Int("passenger_cap", 0, "total passengers to generate (0 = unlimited / legacy unlimited mode)")
	morningTowardKivukoni := flag.Bool("morning_toward_kivukoni", true, "morning peak favored direction toward Kivukoni (outbound)")
	dirBias := flag.Float64("dir_bias", 1.4, "directional bias factor (>1 favor favored direction)")
	spatialGradient := flag.Float64("spatial_gradient", 0.8, "strength of spatial gradient (0-1) concentrating demand near origin of favored direction")
	baselineDemand := flag.Float64("baseline_demand", 0.3, "baseline fraction of demand when gradient applies (0-1)")
	reportPath := flag.String("report", "", "if set, write a CSV report to this file or directory (timestamp appended)")
	defaultSpeed := flag.Float64("time_scale", 1.0, "simulation real-time speed multiplier (>1 = faster)")
	defaultArrFactor := flag.Float64("arrival_factor", 1.0, "multiplier for passenger arrival rate (>1 = faster arrivals)")
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

	// Load fleet configuration
	fleetFile, err := os.Open("data/fleet.json")
	if err != nil { log.Printf("warning: open fleet.json failed: %v; falling back to two default buses", err) }
	var fleetBuses []*model.Bus
	if err == nil {
		defer fleetFile.Close()
		types, qty, ferr := model.LoadFleetFromReader(fleetFile)
		if ferr != nil {
			log.Printf("warning: parse fleet.json failed: %v; using defaults", ferr)
		} else {
			rng := rand.New(rand.NewSource(time.Now().UnixNano()))
			first := route.Stops[0].ID
			last := route.Stops[len(route.Stops)-1].ID
			fleetBuses = model.BuildFleetBuses(types, qty, route.ID, first, last, rng)
		}
	}
	if len(fleetBuses) == 0 {
		// Fallback to two template buses (one per direction)
		bt := &model.BusType{ID: 1, Name: "Standard 12m", Capacity: 70, CostPerKm: 1.75}
		fleetBuses = []*model.Bus{
			{ID: 1, Type: bt, RouteID: route.ID, CurrentStopID: route.Stops[0].ID, Direction: "outbound", AverageSpeedKmph: 28.0},
			{ID: 2, Type: bt, RouteID: route.ID, CurrentStopID: route.Stops[len(route.Stops)-1].ID, Direction: "inbound", AverageSpeedKmph: 28.0},
		}
	}

	http.HandleFunc("/api/route", func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		w.Header().Set("Access-Control-Allow-Origin", "*")
		j, _ := json.Marshal(route)
		w.Write(j)
	})

	// control endpoint: adjust speed/arrival rate of an active stream connection without restarting
	http.HandleFunc("/api/control", func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Access-Control-Allow-Origin", "*")
		w.Header().Set("Access-Control-Allow-Headers", "Content-Type")
		if r.Method == http.MethodOptions { w.WriteHeader(204); return }
		if r.Method != http.MethodPost { http.Error(w, "method not allowed", http.StatusMethodNotAllowed); return }
		var req struct { ConnID string `json:"conn_id"`; Speed float64 `json:"speed"`; ArrivalFactor float64 `json:"arrival_factor"` }
		if err := json.NewDecoder(r.Body).Decode(&req); err != nil { http.Error(w, "bad json", 400); return }
		v, ok := streamControls.Load(req.ConnID)
		if !ok { http.Error(w, "connection not found", 404); return }
		c := v.(*connControl)
		if req.Speed != 0 {
			sp := req.Speed
			if sp <= 0 { sp = 1 }
			if sp < 0.1 { sp = 0.1 }
			if sp > 10.0 { sp = 10.0 }
			c.speed.Store(sp)
		}
		if req.ArrivalFactor != 0 {
			af := req.ArrivalFactor
			if af <= 0 { af = 1 }
			if af < 0.1 { af = 0.1 }
			if af > 50.0 { af = 50.0 }
			c.arrivalMult.Store(af)
		}
		w.WriteHeader(204)
	})

	http.HandleFunc("/api/stream", func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/event-stream")
		w.Header().Set("Cache-Control", "no-cache")
		w.Header().Set("Connection", "keep-alive")
		w.Header().Set("Access-Control-Allow-Origin", "*")
		flusher, ok := w.(http.Flusher)
		if !ok { http.Error(w, "stream unsupported", 500); return }

			// Per-connection bus clones based on fleet
			// We'll instantiate but start them with staggered activation
			baseRNG := rand.New(rand.NewSource(time.Now().UnixNano()))
			connBuses := make([]*model.Bus, 0, len(fleetBuses))
			for _, proto := range fleetBuses {
				// Deep-ish clone
				b := &model.Bus{ID: proto.ID, Type: proto.Type, RouteID: proto.RouteID, CurrentStopID: proto.CurrentStopID, Direction: proto.Direction, AverageSpeedKmph: proto.AverageSpeedKmph}
				connBuses = append(connBuses, b)
			}
			start := time.Now()
			lambda := 1.2
			if qs := r.URL.Query().Get("lambda"); qs != "" { if v, err := strconv.ParseFloat(qs, 64); err == nil && v > 0 { lambda = v } }
			// Per-connection speed control (dynamic via /api/control)
			connID := fmt.Sprintf("%d-%d", time.Now().UnixNano(), rand.Int63())
			ctrl := &connControl{}
			// initial speed from query or default
			initSpeed := *defaultSpeed
			if qs := r.URL.Query().Get("speed"); qs != "" {
				if v, err := strconv.ParseFloat(qs, 64); err == nil && v > 0 { initSpeed = v }
			}
			if initSpeed < 0.1 { initSpeed = 0.1 }
			if initSpeed > 10.0 { initSpeed = 10.0 }
			ctrl.speed.Store(initSpeed)
			// initial arrival factor from query or default
			initArr := *defaultArrFactor
			if qs := r.URL.Query().Get("arrival_factor"); qs != "" {
				if v, err := strconv.ParseFloat(qs, 64); err == nil && v > 0 { initArr = v }
			}
			if initArr < 0.1 { initArr = 0.1 }
			if initArr > 50.0 { initArr = 50.0 }
			ctrl.arrivalMult.Store(initArr)
			streamControls.Store(connID, ctrl)
			defer streamControls.Delete(connID)
			// Use the first bus (or a default) for engine construction; engine.Bus is mostly unused for multi-bus stream logic
			var dummy *model.Bus
			if len(connBuses) > 0 {
				dummy = &model.Bus{ID: 0, Type: connBuses[0].Type, RouteID: route.ID, CurrentStopID: connBuses[0].CurrentStopID, Direction: connBuses[0].Direction, AverageSpeedKmph: connBuses[0].AverageSpeedKmph}
			} else {
				bt := &model.BusType{ID: 1, Name: "Standard", Capacity: 60, CostPerKm: 0}
				dummy = &model.Bus{ID: 0, Type: bt, RouteID: route.ID, CurrentStopID: route.Stops[0].ID, Direction: "outbound", AverageSpeedKmph: 28}
			}
			engine := sim.NewSimulator(route, dummy, time.Now().UnixNano(), lambda, start)
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

		// Consistent mapping from simulation time to real time; baseline 1 sim sec = 0.2 real sec at speed=1
		simSecToReal := 0.2
		// Wait for a duration of SIMULATION time, converting to real time using current speed.
		waitSim := func(simDur time.Duration) {
			remaining := simDur
			for remaining > 0 {
				chunk := remaining
				if chunk > 500*time.Millisecond { chunk = 500 * time.Millisecond }
				cur := ctrl.speed.Load().(float64); if cur <= 0 { cur = 1 }
				realSleep := time.Duration(float64(chunk) * simSecToReal / cur)
				time.Sleep(realSleep)
				remaining -= chunk
			}
		}
		// Per-bus distance (km)
		busDistance := make(map[int]float64)

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
			for _, b := range connBuses { if b.PassengersOnboard > 0 { return false } }
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
						// Interpret lambda as arrivals per corridor per minute (scaled by period multiplier)
						simStep := 1 * time.Second // simulation time step per batch
						for {
							if totalTarget > 0 && engine.GeneratedPassengers >= totalTarget { return }
							// Sleep for one simulation step in real time, respecting current speed
							waitSim(simStep)
							mu.Lock()
							// Re-check inside lock
							if totalTarget > 0 && engine.GeneratedPassengers >= totalTarget { mu.Unlock(); return }
							// Expected arrivals this step with live arrival multiplier
							stepMin := simStep.Minutes()
							arrMult := 1.0
							if v := ctrl.arrivalMult.Load(); v != nil { arrMult = v.(float64) }
							mean := lambda * float64(mult) * stepMin * arrMult
							count := engine.PoissonPublic(mean)
							if count > 0 {
								// Directional bias probability
								pOutbound := 0.5
								if favoredOutbound { pOutbound = engine.DirectionBiasFactor / (engine.DirectionBiasFactor + 1.0) } else if favoredInbound { pOutbound = 1.0 / (engine.DirectionBiasFactor + 1.0) }
								updatedStops := make(map[int]struct{})
								for i := 0; i < count; i++ {
									dir := "outbound"
									if engine.RNG.Float64() >= pOutbound { dir = "inbound" }
									if dir == "outbound" {
										weights := make([]float64, len(route.Stops)-1)
										sum := 0.0
										for si := 0; si < len(route.Stops)-1; si++ { w := gradientWeightOutbound(si); weights[si] = w; sum += w }
										r := engine.RNG.Float64()*sum
										cum := 0.0
										originIdx := 0
										for si, w := range weights { cum += w; if r <= cum { originIdx = si; break } }
										destIdx := originIdx + 1 + engine.RNG.Intn(len(route.Stops)-originIdx-1)
										origin := route.Stops[originIdx]
										dest := route.Stops[destIdx]
										arrTime := engine.Now
										p := engine.NewPassengerPublic(origin.ID, dest.ID, arrTime)
										p.Direction = "outbound"
										origin.EnqueuePassenger(p, "outbound", arrTime)
										engine.GeneratedPassengers++; engine.OutboundGenerated++
										updatedStops[origin.ID] = struct{}{}
									} else {
										weights := make([]float64, len(route.Stops)-1)
										sum := 0.0
										for si := 1; si < len(route.Stops); si++ { w := gradientWeightInbound(si); weights[si-1] = w; sum += w }
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
										updatedStops[origin.ID] = struct{}{}
									}
								}
								// Flush one stop_update per touched stop to limit chatter
								for sid := range updatedStops {
									st := route.GetStop(sid)
									if st != nil {
										flush("stop_update", map[string]any{"stop_id": sid, "outbound_queue": len(st.OutboundQueue), "inbound_queue": len(st.InboundQueue), "generated_passengers": engine.GeneratedPassengers, "outbound_generated": engine.OutboundGenerated, "inbound_generated": engine.InboundGenerated})
									}
								}
							}
							mu.Unlock()
						}
					}()
				}

			engine.Now = start
			// Do not pre-place all buses; they will appear over time via bus_add events
			flush("init", map[string]any{"time": engine.Now, "buses": []any{}, "message": "started", "conn_id": connID, "generated_passengers": engine.GeneratedPassengers, "outbound_generated": engine.OutboundGenerated, "inbound_generated": engine.InboundGenerated, "served_passengers": cumServed, "avg_wait_min": 0.0, "arrival_factor": ctrl.arrivalMult.Load().(float64)})

			var wg sync.WaitGroup

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
						// Small pause so onboard count visibly updates before boarding (simulation time)
						waitSim(650 * time.Millisecond)
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
							log.Printf("Bus %d (%s) STOP %d %s | alight=%d board=%d onboard=%d dwell=%v avg_wait=%.2fmin", bus.ID, bus.Direction, stop.ID, stop.Name, len(alighted), len(boarded), bus.PassengersOnboard, dwell, avgWait)
							flush("dwell", map[string]any{"bus_id": bus.ID, "direction": bus.Direction, "stop_id": stop.ID, "alighted": len(alighted), "boarded": len(boarded), "bus_onboard": bus.PassengersOnboard, "dwell_ms": dwell.Milliseconds(), "generated_passengers": engine.GeneratedPassengers, "outbound_generated": engine.OutboundGenerated, "inbound_generated": engine.InboundGenerated})
							// Sleep in real time proportionally to simulation dwell
							waitSim(dwell)
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
							// Sleep according to simulation step duration
							stepSim := travelDur / time.Duration(steps)
							waitSim(stepSim)
							// advance simulation time proportionally during travel
							mu.Lock()
							engine.Now = engine.Now.Add(travelDur / time.Duration(steps))
							mu.Unlock()
							select { case <-stopCh: return; default: }
						}
						// accumulate segment distance
						mu.Lock()
						busDistance[bus.ID] += dist
						mu.Unlock()
						bus.CurrentStopID = next.ID
					}
					// final alight at terminal, then turnaround after a short dwell
					mu.Lock()
					alighted := bus.AlightPassengersAtCurrentStop(engine.Now)
					if len(alighted) > 0 { cumServed += int64(len(alighted)); flush("alight", map[string]any{"bus_id": bus.ID, "direction": bus.Direction, "stop_id": bus.CurrentStopID, "alighted": len(alighted), "bus_onboard": bus.PassengersOnboard, "passengers_onboard": bus.PassengersOnboard, "generated_passengers": engine.GeneratedPassengers, "final": true, "served_passengers": cumServed}) }
					mu.Unlock()
					// pause at terminal (simulation time)
					waitSim(terminalDwell)
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
						// Pause so onboard decrease is visible before boarding (simulation time)
						waitSim(650 * time.Millisecond)
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
							log.Printf("Bus %d (%s) STOP %d %s | alight=%d board=%d onboard=%d dwell=%v avg_wait=%.2fmin", bus.ID, bus.Direction, stop.ID, stop.Name, len(alighted), len(boarded), bus.PassengersOnboard, dwell, avgWait2)
							flush("dwell", map[string]any{"bus_id": bus.ID, "direction": bus.Direction, "stop_id": stop.ID, "alighted": len(alighted), "boarded": len(boarded), "bus_onboard": bus.PassengersOnboard, "dwell_ms": dwell.Milliseconds(), "generated_passengers": engine.GeneratedPassengers, "outbound_generated": engine.OutboundGenerated, "inbound_generated": engine.InboundGenerated})
							waitSim(dwell)
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
							stepSim := travelDur / time.Duration(steps)
							waitSim(stepSim)
							mu.Lock()
							engine.Now = engine.Now.Add(travelDur / time.Duration(steps))
							mu.Unlock()
							select { case <-stopCh: return; default: }
						}
						// accumulate segment distance (reverse uses prev.DistanceToNext)
						mu.Lock()
						busDistance[bus.ID] += dist
						mu.Unlock()
						bus.CurrentStopID = prev.ID
					}
					// final alight at terminal, then turnaround
					mu.Lock()
					alighted2 := bus.AlightPassengersAtCurrentStop(engine.Now)
					if len(alighted2) > 0 { cumServed += int64(len(alighted2)); flush("alight", map[string]any{"bus_id": bus.ID, "direction": bus.Direction, "stop_id": bus.CurrentStopID, "alighted": len(alighted2), "bus_onboard": bus.PassengersOnboard, "passengers_onboard": bus.PassengersOnboard, "generated_passengers": engine.GeneratedPassengers, "outbound_generated": engine.OutboundGenerated, "inbound_generated": engine.InboundGenerated, "final": true, "served_passengers": cumServed}) }
					mu.Unlock()
					waitSim(terminalDwell)
					mu.Lock()
					engine.Now = engine.Now.Add(terminalDwell)
					mu.Unlock()
					signalStopIfDone()
					bus.Direction = "outbound"
					dirForward = true
				}
				} // end for ping-pong
			}

			// Choose initial direction for each bus using direction bias
			favoredOutbound = (engine.PeriodID == 2 && *morningTowardKivukoni) || (engine.PeriodID == 5 && !*morningTowardKivukoni)
			favoredInbound = (engine.PeriodID == 2 && !*morningTowardKivukoni) || (engine.PeriodID == 5 && *morningTowardKivukoni)
			pOutbound := 0.5
			if favoredOutbound { pOutbound = engine.DirectionBiasFactor / (engine.DirectionBiasFactor + 1.0) } else if favoredInbound { pOutbound = 1.0 / (engine.DirectionBiasFactor + 1.0) }
			// Assign
			for _, b := range connBuses {
				if baseRNG.Float64() <= pOutbound {
					b.Direction = "outbound"
					b.CurrentStopID = route.Stops[0].ID
				} else {
					b.Direction = "inbound"
					b.CurrentStopID = route.Stops[len(route.Stops)-1].ID
				}
			}
			// Compute simulation-time headway per direction and map to real-time activation offsets
			// Estimate trip time using average speed per direction. Then set headway ≈ tripTime / nDir
			busesOutbound := make([]*model.Bus, 0)
			busesInbound := make([]*model.Bus, 0)
			for _, b := range connBuses { if b.Direction == "inbound" { busesInbound = append(busesInbound, b) } else { busesOutbound = append(busesOutbound, b) } }

			routeDistance := route.TotalDistanceKM
			if routeDistance <= 0 {
				// fallback: sum DistanceToNext
				sum := 0.0
				for _, s := range route.Stops { sum += s.DistanceToNext }
				if sum > 0 { routeDistance = sum }
			}
			makeSchedule := func(list []*model.Bus) []struct{ bus *model.Bus; simDelay time.Duration } {
				n := len(list)
				if n == 0 { return nil }
				// average speed among these buses
				var avgV float64
				for _, b := range list { avgV += b.AverageSpeedKmph }
				avgV /= float64(n)
				if avgV <= 0 { avgV = 25 }
				tripMin := routeDistance / avgV * 60.0
				headwayMin := tripMin / float64(n)
				if headwayMin < 0.5 { headwayMin = 0.5 }
				if headwayMin > 15 { headwayMin = 15 }
				sched := make([]struct{ bus *model.Bus; simDelay time.Duration }, 0, n)
				for i, b := range list {
					base := float64(i) * headwayMin
					// jitter ±20%
					jitter := (baseRNG.Float64()*0.4 - 0.2) * headwayMin
					simOffsetMin := base + jitter
					if simOffsetMin < 0 { simOffsetMin = 0 }
					simDelay := time.Duration(simOffsetMin * float64(time.Minute))
					sched = append(sched, struct{ bus *model.Bus; simDelay time.Duration }{bus: b, simDelay: simDelay})
				}
				return sched
			}

			schedule := append(makeSchedule(busesOutbound), makeSchedule(busesInbound)...)
			// Launch buses according to schedule
			for _, item := range schedule {
				bus := item.bus
				forward := bus.Direction == "outbound"
				wg.Add(1)
				go func(bu *model.Bus, fwd bool, simD time.Duration) {
					// wait simulation time using dynamic speed
					waitSim(simD)
					// notify frontend and place at starting terminal
					cap := 0
					if bu.Type != nil { cap = bu.Type.Capacity }
					flush("bus_add", map[string]any{"bus_id": bu.ID, "direction": bu.Direction, "avg_speed_kmph": bu.AverageSpeedKmph, "capacity": cap})
					log.Printf("Bus %d added to route (%s), avg_speed=%.1f km/h", bu.ID, bu.Direction, bu.AverageSpeedKmph)
					var lat, lng float64
					if bu.Direction == "inbound" {
						lat = route.Stops[len(route.Stops)-1].Latitude
						lng = route.Stops[len(route.Stops)-1].Longitude
					} else {
						lat = route.Stops[0].Latitude
						lng = route.Stops[0].Longitude
					}
					flush("move", map[string]any{"bus_id": bu.ID, "direction": bu.Direction, "lat": lat, "lng": lng, "from": 0, "to": bu.CurrentStopID, "t": 0})
					simulate(bu, fwd)
				}(bus, forward, item.simDelay)
			}

			// Wait for simulate goroutines to finish (stopCh closed), then ensure generator finished
			wg.Wait()
			if genWgPtr != nil && *passengerCap > 0 { genWgPtr.Wait() }
			avgFinal := 0.0
			if waitCount > 0 { avgFinal = waitSumMin / float64(waitCount) }
			flush("done", map[string]any{"completed": true, "generated_passengers": engine.GeneratedPassengers, "outbound_generated": engine.OutboundGenerated, "inbound_generated": engine.InboundGenerated, "served_passengers": cumServed, "avg_wait_min": avgFinal})
			// Optional CSV report
			if *reportPath != "" {
				ts := time.Now().Format("20060102-150405")
				outPath := *reportPath
				if fi, err := os.Stat(outPath); err == nil && fi.IsDir() {
					outPath = filepath.Join(outPath, fmt.Sprintf("report-%s.csv", ts))
				} else if outPath != "" {
					ext := filepath.Ext(outPath)
					base := outPath[:len(outPath)-len(ext)]
					outPath = fmt.Sprintf("%s-%s%s", base, ts, ext)
				}
				f, err := os.Create(outPath)
				if err != nil {
					log.Printf("report: create failed: %v", err)
				} else {
					defer f.Close()
					fmt.Fprintln(f, "section,bus_id,direction,type,avg_speed_kmph,distance_km,cost,generated,served,avg_wait_min,buses_count,timestamp")
					for _, b := range connBuses {
						d := busDistance[b.ID]
						c := 0.0
						typeName := ""
						if b.Type != nil { c = float64(b.Type.CostPerKm) * d; typeName = b.Type.Name }
						fmt.Fprintf(f, "bus,%d,%s,%s,%.1f,%.3f,%.2f,,,,,%s\n", b.ID, b.Direction, typeName, b.AverageSpeedKmph, d, c, ts)
					}
					totalCost := 0.0
					for _, b := range connBuses { if b.Type != nil { totalCost += float64(b.Type.CostPerKm) * busDistance[b.ID] } }
					fmt.Fprintf(f, "summary,,,,,,%.2f,%d,%d,%.2f,%d,%s\n", totalCost, engine.GeneratedPassengers, cumServed, avgFinal, len(connBuses), ts)
					log.Printf("CSV report written to %s", outPath)
				}
			}
			// Final console report
			// Compute costs per bus
			totalCost := 0.0
			totalDist := 0.0
			fmt.Println("=== Simulation Report ===")
			fmt.Printf("Buses on route: %d", len(connBuses))
			fmt.Printf("Passengers generated: %d", engine.GeneratedPassengers)
			fmt.Printf("Passengers served: %d", cumServed)
			fmt.Printf("Average wait: %.2f minutes", avgFinal)
			for _, b := range connBuses {
				d := busDistance[b.ID]
				c := 0.0
				if b.Type != nil { c = float64(b.Type.CostPerKm) * d }
				totalDist += d
				totalCost += c
				name := ""
				if b.Type != nil { name = b.Type.Name }
				fmt.Printf("Bus %d (%s, %s) distance=%.2f km cost=%.2f", b.ID, b.Direction, name, d, c)
			}
			fmt.Printf("Total distance: %.2f km", totalDist)
			fmt.Printf("Total operating cost: %.2f", totalCost)
	})

	log.Println("Serving on :8080")
	log.Fatal(http.ListenAndServe(":8080", nil))
}

// (helper removed; generation moved into stream loop)
