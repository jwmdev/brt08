package model

import "time"

// Passenger represents a single trip request / rider through the system.
type Passenger struct {
    ID                int        `json:"id"`
    RouteID           int        `json:"route_id"`
    StartStopID       int        `json:"start_stop_id"`
    EndStopID         int        `json:"end_stop_id"`
    ArrivalStopTime   time.Time  `json:"arrival_stop_time"`   // when passenger arrived at origin stop (intending to travel)
    BoardingTime      *time.Time `json:"boarding_time,omitempty"`      // when passenger actually boarded a bus
    WaitDuration      *float64   `json:"wait_duration_minutes,omitempty"` // (BoardingTime - ArrivalStopTime) in minutes
    DepartureTime     *time.Time `json:"departure_time,omitempty"`     // same as BoardingTime, explicit for clarity
    ArrivalDestTime   *time.Time `json:"arrival_destination_time,omitempty"` // when passenger alights at destination
}

// MarkBoarded sets the boarding / departure time and computes wait duration.
func (p *Passenger) MarkBoarded(ts time.Time) {
    p.BoardingTime = &ts
    p.DepartureTime = &ts
    diff := ts.Sub(p.ArrivalStopTime).Minutes()
    if diff < 0 {
        diff = 0
    }
    d := diff
    p.WaitDuration = &d
}

// MarkArrived sets arrival at destination.
func (p *Passenger) MarkArrived(ts time.Time) {
    p.ArrivalDestTime = &ts
}

// IsOnboard returns true if passenger has boarded but not yet arrived at destination.
func (p *Passenger) IsOnboard() bool {
    return p.BoardingTime != nil && p.ArrivalDestTime == nil
}

// Completed returns true if the journey finished.
func (p *Passenger) Completed() bool {
    return p.ArrivalDestTime != nil
}
