package model

import "time"

// BusType represents a category of buses with cost and capacity attributes.
type BusType struct {
	ID        int     `json:"id"`
	Name      string  `json:"name"`
	Capacity  int     `json:"capacity"`
	CostPerKm float64 `json:"cost_per_km"`
}

// Bus represents an individual bus in operation.
type Bus struct {
	ID                int          `json:"id"`
	Type              *BusType     `json:"type"`
	RouteID           int          `json:"route_id"`
	CurrentStopID     int          `json:"current_stop_id"`
	Direction         string       `json:"direction"` // "outbound" or "inbound"
	PassengersOnboard int          `json:"passengers_onboard"`
	IsFull            bool         `json:"is_full"`
	AverageSpeedKmph  float64      `json:"average_speed_kmph"`
	// Detailed passenger tracking
	Passengers    []*Passenger `json:"passengers,omitempty"`
	TotalBoarded  int          `json:"total_boarded"`
	TotalAlighted int          `json:"total_alighted"`
}



// LoadPassengers attempts to board up to n passengers.
// It returns the number actually boarded (0..n).
func (b *Bus) LoadPassengers(n int) int {
	if n <= 0 {
		return 0
	}
	if b.Type == nil || b.Type.Capacity <= 0 {
		return 0
	}
	remaining := b.Type.Capacity - b.PassengersOnboard
	if remaining <= 0 {
		b.IsFull = true
		return 0
	}
	boarded := n
	if boarded > remaining {
		boarded = remaining
	}
	b.PassengersOnboard += boarded
	if b.PassengersOnboard >= b.Type.Capacity {
		b.PassengersOnboard = b.Type.Capacity
		b.IsFull = true
	} else {
		b.IsFull = false
	}
	return boarded
}

// UnloadPassengers attempts to disembark up to n passengers.
// It returns the number actually removed (0..n).
func (b *Bus) UnloadPassengers(n int) int {
	if n <= 0 {
		return 0
	}
	if b.PassengersOnboard <= 0 {
		b.PassengersOnboard = 0
		b.IsFull = false
		return 0
	}
	removed := n
	if removed > b.PassengersOnboard {
		removed = b.PassengersOnboard
	}
	b.PassengersOnboard -= removed
	if b.Type != nil && b.PassengersOnboard >= b.Type.Capacity {
		b.PassengersOnboard = b.Type.Capacity
		b.IsFull = true
	} else {
		b.IsFull = false
	}
	return removed
}

// SetSpeedKmph updates the average speed (bounded to reasonable range).
func (b *Bus) SetSpeedKmph(v float64) {
	if v < 0 {
		v = 0
	}
	if v > 120 { // safety cap
		v = 120
	}
	b.AverageSpeedKmph = v
}

// RemainingCapacity returns how many more passengers can board.
func (b *Bus) RemainingCapacity() int {
	if b.Type == nil {
		return 0
	}
	rem := b.Type.Capacity - b.PassengersOnboard
	if rem < 0 {
		return 0
	}
	return rem
}

// OccupancyRatio returns the fraction (0..1) of seats occupied.
func (b *Bus) OccupancyRatio() float64 {
	if b.Type == nil || b.Type.Capacity == 0 {
		return 0
	}
	return float64(b.PassengersOnboard) / float64(b.Type.Capacity)
}

// BoardPassengersAtStop boards as many waiting passengers (in order) whose StartStopID matches stopID
// up to remaining capacity. Returns slices of boarded passengers and remaining waiting passengers.
// Passengers whose route or start stop don't match are left in remaining unchanged.
func (b *Bus) BoardPassengersAtStop(stopID int, waiting []*Passenger, now time.Time) (boarded []*Passenger, remaining []*Passenger) {
	if stopID == 0 || len(waiting) == 0 || b.Type == nil {
		return nil, waiting
	}
	capacityLeft := b.RemainingCapacity()
	if capacityLeft <= 0 {
		b.IsFull = true
		return nil, waiting
	}
	for _, p := range waiting {
		// Only consider those starting here & on same route & not already boarded
		if capacityLeft > 0 && p.RouteID == b.RouteID && p.StartStopID == stopID && p.BoardingTime == nil {
			p.MarkBoarded(now)
			b.Passengers = append(b.Passengers, p)
			boarded = append(boarded, p)
			capacityLeft--
			b.TotalBoarded++
		} else {
			remaining = append(remaining, p)
		}
	}
	b.PassengersOnboard = len(b.Passengers)
	if b.PassengersOnboard >= b.Type.Capacity {
		b.IsFull = true
	} else {
		b.IsFull = false
	}
	return boarded, remaining
}

// AlightPassengersAtCurrentStop removes passengers whose EndStopID matches the bus CurrentStopID
// marking their arrival time. Returns slice of alighted passengers.
func (b *Bus) AlightPassengersAtCurrentStop(now time.Time) (alighted []*Passenger) {
	if len(b.Passengers) == 0 {
		return nil
	}
	keep := make([]*Passenger, 0, len(b.Passengers))
	for _, p := range b.Passengers {
		if p.EndStopID == b.CurrentStopID && p.IsOnboard() {
			p.MarkArrived(now)
			alighted = append(alighted, p)
			b.TotalAlighted++
		} else {
			keep = append(keep, p)
		}
	}
	b.Passengers = keep
	b.PassengersOnboard = len(b.Passengers)
	if b.Type != nil && b.PassengersOnboard >= b.Type.Capacity {
		b.IsFull = true
	} else {
		b.IsFull = false
	}
	return alighted
}

// AdvanceToStop updates the bus to a new stop, first alighting passengers, then boarding from provided queue.
// Returns (alighted, boarded, remainingQueue).
func (b *Bus) AdvanceToStop(stopID int, waitingQueue []*Passenger, now time.Time) (alighted []*Passenger, boarded []*Passenger, remaining []*Passenger) {
	// Move bus
	b.CurrentStopID = stopID
	// Alight
	alighted = b.AlightPassengersAtCurrentStop(now)
	// Board
	boarded, remaining = b.BoardPassengersAtStop(stopID, waitingQueue, now)
	return alighted, boarded, remaining
}
