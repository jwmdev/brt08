package server

import (
	"brt08/backend/model"
	"brt08/backend/sim"
	"encoding/json"
	"fmt"
	"log"
	"math/rand"
	"net/http"
	"strconv"
	"sync"
	"sync/atomic"
	"time"
)

// ctrlAdapter bridges server connControl to sim.Control.
type ctrlAdapter struct{ c *connControl }

func (a ctrlAdapter) Speed() float64 {
	if a.c == nil {
		return 1
	}
	v := a.c.speed.Load()
	if v == nil {
		return 1
	}
	f := v.(float64)
	if f < 0.1 {
		f = 0.1
	}
	if f > 10 {
		f = 10
	}
	return f
}
func (a ctrlAdapter) ArrivalFactor() float64 {
	if a.c == nil {
		return 1
	}
	v := a.c.arrivalMult.Load()
	if v == nil {
		return 1
	}
	f := v.(float64)
	if f < 0.1 {
		f = 0.1
	}
	if f > 50 {
		f = 50
	}
	return f
}

// connControl holds per-stream tunables.
type connControl struct {
	speed       atomic.Value
	arrivalMult atomic.Value
}

// Options configures the server instance.
type Options struct {
	PeriodID              int
	SpatialGradient       float64
	BaselineDemand        float64
	DefaultSpeed          float64
	DefaultArrivalFactor  float64
	ReportPath            string
	Seed                  int64
	TraceBusID            int
	PassengerCap          int
	MorningTowardKivukoni bool
	DirBias               float64
}

type Server struct {
	Route *model.Route
	Fleet []*model.Bus
	Opt   Options

	streamControls sync.Map // map[connID]*connControl
}

func New(route *model.Route, fleet []*model.Bus, opt Options) *Server {
	return &Server{Route: route, Fleet: fleet, Opt: opt}
}

// Serve registers HTTP handlers on default mux.
func (s *Server) Serve() {
	routeHandler := func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		w.Header().Set("Access-Control-Allow-Origin", "*")
		j, _ := json.Marshal(s.Route)
		w.Write(j)
	}
	http.HandleFunc("/api/route", routeHandler)
	http.HandleFunc("/api/route.json", routeHandler)
	http.HandleFunc("/api/routejson", routeHandler)
	http.HandleFunc("/api/control", s.handleControl)
	http.HandleFunc("/api/stream", s.handleStream)
}

func (s *Server) handleControl(w http.ResponseWriter, r *http.Request) {
	w.Header().Set("Access-Control-Allow-Origin", "*")
	w.Header().Set("Access-Control-Allow-Headers", "Content-Type")
	if r.Method == http.MethodOptions {
		w.WriteHeader(204)
		return
	}
	var req struct {
		ConnID        string  `json:"conn_id"`
		Speed         float64 `json:"speed"`
		ArrivalFactor float64 `json:"arrival_factor"`
	}
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		http.Error(w, "bad json", 400)
		return
	}
	v, ok := s.streamControls.Load(req.ConnID)
	if !ok {
		http.Error(w, "connection not found", 404)
		return
	}
	c := v.(*connControl)
	if req.Speed != 0 {
		sp := req.Speed
		if sp <= 0 {
			sp = 1
		}
		if sp > 10.0 {
			sp = 10.0
		}
		c.speed.Store(sp)
		log.Printf("control: conn=%s speed=%.2fx", req.ConnID, sp)
	}
	if req.ArrivalFactor != 0 {
		af := req.ArrivalFactor
		if af <= 0 {
			af = 1
		}
		if af < 0.1 {
			af = 0.1
		}
		if af > 50.0 {
			af = 50.0
		}
		c.arrivalMult.Store(af)
	}
	w.WriteHeader(204)
}

