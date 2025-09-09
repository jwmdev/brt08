package sim

import (
	"math"
	"math/rand"
	"time"

	"brt08/backend/model"
)

// StopStats holds aggregated statistics per stop.
type StopStats struct {
	StopID            int     `json:"stop_id"`
	Name              string  `json:"name"`
	ArrivalsGenerated int     `json:"arrivals_generated"`
	Boarded           int     `json:"boarded"`
	AvgWaitMinutes    float64 `json:"avg_wait_minutes"`
	RemainingOutbound int     `json:"remaining_outbound_queue"`
	RemainingInbound  int     `json:"remaining_inbound_queue"`
	sumWait           float64
}

// Simulator parameters and state for a single bus on one route.
type Simulator struct {
	Route      *model.Route
	Bus        *model.Bus
	RNG        *rand.Rand
	StartTime  time.Time
	Now        time.Time
	PassengerID int

	LambdaPerMinute float64 // expected passenger arrivals per stop per minute (outbound direction only for this demo)

	Completed []*model.Passenger
	Stats     map[int]*StopStats
}

// NewSimulator constructs a simulator with given route and bus.
func NewSimulator(route *model.Route, bus *model.Bus, seed int64, lambdaPerMinute float64, start time.Time) *Simulator {
	stats := make(map[int]*StopStats, len(route.Stops))
	for _, s := range route.Stops {
		stats[s.ID] = &StopStats{StopID: s.ID, Name: s.Name}
	}
	return &Simulator{
		Route:          route,
		Bus:            bus,
		RNG:            rand.New(rand.NewSource(seed)),
		StartTime:      start,
		Now:            start,
		LambdaPerMinute: lambdaPerMinute,
		Stats:          stats,
	}
}

// RunOnce moves the bus from first to last stop generating passengers and handling board/alight.
func (s *Simulator) RunOnce() {
	// Seed initial passengers (simulate arrivals in previous 5 minutes)
	seedWindow := 5.0 // minutes
	for i := 0; i < len(s.Route.Stops)-1; i++ { // exclude terminal (no departures)
		count := s.poisson(s.LambdaPerMinute * seedWindow)
		for j := 0; j < count; j++ {
			origin := s.Route.Stops[i]
			destIndex := i + 1 + s.RNG.Intn(len(s.Route.Stops)-i-1)
			dest := s.Route.Stops[destIndex]
			arrTime := s.StartTime.Add(-time.Duration(s.RNG.Float64()*seedWindow*float64(time.Minute)))
			p := s.newPassenger(origin.ID, dest.ID, arrTime)
			origin.EnqueuePassenger(p, "outbound", arrTime)
			ss := s.Stats[origin.ID]
			ss.ArrivalsGenerated++
		}
	}

	// Initialize bus at first stop
	if len(s.Route.Stops) == 0 { return }
	s.Bus.CurrentStopID = s.Route.Stops[0].ID

	// Iterate through each stop except final (where no boarding outbound after alight) and move to next
	for idx := 0; idx < len(s.Route.Stops); idx++ {
		stop := s.Route.Stops[idx]
		// Bus arrives at stop at current time: alight first
		alighted := s.Bus.AlightPassengersAtCurrentStop(s.Now)
		if len(alighted) > 0 {
			for _, p := range alighted { s.Completed = append(s.Completed, p) }
		}
		// Board waiting outbound passengers
		boarded := stop.BoardAtStop(s.Bus, s.Now)
		if len(boarded) > 0 {
			ss := s.Stats[stop.ID]
			for _, p := range boarded {
				if p.WaitDuration != nil { ss.sumWait += *p.WaitDuration }
			}
			ss.Boarded += len(boarded)
		}
		// Record remaining queues
		ssCurr := s.Stats[stop.ID]
		ssCurr.RemainingOutbound = len(stop.OutboundQueue)
		ssCurr.RemainingInbound = len(stop.InboundQueue)

		// If last stop, force alight any remaining passengers and finish
		if idx == len(s.Route.Stops)-1 {
			if len(s.Bus.Passengers) > 0 {
				alighted := s.Bus.AlightPassengersAtCurrentStop(s.Now)
				if len(alighted) > 0 { for _, p := range alighted { s.Completed = append(s.Completed, p) } }
			}
			break
		}

		// Determine departure (with simple dwell formula)
		dwellSeconds := 15 + 2*len(boarded) + 1*len(alighted)
		s.Now = s.Now.Add(time.Duration(dwellSeconds) * time.Second)

		// Travel to next stop
		next := s.Route.Stops[idx+1]
		distance := stop.DistanceToNext
		travelMinutes := distance / s.Bus.AverageSpeedKmph * 60.0
		travelDur := time.Duration(travelMinutes * float64(time.Minute))

		// During travel, generate passenger arrivals at downstream stops (excluding current and final already passed)
		intervalStart := s.Now
		intervalEnd := s.Now.Add(travelDur)
		s.generateArrivals(intervalStart, intervalEnd, idx+1)

		// Advance time to arrival at next stop
		s.Now = intervalEnd
		s.Bus.CurrentStopID = next.ID
	}

	// Finalize average waits
	for _, st := range s.Stats {
		if st.Boarded > 0 { st.AvgWaitMinutes = st.sumWait / float64(st.Boarded) }
	}
}

