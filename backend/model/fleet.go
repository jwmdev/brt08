package model

import (
    "encoding/json"
    "fmt"
    "io"
    "math"
    "math/rand"
)

// FleetFile maps the layout of backend/data/fleet.json
type FleetFile struct {
    BusTypes []BusType       `json:"bus_types"`
    Fleet    []FleetQuantity `json:"fleet"`
}

// FleetQuantity declares how many vehicles of a given type to deploy
type FleetQuantity struct {
    TypeID   int `json:"type_id"`
    Quantity int `json:"quantity"`
}

// LoadFleetFromReader parses a fleet JSON file and returns types indexed by id and the requested quantities.
func LoadFleetFromReader(r io.Reader) (map[int]*BusType, []FleetQuantity, error) {
    dec := json.NewDecoder(r)
    var ff FleetFile
    if err := dec.Decode(&ff); err != nil {
        return nil, nil, fmt.Errorf("decode fleet: %w", err)
    }
    types := make(map[int]*BusType, len(ff.BusTypes))
    for i := range ff.BusTypes {
        bt := ff.BusTypes[i] // copy
        // ensure sane values
        if bt.Capacity < 1 { bt.Capacity = 60 }
        if bt.CostPerKm < 0 { bt.CostPerKm = 0 }
        types[bt.ID] = &bt
    }
    // filter out non-positive quantities
    q := make([]FleetQuantity, 0, len(ff.Fleet))
    for _, it := range ff.Fleet {
        if it.Quantity > 0 && it.TypeID != 0 {
            q = append(q, it)
        }
    }
    return types, q, nil
}

// randomSpeedForType returns a plausible average speed (km/h) for a bus type.
// Uses a truncated normal distribution around a type-specific mean.
func randomSpeedForType(rng *rand.Rand, t *BusType) float64 {
    mean := 28.0
    std := 3.5
    // Light heuristic by type name/capacity
    if t != nil {
        if t.Capacity >= 120 { // articulated tend to be a bit slower
            mean = 25.0
            std = 3.0
        } else if t.Capacity <= 70 {
            mean = 28.0
            std = 4.0
        }
        // name-based tweak
        if t.Name != "" {
            switch {
            case containsFold(t.Name, "articulated"):
                mean = 25.0
                std = 3.0
            case containsFold(t.Name, "standard"):
                mean = 28.0
                std = 4.0
            }
        }
    }
    // sample truncated to [15, 45]
    v := rng.NormFloat64()*std + mean
    if v < 15 { v = 15 }
    if v > 45 { v = 45 }
    // round to one decimal for nicer display
    return math.Round(v*10) / 10
}

// BuildFleetBuses creates concrete Bus instances according to fleet quantities.
// Each bus is assigned a randomized average speed and alternating starting directions.
func BuildFleetBuses(types map[int]*BusType, q []FleetQuantity, routeID int, firstStopID, lastStopID int, rng *rand.Rand) []*Bus {
    buses := make([]*Bus, 0)
    id := 1
    for _, it := range q {
        bt := types[it.TypeID]
        if bt == nil { continue }
        for i := 0; i < it.Quantity; i++ {
            dir := "outbound"
            if rng.Intn(2) == 1 { dir = "inbound" }
            startStop := firstStopID
            if dir == "inbound" { startStop = lastStopID }
            b := &Bus{
                ID:               id,
                Type:             bt,
                RouteID:          routeID,
                CurrentStopID:    startStop,
                Direction:        dir,
                AverageSpeedKmph: randomSpeedForType(rng, bt),
            }
            buses = append(buses, b)
            id++
        }
    }
    return buses
}

// containsFold reports whether substr is within s, case-insensitive ASCII.
func containsFold(s, substr string) bool {
    // simple ASCII fold; acceptable for identifier-like names
    toLower := func(r byte) byte {
        if r >= 'A' && r <= 'Z' { return r + 32 }
        return r
    }
    if len(substr) == 0 { return true }
    n := len(s) - len(substr)
    if n < 0 { return false }
    for i := 0; i <= n; i++ {
        j := 0
        for ; j < len(substr); j++ {
            if toLower(s[i+j]) != toLower(substr[j]) { break }
        }
        if j == len(substr) { return true }
    }
    return false
}
