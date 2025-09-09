package model

import (
    "encoding/json"
    "fmt"
    "io"
)

// raw structures matching the JSON file
type rawRoute struct {
    Name            string       `json:"route"`
    Direction       string       `json:"direction"`
    UnitDistance    string       `json:"unit_distance"`
    TotalDistanceKM float64      `json:"total_distance_km"`
    Stops           []rawStop    `json:"stops"`
    Pins            []rawPin     `json:"pins"`
}

type rawStop struct {
    StopID           int     `json:"stop_id"`
    StopName         string  `json:"stop_name"`
    Lat              float64 `json:"latitute"`
    Lng              float64 `json:"longtude"`
    DistanceNext     float64 `json:"distance_next_stop"`
}

type rawPin struct {
    LeftStopID  int     `json:"left_stop_id"`
    RightStopID int     `json:"right_stop_id"`
    Lat         float64 `json:"latitute"`
    Lng         float64 `json:"longtude"`
}

// LoadRouteFromReader parses a route JSON (kimara_kivukoni_stops.json format) and builds a Route struct.
func LoadRouteFromReader(r io.Reader, id int) (*Route, error) {
    dec := json.NewDecoder(r)
    var raw rawRoute
    if err := dec.Decode(&raw); err != nil {
        return nil, fmt.Errorf("decode route: %w", err)
    }
    route := &Route{
        ID:              id,
        Name:            raw.Name,
        Direction:       raw.Direction,
        TotalDistanceKM: raw.TotalDistanceKM,
        UnitDistance:    raw.UnitDistance,
        Stops:           make([]*BusStop, 0, len(raw.Stops)),
        Pins:            make([]*RoutePin, 0, len(raw.Pins)),
    }
    var cumulative float64
    for _, s := range raw.Stops {
        bs := &BusStop{
            ID:             s.StopID,
            Name:           s.StopName,
            RouteID:        id,
            Latitude:       s.Lat,
            Longitude:      s.Lng,
            DistanceToNext: s.DistanceNext,
            CumulativeDist: cumulative,
        }
        cumulative += s.DistanceNext
        route.Stops = append(route.Stops, bs)
    }
    for _, p := range raw.Pins {
        rp := &RoutePin{LeftStopID: p.LeftStopID, RightStopID: p.RightStopID, Latitude: p.Lat, Longitude: p.Lng}
        route.Pins = append(route.Pins, rp)
    }
    return route, nil
}