func (s *Simulator) generateArrivals(start, end time.Time, fromIndex int) {
	durMinutes := end.Sub(start).Minutes()
	if durMinutes <= 0 { return }
	for i := fromIndex; i < len(s.Route.Stops)-1; i++ { // exclude last stop
		stop := s.Route.Stops[i]
		mean := s.LambdaPerMinute * durMinutes
		count := s.poisson(mean)
		if count == 0 { continue }
		ss := s.Stats[stop.ID]
		for j := 0; j < count; j++ {
			// destination strictly downstream
			destIdx := i + 1 + s.RNG.Intn(len(s.Route.Stops)-i-1)
			dest := s.Route.Stops[destIdx]
			// uniform arrival time in interval
			t := start.Add(time.Duration(s.RNG.Float64()*durMinutes*float64(time.Minute)))
			p := s.newPassenger(stop.ID, dest.ID, t)
			stop.EnqueuePassenger(p, "outbound", t)
			ss.ArrivalsGenerated++
		}
		ss.RemainingOutbound = len(stop.OutboundQueue)
		ss.RemainingInbound = len(stop.InboundQueue)
	}
}

func (s *Simulator) newPassenger(origin, dest int, arrival time.Time) *model.Passenger {
	s.PassengerID++
	// Determine direction by index positions (simplistic: origin index < dest index => outbound)
	dir := "outbound"
	originIdx := -1
	destIdx := -1
	for i, st := range s.Route.Stops {
		if st.ID == origin { originIdx = i }
		if st.ID == dest { destIdx = i }
	}
	if originIdx >=0 && destIdx >=0 && destIdx < originIdx { dir = "inbound" }
	return &model.Passenger{
		ID:             s.PassengerID,
		RouteID:        s.Route.ID,
		StartStopID:    origin,
		EndStopID:      dest,
		Direction:      dir,
		ArrivalStopTime: arrival,
	}
}

// NewPassengerPublic exposes passenger creation for streaming mode.
func (s *Simulator) NewPassengerPublic(origin, dest int, arrival time.Time) *model.Passenger { return s.newPassenger(origin, dest, arrival) }

// Poisson sample with mean using Knuth algorithm (suitable for moderate means).
func (s *Simulator) poisson(mean float64) int {
	if mean <= 0 { return 0 }
	if mean > 30 { // For large means, use normal approximation then adjust (simple approach)
		std := math.Sqrt(mean)
		val := int(math.Round(s.RNG.NormFloat64()*std + mean))
		if val < 0 { return 0 }
		return val
	}
	L := math.Exp(-mean)
	k := 0
	p := 1.0
	for p > L {
		k++
		p *= s.RNG.Float64()
	}
	return k - 1
}

// PoissonPublic exposes sampling for external stepwise simulation.
func (s *Simulator) PoissonPublic(mean float64) int { return s.poisson(mean) }
