package sim

import (
	"fmt"
	"log"
	"math"
	"os"
	"path/filepath"
	"time"

	"brt08/backend/model"
)

// ReportSummary carries end-of-run metrics needed for reporting.
type ReportSummary struct {
	Generated   int
	Served      int64
	AvgWaitMin  float64
	BusDistance map[int]float64 // km per bus id
}

// WriteCSVReport writes a CSV report to the given path or directory.
// If reportPath is a directory, it creates a timestamped file inside.
// If reportPath is a file, a timestamp is suffixed before the extension.
func WriteCSVReport(reportPath string, buses []*model.Bus, sum ReportSummary) (string, error) {
	if reportPath == "" {
		return "", nil
	}
	ts := time.Now().Format("20060102-150405")
	outPath := reportPath
	if fi, err := os.Stat(outPath); err == nil && fi.IsDir() {
		outPath = filepath.Join(outPath, fmt.Sprintf("report-%s.csv", ts))
	} else if outPath != "" {
		ext := filepath.Ext(outPath)
		base := outPath[:len(outPath)-len(ext)]
		outPath = fmt.Sprintf("%s-%s%s", base, ts, ext)
	}
	f, err := os.Create(outPath)
	if err != nil {
		return "", err
	}
	defer f.Close()
	fmt.Fprintln(f, "section,bus_id,direction,type,avg_speed_kmph,distance_km,cost,generated,served,avg_wait_min,buses_count,timestamp")
	round2 := func(x float64) float64 { return math.Round(x*100) / 100 }
	for _, b := range buses {
		d := round2(sum.BusDistance[b.ID])
		c := 0.0
		typeName := ""
		if b.Type != nil {
			c = round2(float64(b.Type.CostPerKm) * d)
			typeName = b.Type.Name
		}
		fmt.Fprintf(f, "bus,%d,%s,%s,%.1f,%.2f,%.2f,,,,,%s\n", b.ID, b.Direction, typeName, b.AverageSpeedKmph, d, c, ts)
	}
	totalCost := 0.0
	for _, b := range buses {
		if b.Type != nil {
			d := round2(sum.BusDistance[b.ID])
			totalCost += round2(float64(b.Type.CostPerKm) * d)
		}
	}
	fmt.Fprintf(f, "summary,,,,,,%.2f,%d,%d,%.2f,%d,%s\n", totalCost, sum.Generated, sum.Served, sum.AvgWaitMin, len(buses), ts)
	log.Printf("CSV report written to %s", outPath)
	return outPath, nil
}

// PrintConsoleReport prints a human-readable report to stdout.
func PrintConsoleReport(buses []*model.Bus, sum ReportSummary) {
	totalCost := 0.0
	totalDist := 0.0
	fmt.Println("=== Simulation Report ===")
	fmt.Printf("Buses on route: %d\n", len(buses))
	fmt.Printf("Passengers generated: %d\n", sum.Generated)
	fmt.Printf("Passengers served: %d\n", sum.Served)
	fmt.Printf("Average wait: %.2f minutes\n", sum.AvgWaitMin)
	round2 := func(x float64) float64 { return math.Round(x*100) / 100 }
	for _, b := range buses {
		d := round2(sum.BusDistance[b.ID])
		c := 0.0
		if b.Type != nil {
			c = round2(float64(b.Type.CostPerKm) * d)
		}
		totalDist += d
		totalCost += c
		name := ""
		if b.Type != nil {
			name = b.Type.Name
		}
		fmt.Printf("Bus %d (%s, %s) distance=%.2f km cost=%.2f\n", b.ID, b.Direction, name, d, c)
	}
	fmt.Printf("Total distance: %.2f km\n", totalDist)
	fmt.Printf("Total operating cost: %.2f\n", totalCost)
}