func (s *Server) handleStream(w http.ResponseWriter, r *http.Request) {
	w.Header().Set("Content-Type", "text/event-stream")
	w.Header().Set("Cache-Control", "no-cache")
	w.Header().Set("Connection", "keep-alive")
	w.Header().Set("Access-Control-Allow-Origin", "*")
	flusher, ok := w.(http.Flusher)
	if !ok {
		http.Error(w, "stream unsupported", 500)
		return
	}

	// Per-connection clones
	seedBase := s.Opt.Seed
	if seedBase == 0 {
		seedBase = time.Now().UnixNano()
	}
	engineSeed := seedBase + 1
	connBuses := make([]*model.Bus, 0, len(s.Fleet))
	for _, proto := range s.Fleet {
		b := &model.Bus{ID: proto.ID, Type: proto.Type, RouteID: proto.RouteID, CurrentStopID: proto.CurrentStopID, Direction: proto.Direction, AverageSpeedKmph: proto.AverageSpeedKmph}
		connBuses = append(connBuses, b)
	}
	start := time.Now()
	lambda := 1.2
	if qs := r.URL.Query().Get("lambda"); qs != "" {
		if v, err := strconv.ParseFloat(qs, 64); err == nil && v > 0 {
			lambda = v
		}
	}
	connID := fmt.Sprintf("%d-%d", time.Now().UnixNano(), rand.Int63())
	ctrl := &connControl{}
	initSpeed := s.Opt.DefaultSpeed
	if qs := r.URL.Query().Get("speed"); qs != "" {
		if v, err := strconv.ParseFloat(qs, 64); err == nil && v > 0 {
			initSpeed = v
		}
	}
	if initSpeed < 0.1 {
		initSpeed = 0.1
	}
	if initSpeed > 10.0 {
		initSpeed = 10.0
	}
	ctrl.speed.Store(initSpeed)
	initArr := s.Opt.DefaultArrivalFactor
	if qs := r.URL.Query().Get("arrival_factor"); qs != "" {
		if v, err := strconv.ParseFloat(qs, 64); err == nil && v > 0 {
			initArr = v
		}
	}
	if initArr < 0.1 {
		initArr = 0.1
	}
	if initArr > 50.0 {
		initArr = 50.0
	}
	ctrl.arrivalMult.Store(initArr)
	s.streamControls.Store(connID, ctrl)
	defer s.streamControls.Delete(connID)

	// Serialize writer
	var writeMu sync.Mutex
	flush := func(event string, payload any) {
		writeMu.Lock()
		b, _ := json.Marshal(payload)
		fmt.Fprintf(w, "event: %s\n", event)
		fmt.Fprintf(w, "data: %s\n\n", string(b))
		flusher.Flush()
		writeMu.Unlock()
	}
	// Always use channel-based engine (runner) unless explicitly requested legacy
	useLegacy := r.URL.Query().Get("engine") == "legacy"
	if !useLegacy {
		// Build control adapter to read live controls
		var _ sim.Control = ctrlAdapter{}
		evCh, stopFn, waitFn := sim.StartRunner(s.Route, connBuses, engineSeed, lambda, struct {
			PeriodID              int
			PassengerCap          int
			MorningTowardKivukoni bool
			DirBias               float64
			SpatialGradient       float64
			BaselineDemand        float64
			TraceBusID            int
			ConnID                string
			Start                 time.Time
		}{PeriodID: s.Opt.PeriodID, PassengerCap: s.Opt.PassengerCap, MorningTowardKivukoni: s.Opt.MorningTowardKivukoni, DirBias: s.Opt.DirBias, SpatialGradient: s.Opt.SpatialGradient, BaselineDemand: s.Opt.BaselineDemand, TraceBusID: s.Opt.TraceBusID, ConnID: connID, Start: start}, ctrlAdapter{c: ctrl})

		// Ensure cleanup if client disconnects early
		defer stopFn()
		defer waitFn()

		// Capture final metrics for reporting
		var finalDone *sim.DoneEvent
		for e := range evCh {
			switch ev := e.(type) {
			case sim.InitEvent:
				flush("init", map[string]any{"time": ev.Time, "buses": []any{}, "message": "started", "conn_id": ev.ConnID, "generated_passengers": ev.Generated, "outbound_generated": ev.OutboundGen, "inbound_generated": ev.InboundGen, "served_passengers": 0, "avg_wait_min": ev.AvgWaitMin, "arrival_factor": ev.ArrivalFactor})
			case sim.StopUpdateEvent:
				flush("stop_update", map[string]any{"stop_id": ev.StopID, "outbound_queue": ev.OutboundQueue, "inbound_queue": ev.InboundQueue, "generated_passengers": ev.Generated, "outbound_generated": ev.OutboundGenerated, "inbound_generated": ev.InboundGenerated})
			case sim.BusAddEvent:
				flush("bus_add", map[string]any{"bus_id": ev.BusID, "direction": ev.Direction, "avg_speed_kmph": ev.AvgSpeedKmph, "capacity": ev.Capacity})
			case sim.ArriveEvent:
				flush("arrive", map[string]any{"bus_id": ev.BusID, "direction": ev.Direction, "stop_id": ev.StopID, "time": ev.Time, "bus_onboard": ev.BusOnboard, "passengers_onboard": ev.PassengersOnboard, "generated_passengers": ev.Generated, "outbound_generated": ev.OutboundGenerated, "inbound_generated": ev.InboundGenerated})
			case sim.AlightEvent:
				flush("alight", map[string]any{"bus_id": ev.BusID, "direction": ev.Direction, "stop_id": ev.StopID, "alighted": ev.Alighted, "bus_onboard": ev.BusOnboard, "passengers_onboard": ev.PassengersOnboard, "generated_passengers": ev.Generated, "outbound_generated": ev.OutboundGenerated, "inbound_generated": ev.InboundGenerated, "final": ev.Final, "served_passengers": ev.ServedPassengers})
			case sim.BoardEvent:
				flush("board", map[string]any{"bus_id": ev.BusID, "direction": ev.Direction, "stop_id": ev.StopID, "boarded": ev.Boarded, "bus_onboard": ev.BusOnboard, "passengers_onboard": ev.PassengersOnboard, "stop_outbound": ev.StopOutbound, "stop_inbound": ev.StopInbound, "generated_passengers": ev.Generated, "outbound_generated": ev.OutboundGenerated, "inbound_generated": ev.InboundGenerated, "served_passengers": ev.ServedPassengers, "avg_wait_min": ev.AvgWaitMin})
			case sim.MoveEvent:
				flush("move", map[string]any{"bus_id": ev.BusID, "direction": ev.Direction, "lat": ev.Lat, "lng": ev.Lng, "t": ev.T, "from": ev.From, "to": ev.To, "phase": ev.Phase})
			case sim.LayoverEvent:
				flush("layover", map[string]any{"bus_id": ev.BusID, "terminal_stop_id": ev.TerminalStopID})
			case sim.RepositionStartEvent:
				flush("reposition_start", map[string]any{"buses": ev.Buses, "layover_indices": ev.LayoverIndices})
			case sim.RepositionBusEvent:
				flush("reposition_bus", map[string]any{"bus_id": ev.BusID, "from_index": ev.FromIndex, "target_index": ev.TargetIndex, "current_stop_id": ev.CurrentStopID, "ahead_only": ev.AheadOnly})
			case sim.RepositionCompleteEvent:
				flush("reposition_complete", map[string]any{"elapsed_ms": ev.ElapsedMs})
			case sim.DoneEvent:
				// Remember final metrics and forward done downstream
				finalDone = &ev
				flush("done", map[string]any{"generated_passengers": ev.Generated, "served_passengers": ev.ServedPassengers, "avg_wait_min": ev.AvgWaitMin, "bus_distance": ev.BusDistance})
			}
		}
		// After stream closes, write reports if requested
		if finalDone != nil {
			sum := sim.ReportSummary{Generated: finalDone.Generated, Served: finalDone.ServedPassengers, AvgWaitMin: finalDone.AvgWaitMin, BusDistance: finalDone.BusDistance}
			if s.Opt.ReportPath != "" {
				if _, err := sim.WriteCSVReport(s.Opt.ReportPath, connBuses, sum); err != nil {
					log.Printf("report: create failed: %v", err)
				}
			}
			sim.PrintConsoleReport(connBuses, sum)
		}
		return
	}

	// If legacy explicitly requested, fall back to old inline simulation (currently disabled)
	http.Error(w, "legacy engine disabled; remove engine=legacy to use runner", http.StatusGone)
}
