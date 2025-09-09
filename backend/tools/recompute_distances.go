package main

import (
	"encoding/json"
	"fmt"
	"math"
	"os"
)

type Stop struct {
	StopID          int     `json:"stop_id"`
	StopName        string  `json:"stop_name"`
	Lat             float64 `json:"latitute"`
	Lng             float64 `json:"longtude"`
	DistanceNextRaw float64 `json:"distance_next_stop"`
}

type Pin struct {
	LeftStopID  int     `json:"left_stop_id"`
	RightStopID int     `json:"right_stop_id"`
	Lat         float64 `json:"latitute"`
	Lng         float64 `json:"longtude"`
}

type RouteFile struct {
	Route          string  `json:"route"`
	Direction      string  `json:"direction"`
	UnitDistance   string  `json:"unit_distance"`
	TotalDistance  float64 `json:"total_distance_km"`
	Stops          []Stop  `json:"stops"`
	Pins           []Pin   `json:"pins"`
	Note           string  `json:"note"`
}

// haversine distance in km
func haversine(lat1, lon1, lat2, lon2 float64) float64 {
	const R = 6371.0088 // mean Earth radius km
	dLat := (lat2 - lat1) * math.Pi / 180
	dLon := (lon2 - lon1) * math.Pi / 180
	la1 := lat1 * math.Pi / 180
	la2 := lat2 * math.Pi / 180
	a := math.Sin(dLat/2)*math.Sin(dLat/2) + math.Cos(la1)*math.Cos(la2)*math.Sin(dLon/2)*math.Sin(dLon/2)
	c := 2 * math.Atan2(math.Sqrt(a), math.Sqrt(1-a))
	return R * c
}

func main() {
	if len(os.Args) < 2 {
		fmt.Println("usage: recompute_distances <json-file>")
		os.Exit(1)
	}
	path := os.Args[1]
	b, err := os.ReadFile(path)
	if err != nil { panic(err) }
	var rf RouteFile
	if err := json.Unmarshal(b, &rf); err != nil { panic(err) }

	// Index pins between stop pairs
	pinsByPair := make(map[[2]int][]Pin)
	for _, p := range rf.Pins {
		key := [2]int{p.LeftStopID, p.RightStopID}
		pinsByPair[key] = append(pinsByPair[key], p)
	}

	var total float64
	for i := 0; i < len(rf.Stops)-1; i++ {
		a := rf.Stops[i]
		bStop := rf.Stops[i+1]
		// Build sequence: start stop, pins (in insertion order), end stop
		seq := [][2]float64{{a.Lat, a.Lng}}
		if ps, ok := pinsByPair[[2]int{a.StopID, bStop.StopID}]; ok {
			for _, p := range ps { seq = append(seq, [2]float64{p.Lat, p.Lng}) }
		}
		seq = append(seq, [2]float64{bStop.Lat, bStop.Lng})
		// Sum segment distances
		segDist := 0.0
		for j := 0; j < len(seq)-1; j++ {
			segDist += haversine(seq[j][0], seq[j][1], seq[j+1][0], seq[j+1][1])
		}
		// Round to 3 decimals for storage
		segDistRounded := math.Round(segDist*1000) / 1000
		rf.Stops[i].DistanceNextRaw = segDistRounded
		total += segDist
	}
	// Last stop distance_next_stop stays 0
	lastIdx := len(rf.Stops) - 1
	if lastIdx >= 0 { rf.Stops[lastIdx].DistanceNextRaw = 0 }
	// Update total distance
	 rf.TotalDistance = math.Round(total*1000) / 1000

	// Marshal updated JSON preserving structure
	out, err := json.MarshalIndent(rf, "", "  ")
	if err != nil { panic(err) }
	if err := os.WriteFile(path, out, 0644); err != nil { panic(err) }
	fmt.Printf("Updated distances. New total_distance_km=%.3f\n", rf.TotalDistance)
}
