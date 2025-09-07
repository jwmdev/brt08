package main

import (
	"encoding/json"
	"fmt"
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

	// Run stochastic simulation for this trip
	start := time.Now()
	simEngine := sim.NewSimulator(route, bus, time.Now().UnixNano(), 0.9, start)
	simEngine.RunOnce()

	// Collect stats slice in route order
	stats := make([]*sim.StopStats, 0, len(route.Stops))
	for _, sst := range route.Stops {
		stats = append(stats, simEngine.Stats[sst.ID])
	}

	out := struct {
		RouteID        int              `json:"route_id"`
		Bus            *model.Bus       `json:"bus"`
		CompletedTrips int              `json:"completed_trips"`
		EndOnboard     int              `json:"passengers_onboard_end"`
		Stats          []*sim.StopStats `json:"stop_stats"`
		TotalBoarded   int              `json:"total_boarded"`
		TotalAlighted  int              `json:"total_alighted"`
		SimEndTime     time.Time        `json:"simulation_end_time"`
	}{route.ID, bus, len(simEngine.Completed), bus.PassengersOnboard, stats, bus.TotalBoarded, bus.TotalAlighted, simEngine.Now}

	j, _ := json.MarshalIndent(out, "", "  ")
	fmt.Println(string(j))
}
