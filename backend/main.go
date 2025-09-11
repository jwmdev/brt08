package main

import (
	"flag"
	"log"
	"math/rand"
	"net/http"
	"os"
	"time"
	"brt08/backend/model"
	"brt08/backend/server"
)

func main() {
	// Flags
	periodID := flag.Int("period", 2, "time period id influencing demand (1..6)")
	passengerCap := flag.Int("passenger_cap", 0, "total passengers to generate (0 = unlimited / legacy unlimited mode)")
	morningTowardKivukoni := flag.Bool("morning_toward_kivukoni", true, "morning peak favored direction toward Kivukoni (outbound)")
	dirBias := flag.Float64("dir_bias", 1.4, "directional bias factor (>1 favor favored direction)")
	spatialGradient := flag.Float64("spatial_gradient", 0.8, "strength of spatial gradient (0-1)")
	baselineDemand := flag.Float64("baseline_demand", 0.3, "baseline fraction when gradient applies (0-1)")
	reportPath := flag.String("report", "", "if set, write CSV to this file or directory (timestamp appended)")
	defaultSpeed := flag.Float64("time_scale", 1.0, "simulation real-time speed multiplier (>1 = faster)")
	defaultArrFactor := flag.Float64("arrival_factor", 1.0, "multiplier for passenger arrival rate (>1 = faster)")
	addr := flag.String("addr", ":8080", "listen address")
	flag.Parse()

	// Load route
	rf, err := os.Open("data/kimara_kivukoni_stops.json"); if err != nil { panic(err) }
	defer rf.Close()
	route, err := model.LoadRouteFromReader(rf, 100); if err != nil { panic(err) }

	// Load fleet or fallback
	fleetFile, err := os.Open("data/fleet.json"); if err != nil { log.Printf("warning: open fleet.json failed: %v; falling back to two default buses", err) }
	var fleetBuses []*model.Bus
	if err == nil { defer fleetFile.Close(); types, qty, ferr := model.LoadFleetFromReader(fleetFile); if ferr != nil { log.Printf("warning: parse fleet.json failed: %v; using defaults", ferr) } else { rng := rand.New(rand.NewSource(time.Now().UnixNano())); first := route.Stops[0].ID; last := route.Stops[len(route.Stops)-1].ID; fleetBuses = model.BuildFleetBuses(types, qty, route.ID, first, last, rng) } }
	if len(fleetBuses) == 0 { bt := &model.BusType{ID:1, Name:"Standard 12m", Capacity:70, CostPerKm:1.75}; fleetBuses = []*model.Bus{{ID:1, Type:bt, RouteID: route.ID, CurrentStopID: route.Stops[0].ID, Direction:"outbound", AverageSpeedKmph:28.0}, {ID:2, Type:bt, RouteID: route.ID, CurrentStopID: route.Stops[len(route.Stops)-1].ID, Direction:"inbound", AverageSpeedKmph:28.0}} }

	srv := server.New(route, fleetBuses, server.Options{PeriodID:*periodID, PassengerCap:*passengerCap, MorningTowardKivukoni:*morningTowardKivukoni, DirBias:*dirBias, SpatialGradient:*spatialGradient, BaselineDemand:*baselineDemand, DefaultSpeed:*defaultSpeed, DefaultArrivalFactor:*defaultArrFactor, ReportPath:*reportPath})
	srv.Serve()
	log.Printf("Serving on %s", *addr)
	log.Fatal(http.ListenAndServe(*addr, nil))
}

// (helper removed; generation moved into stream loop)
