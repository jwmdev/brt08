package main

import (
	"encoding/json"
	"fmt"
	"log"
	"net/http"
	"os"
	"sync"
	"time"
	"brt08/backend/model"
	"brt08/backend/sim"
)

func main() {
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
			engine := sim.NewSimulator(route, outBus, time.Now().UnixNano(), 1.2, start)

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

			// Shared lock for stop queues
			var mu sync.Mutex
			seedWindow := 3.0
			// Seed outbound passengers
			for i := 0; i < len(route.Stops)-1; i++ {
				origin := route.Stops[i]
				count := engine.PoissonPublic(engine.LambdaPerMinute * seedWindow)
				for j := 0; j < count; j++ {
					destIndex := i + 1 + engine.RNG.Intn(len(route.Stops)-i-1)
					dest := route.Stops[destIndex]
					arrTime := start.Add(-time.Duration(engine.RNG.Float64()*seedWindow*float64(time.Minute)))
					p := engine.NewPassengerPublic(origin.ID, dest.ID, arrTime)
					p.Direction = "outbound"
					origin.EnqueuePassenger(p, "outbound", arrTime)
				}
			}
			// Seed inbound passengers
			for i := len(route.Stops)-1; i > 0; i-- {
				origin := route.Stops[i]
				count := engine.PoissonPublic(engine.LambdaPerMinute * seedWindow / 1.2) // maybe slightly lower inbound
				for j := 0; j < count; j++ {
					destIndex := engine.RNG.Intn(i) // 0 .. i-1
					dest := route.Stops[destIndex]
					arrTime := start.Add(-time.Duration(engine.RNG.Float64()*seedWindow*float64(time.Minute)))
					p := engine.NewPassengerPublic(origin.ID, dest.ID, arrTime)
					p.Direction = "inbound"
					origin.EnqueuePassenger(p, "inbound", arrTime)
				}
			}
			for _, st := range route.Stops {
				flush("stop_update", map[string]any{"stop_id": st.ID, "outbound_queue": len(st.OutboundQueue), "inbound_queue": len(st.InboundQueue)})
			}

			engine.Now = start
			flush("init", map[string]any{"time": engine.Now, "buses": []any{outBus, inBus}, "message": "started"})
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
				if forward {
					for idx := 0; idx < len(route.Stops); idx++ {
						stop := route.Stops[idx]
						mu.Lock()
							flush("arrive", map[string]any{"bus_id": bus.ID, "direction": bus.Direction, "stop_id": stop.ID, "outbound_queue": len(stop.OutboundQueue), "inbound_queue": len(stop.InboundQueue), "time": engine.Now, "bus_onboard": bus.PassengersOnboard, "passengers_onboard": bus.PassengersOnboard})
							alighted := bus.AlightPassengersAtCurrentStop(engine.Now)
							if len(alighted) > 0 { flush("alight", map[string]any{"bus_id": bus.ID, "direction": bus.Direction, "stop_id": stop.ID, "alighted": len(alighted), "bus_onboard": bus.PassengersOnboard, "passengers_onboard": bus.PassengersOnboard}) }
							boarded := stop.BoardAtStop(bus, engine.Now)
							if len(boarded) > 0 {
								flush("board", map[string]any{"bus_id": bus.ID, "direction": bus.Direction, "stop_id": stop.ID, "boarded": len(boarded), "bus_onboard": bus.PassengersOnboard, "passengers_onboard": bus.PassengersOnboard, "stop_outbound": len(stop.OutboundQueue), "stop_inbound": len(stop.InboundQueue)})
							}
							flush("stop_update", map[string]any{"stop_id": stop.ID, "outbound_queue": len(stop.OutboundQueue), "inbound_queue": len(stop.InboundQueue)})
							// compute dwell inside lock (using counts), then unlock before sleeping
							dwell := computeDwell(len(boarded), len(alighted))
							mu.Unlock()
							var avgWait float64
							if len(boarded) > 0 {
								var sum float64
								for _, p := range boarded { if p.WaitDuration != nil { sum += *p.WaitDuration } }
								avgWait = sum / float64(len(boarded))
							}
							log.Printf("STOP %d %s | alight=%d board=%d onboard=%d dwell=%v avg_wait=%.2fmin\n", stop.ID, stop.Name, len(alighted), len(boarded), bus.PassengersOnboard, dwell, avgWait)
							flush("dwell", map[string]any{"bus_id": bus.ID, "direction": bus.Direction, "stop_id": stop.ID, "alighted": len(alighted), "boarded": len(boarded), "bus_onboard": bus.PassengersOnboard, "dwell_ms": dwell.Milliseconds()})
							time.Sleep(dwell)
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
						}
						bus.CurrentStopID = next.ID
					}
					// final alight
					mu.Lock()
					alighted := bus.AlightPassengersAtCurrentStop(engine.Now)
					if len(alighted) > 0 { flush("alight", map[string]any{"bus_id": bus.ID, "direction": bus.Direction, "stop_id": bus.CurrentStopID, "alighted": len(alighted), "bus_onboard": bus.PassengersOnboard, "passengers_onboard": bus.PassengersOnboard, "final": true}) }
					mu.Unlock()
				} else { // inbound (reverse)
						for ridx := len(route.Stops)-1; ridx >= 0; ridx-- {
						stop := route.Stops[ridx]
						mu.Lock()
							flush("arrive", map[string]any{"bus_id": bus.ID, "direction": bus.Direction, "stop_id": stop.ID, "outbound_queue": len(stop.OutboundQueue), "inbound_queue": len(stop.InboundQueue), "time": engine.Now, "bus_onboard": bus.PassengersOnboard, "passengers_onboard": bus.PassengersOnboard})
							alighted := bus.AlightPassengersAtCurrentStop(engine.Now)
							if len(alighted) > 0 { flush("alight", map[string]any{"bus_id": bus.ID, "direction": bus.Direction, "stop_id": stop.ID, "alighted": len(alighted), "bus_onboard": bus.PassengersOnboard, "passengers_onboard": bus.PassengersOnboard}) }
							boarded := stop.BoardAtStop(bus, engine.Now)
							if len(boarded) > 0 {
								flush("board", map[string]any{"bus_id": bus.ID, "direction": bus.Direction, "stop_id": stop.ID, "boarded": len(boarded), "bus_onboard": bus.PassengersOnboard, "passengers_onboard": bus.PassengersOnboard, "stop_outbound": len(stop.OutboundQueue), "stop_inbound": len(stop.InboundQueue)})
							}
							flush("stop_update", map[string]any{"stop_id": stop.ID, "outbound_queue": len(stop.OutboundQueue), "inbound_queue": len(stop.InboundQueue)})
							dwell := computeDwell(len(boarded), len(alighted))
							mu.Unlock()
							var avgWait2 float64
							if len(boarded) > 0 {
								var sum2 float64
								for _, p := range boarded { if p.WaitDuration != nil { sum2 += *p.WaitDuration } }
								avgWait2 = sum2 / float64(len(boarded))
							}
							log.Printf("STOP %d %s | alight=%d board=%d onboard=%d dwell=%v avg_wait=%.2fmin\n", stop.ID, stop.Name, len(alighted), len(boarded), bus.PassengersOnboard, dwell, avgWait2)
							flush("dwell", map[string]any{"bus_id": bus.ID, "direction": bus.Direction, "stop_id": stop.ID, "alighted": len(alighted), "boarded": len(boarded), "bus_onboard": bus.PassengersOnboard, "dwell_ms": dwell.Milliseconds()})
							time.Sleep(dwell)
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
						}
						bus.CurrentStopID = prev.ID
					}
					mu.Lock()
					alighted := bus.AlightPassengersAtCurrentStop(engine.Now)
					if len(alighted) > 0 { flush("alight", map[string]any{"bus_id": bus.ID, "direction": bus.Direction, "stop_id": bus.CurrentStopID, "alighted": len(alighted), "bus_onboard": bus.PassengersOnboard, "passengers_onboard": bus.PassengersOnboard, "final": true}) }
					mu.Unlock()
				}
			}

			go simulate(outBus, true)
			go simulate(inBus, false)

			wg.Wait()
			flush("done", map[string]any{"completed": true})
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
