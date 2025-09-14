package driver

import (
	"brt08/backend/model"
	"brt08/backend/sim"
	"fmt"
	"math"
	"math/rand"
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
	TimeScale             float64
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

	// Clone fleet to avoid mutating caller's instances (same as server)
	buses := make([]*model.Bus, 0, len(fleet))
	for _, proto := range fleet {
		if proto == nil {
			continue
		}
		b := &model.Bus{ID: proto.ID, Type: proto.Type, RouteID: proto.RouteID, CurrentStopID: proto.CurrentStopID, Direction: proto.Direction, AverageSpeedKmph: proto.AverageSpeedKmph}
		buses = append(buses, b)
	}
	if len(buses) == 0 {
		// fallback default two buses
		bt := &model.BusType{ID: 1, Name: "Standard 12m", Capacity: 70, CostPerKm: 1.75}
		buses = []*model.Bus{
			{ID: 1, Type: bt, RouteID: route.ID, CurrentStopID: route.Stops[0].ID, Direction: "outbound", AverageSpeedKmph: 28.0},
			{ID: 2, Type: bt, RouteID: route.ID, CurrentStopID: route.Stops[len(route.Stops)-1].ID, Direction: "inbound", AverageSpeedKmph: 28.0},
		}
	}

	// Seeds and control identical to server, but run fast (speed x10)
	start := time.Now()
	baseSeed := opt.Seed
	if baseSeed == 0 {
		baseSeed = time.Now().UnixNano()
	}
	_ = rand.New(rand.NewSource(baseSeed)) // reserve for potential future parity needs
	engineSeed := baseSeed + 1
	lambda := 1.2
	speed := opt.TimeScale
	if speed <= 0 {
		speed = 10.0 // default to fast batch if not specified
	}
	ctrl := sim.StaticControl{SpeedMult: speed, ArrivalMult: opt.ArrivalFactor}

	evCh, stopFn, waitFn := sim.StartRunner(route, buses, engineSeed, lambda, struct {
		PeriodID              int
		PassengerCap          int
		MorningTowardKivukoni bool
		DirBias               float64
		SpatialGradient       float64
		BaselineDemand        float64
		TraceBusID            int
		ConnID                string
		Start                 time.Time
		RealTimeFactor        float64
		MinSleep              time.Duration
	}{PeriodID: opt.PeriodID, PassengerCap: opt.PassengerCap, MorningTowardKivukoni: opt.MorningTowardKivukoni, DirBias: opt.DirBias, SpatialGradient: opt.SpatialGradient, BaselineDemand: opt.BaselineDemand, TraceBusID: opt.TraceBusID, ConnID: fmt.Sprintf("batch-%d", baseSeed), Start: start, RealTimeFactor: 0.2, MinSleep: 0}, ctrl)
	defer stopFn()
	defer waitFn()

	var final *sim.DoneEvent
	for e := range evCh {
		switch ev := e.(type) {
		case sim.DoneEvent:
			// capture and also forward-like behavior if needed
			final = &ev
		default:
			// in batch, we don't emit SSE; traces are produced inside runner when TraceBusID matches
		}
	}
	if final == nil {
		return Summary{}, fmt.Errorf("simulation did not complete")
	}

	// Build summary identical to server/report helpers
	round2 := func(x float64) float64 { return math.Round(x*100) / 100 }
	sum := Summary{Generated: final.Generated, Served: final.ServedPassengers, AvgWaitMin: final.AvgWaitMin, BusDistance: final.BusDistance}
	for _, b := range buses {
		d := round2(final.BusDistance[b.ID])
		sum.TotalDistance += d
		if b.Type != nil {
			sum.TotalCost += round2(float64(b.Type.CostPerKm) * d)
		}
	}

	// Reports identical to server
	report := sim.ReportSummary{Generated: sum.Generated, Served: sum.Served, AvgWaitMin: sum.AvgWaitMin, BusDistance: sum.BusDistance}
	if opt.ReportPath != "" {
		if _, err := sim.WriteCSVReport(opt.ReportPath, buses, report); err != nil {
			// non-fatal
		}
	}
	sim.PrintConsoleReport(buses, report)
	return sum, nil
}
