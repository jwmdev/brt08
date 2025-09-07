package model

// Route models an ordered sequence of bus stops in one direction.
type Route struct {
    ID              int        `json:"id"`
    Name            string     `json:"route"`
    Direction       string     `json:"direction"`
    TotalDistanceKM float64    `json:"total_distance_km"`
    UnitDistance    string     `json:"unit_distance"`
    Stops           []*BusStop `json:"stops"`
}

// GetStop returns the stop by id.
func (r *Route) GetStop(id int) *BusStop {
    for _, s := range r.Stops {
        if s.ID == id {
            return s
        }
    }
    return nil
}

// IndexOf returns index of stop id or -1.
func (r *Route) IndexOf(id int) int {
    for i, s := range r.Stops {
        if s.ID == id {
            return i
        }
    }
    return -1
}

// NextStopID returns id of next stop or 0.
func (r *Route) NextStopID(current int) int {
    idx := r.IndexOf(current)
    if idx == -1 || idx+1 >= len(r.Stops) {
        return 0
    }
    return r.Stops[idx+1].ID
}

// PreviousStopID returns previous stop id or 0.
func (r *Route) PreviousStopID(current int) int {
    idx := r.IndexOf(current)
    if idx <= 0 {
        return 0
    }
    return r.Stops[idx-1].ID
}
