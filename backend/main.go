package main

import (
	"encoding/json"
	"fmt"
	"time"
	"brt08/backend/model"
)

func main() {
	bt := &model.BusType{ID: 1, Name: "Standard 12m", Capacity: 8, CostPerKm: 1.75}
	bus := &model.Bus{ID: 1, Type: bt, RouteID: 100, CurrentStopID: 1, Direction: "outbound", AverageSpeedKmph: 32.5}

	// Define three stops on the route
	stop1 := &model.BusStop{ID: 1, Name: "Kimara", RouteID: 100}
	stop2 := &model.BusStop{ID: 2, Name: "UBungo", RouteID: 100}
	stop3 := &model.BusStop{ID: 3, Name: "Manzese", RouteID: 100}

	now := time.Now()
	// Enqueue passengers at their origin stops (outbound direction)
	stop1.EnqueuePassenger(&model.Passenger{ID: 1, RouteID: 100, StartStopID: 1, EndStopID: 3}, "outbound", now.Add(-6*time.Minute))
	stop1.EnqueuePassenger(&model.Passenger{ID: 2, RouteID: 100, StartStopID: 1, EndStopID: 2}, "outbound", now.Add(-4*time.Minute))
	stop2.EnqueuePassenger(&model.Passenger{ID: 3, RouteID: 100, StartStopID: 2, EndStopID: 3}, "outbound", now.Add(-3*time.Minute))
	stop2.EnqueuePassenger(&model.Passenger{ID: 4, RouteID: 100, StartStopID: 2, EndStopID: 3}, "outbound", now.Add(-2*time.Minute))
	stop3.EnqueuePassenger(&model.Passenger{ID: 5, RouteID: 100, StartStopID: 3, EndStopID: 2}, "inbound", now.Add(-5*time.Minute)) // inbound future trip

	// Arrive at stop1
	bus.CurrentStopID = 1
	boardedS1 := stop1.BoardAtStop(bus, now)
	// Travel to stop2 (alight + board)
	t2 := now.Add(5 * time.Minute)
	bus.CurrentStopID = 2
	alightedS2 := bus.AlightPassengersAtCurrentStop(t2)
	boardedS2 := stop2.BoardAtStop(bus, t2)
	// Travel to stop3
	t3 := t2.Add(5 * time.Minute)
	bus.CurrentStopID = 3
	alightedS3 := bus.AlightPassengersAtCurrentStop(t3)
	boardedS3 := stop3.BoardAtStop(bus, t3) // should board none outbound (only inbound queued)

	out := struct {
		Bus       *model.Bus       `json:"bus"`
		Stop1     *model.BusStop   `json:"stop1_final"`
		Stop2     *model.BusStop   `json:"stop2_final"`
		Stop3     *model.BusStop   `json:"stop3_final"`
		BoardedS1 []*model.Passenger `json:"boarded_stop_1"`
		AlightS2  []*model.Passenger `json:"alighted_stop_2"`
		BoardedS2 []*model.Passenger `json:"boarded_stop_2"`
		AlightS3  []*model.Passenger `json:"alighted_stop_3"`
		BoardedS3 []*model.Passenger `json:"boarded_stop_3"`
	}{bus, stop1, stop2, stop3, boardedS1, alightedS2, boardedS2, alightedS3, boardedS3}

	j, _ := json.MarshalIndent(out, "", "  ")
	fmt.Println(string(j))
}
