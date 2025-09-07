package model

import "time"

// BusStop holds separate queues for outbound and inbound passengers.
type BusStop struct {
    ID              int           `json:"id"`
    Name            string        `json:"name"`
    RouteID         int           `json:"route_id"`
    Latitude        float64       `json:"latitute"`
    Longitude       float64       `json:"longtude"`
    DistanceToNext  float64       `json:"distance_next_stop"`
    CumulativeDist  float64       `json:"cumulative_distance_km"`
    OutboundQueue   []*Passenger  `json:"outbound_queue,omitempty"`
    InboundQueue    []*Passenger  `json:"inbound_queue,omitempty"`
    TotalArrivals   int           `json:"total_arrivals"`
    TotalBoarded    int           `json:"total_boarded"`
    TotalDepartures int           `json:"total_departures"` // passengers leaving the queue (boarded)
}

// EnqueuePassenger adds a passenger to the correct directional queue and stamps arrival time if zero.
func (s *BusStop) EnqueuePassenger(p *Passenger, dir string, now time.Time) {
    if p == nil {
        return
    }
    if p.ArrivalStopTime.IsZero() {
        p.ArrivalStopTime = now
    }
    s.TotalArrivals++
    if dir == "inbound" {
        s.InboundQueue = append(s.InboundQueue, p)
    } else { // default outbound
        s.OutboundQueue = append(s.OutboundQueue, p)
    }
}

// BoardAtStop boards passengers from the specified direction queue onto the bus.
// Returns slice of boarded passengers.
func (s *BusStop) BoardAtStop(bus *Bus, now time.Time) []*Passenger {
    if bus == nil {
        return nil
    }
    var queue *[]*Passenger
    if bus.Direction == "inbound" {
        queue = &s.InboundQueue
    } else {
        queue = &s.OutboundQueue
    }
    if len(*queue) == 0 {
        return nil
    }
    remaining := bus.RemainingCapacity()
    if remaining <= 0 {
        return nil
    }
    boarded := make([]*Passenger, 0, remaining)
    newQueue := make([]*Passenger, 0, len(*queue))
    for _, p := range *queue {
        if remaining <= 0 { // capacity reached, keep rest
            newQueue = append(newQueue, p)
            continue
        }
        if p.RouteID == bus.RouteID && p.StartStopID == s.ID && p.BoardingTime == nil {
            p.MarkBoarded(now)
            bus.Passengers = append(bus.Passengers, p)
            boarded = append(boarded, p)
            bus.TotalBoarded++
            s.TotalBoarded++
            s.TotalDepartures++
            remaining--
        } else {
            newQueue = append(newQueue, p)
        }
    }
    *queue = newQueue
    bus.PassengersOnboard = len(bus.Passengers)
    if bus.Type != nil && bus.PassengersOnboard >= bus.Type.Capacity {
        bus.IsFull = true
    } else {
        bus.IsFull = false
    }
    return boarded
}
