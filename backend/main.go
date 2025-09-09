package main

import (
	"encoding/json"
	"fmt"
	"log"
	"net/http"
	"os"
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
	bus := &model.Bus{ID: 1, Type: bt, RouteID: route.ID, CurrentStopID: route.Stops[0].ID, Direction: "outbound", AverageSpeedKmph: 28.0}

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

		// Per-connection bus clone
		connBus := &model.Bus{ID: bus.ID, Type: bus.Type, RouteID: route.ID, CurrentStopID: route.Stops[0].ID, Direction: "outbound", AverageSpeedKmph: bus.AverageSpeedKmph}
		start := time.Now()
		engine := sim.NewSimulator(route, connBus, time.Now().UnixNano(), 1.2, start) // slightly higher arrival rate for visibility

		flush := func(event string, payload any) {
			b, _ := json.Marshal(payload)
			fmt.Fprintf(w, "event: %s\n", event)
			fmt.Fprintf(w, "data: %s\n\n", b)
			flusher.Flush()
		}

		// Seed initial queues (3 min lookback) & send initial stop states (skip pins)
		seedWindow := 3.0
		for i := 0; i < len(route.Stops)-1; i++ {
			origin := route.Stops[i]
			count := engine.PoissonPublic(engine.LambdaPerMinute * seedWindow)
			if count == 0 { continue }
			for j := 0; j < count; j++ {
				destIndex := i + 1 + engine.RNG.Intn(len(route.Stops)-i-1)
				dest := route.Stops[destIndex]
				arrTime := start.Add(-time.Duration(engine.RNG.Float64()*seedWindow*float64(time.Minute)))
				p := engine.NewPassengerPublic(origin.ID, dest.ID, arrTime)
				origin.EnqueuePassenger(p, "outbound", arrTime)
			}
		}
		for _, st := range route.Stops {
			flush("stop_update", map[string]any{"stop_id": st.ID, "outbound_queue": len(st.OutboundQueue)})
		}

		engine.Now = start
		flush("init", map[string]any{"time": engine.Now, "bus": connBus, "message": "started"})
		flush("move", map[string]any{"lat": route.Stops[0].Latitude, "lng": route.Stops[0].Longitude, "from": 0, "to": route.Stops[0].ID, "t": 0})

		for idx := 0; idx < len(route.Stops); idx++ {
			stop := route.Stops[idx]
			flush("arrive", map[string]any{"stop_id": stop.ID, "outbound_queue": len(stop.OutboundQueue), "time": engine.Now})
			// Process dwell/boarding (all stops now real; pins moved to separate array)
			alightedCount := 0
			boardedCount := 0
			{
				constPauseAlight := 300 * time.Millisecond
				constPauseBoard := 300 * time.Millisecond
				alighted := connBus.AlightPassengersAtCurrentStop(engine.Now)
				if len(alighted) > 0 {
					alightedCount = len(alighted)
					flush("alight", map[string]any{"stop_id": stop.ID, "alighted": alightedCount, "bus_onboard": connBus.PassengersOnboard})
					time.Sleep(constPauseAlight)
					engine.Now = engine.Now.Add(constPauseAlight)
				}
				boarded := stop.BoardAtStop(connBus, engine.Now)
				if len(boarded) > 0 {
					boardedCount = len(boarded)
					flush("board", map[string]any{"stop_id": stop.ID, "boarded": boardedCount, "bus_onboard": connBus.PassengersOnboard, "stop_queue": len(stop.OutboundQueue)})
					time.Sleep(constPauseBoard)
					engine.Now = engine.Now.Add(constPauseBoard)
				}
				flush("stop_update", map[string]any{"stop_id": stop.ID, "outbound_queue": len(stop.OutboundQueue)})
				if idx == len(route.Stops)-1 { break }
				baseDwell := 1500*time.Millisecond + time.Duration(boardedCount)*200*time.Millisecond
				if baseDwell > 0 {
					flush("dwell", map[string]any{"stop_id": stop.ID, "remaining_ms": baseDwell.Milliseconds()})
					time.Sleep(baseDwell)
					engineGenerateArrivals(engine, engine.Now, engine.Now.Add(baseDwell), idx+1, flush)
					engine.Now = engine.Now.Add(baseDwell)
				}
			}
			if idx == len(route.Stops)-1 {
				// Final stop: force remaining onboard passengers to alight here.
				connBus.CurrentStopID = stop.ID
				alightedFinal := connBus.AlightPassengersAtCurrentStop(engine.Now)
				if len(alightedFinal) > 0 {
					flush("alight", map[string]any{"stop_id": stop.ID, "alighted": len(alightedFinal), "bus_onboard": connBus.PassengersOnboard, "final": true})
				}
				break
			}
			next := route.Stops[idx+1]
			dist := stop.DistanceToNext
			travelMin := dist / connBus.AverageSpeedKmph * 60
			travelDur := time.Duration(travelMin * float64(time.Minute))
			steps := int(travelDur / (800 * time.Millisecond))
			if steps < 1 { steps = 1 }
			for sstep := 1; sstep <= steps; sstep++ {
				t := float64(sstep) / float64(steps)
				lat := stop.Latitude + (next.Latitude-stop.Latitude)*t
				lng := stop.Longitude + (next.Longitude-stop.Longitude)*t
				flush("move", map[string]any{"lat": lat, "lng": lng, "t": t, "from": stop.ID, "to": next.ID})
				time.Sleep(120 * time.Millisecond)
				engineGenerateArrivals(engine, engine.Now, engine.Now.Add(120*time.Millisecond), idx+1, flush)
				engine.Now = engine.Now.Add(120 * time.Millisecond)
			}
			connBus.CurrentStopID = next.ID
		}
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
