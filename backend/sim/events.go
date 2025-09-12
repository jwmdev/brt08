package sim

import "time"

// Event is a marker for all simulation events emitted by Runner.
type Event interface{ isEvent() }

// InitEvent signals the start of a simulation stream.
type InitEvent struct {
	Time          time.Time
	ConnID        string
	Generated     int
	OutboundGen   int
	InboundGen    int
	AvgWaitMin    float64
	ArrivalFactor float64
}

func (InitEvent) isEvent() {}

// StopUpdateEvent updates stop queue sizes and counters.
type StopUpdateEvent struct {
	StopID            int
	OutboundQueue     int
	InboundQueue      int
	Generated         int
	OutboundGenerated int
	InboundGenerated  int
}

func (StopUpdateEvent) isEvent() {}

// BusAddEvent indicates a bus added to the route at the start.
type BusAddEvent struct {
	BusID        int
	Direction    string
	AvgSpeedKmph float64
	Capacity     int
}

func (BusAddEvent) isEvent() {}

// ArriveEvent indicates a bus arrival at a stop.
type ArriveEvent struct {
	BusID             int
	Direction         string
	StopID            int
	Time              time.Time
	BusOnboard        int
	PassengersOnboard int
	Generated         int
	OutboundGenerated int
	InboundGenerated  int
}

func (ArriveEvent) isEvent() {}

// AlightEvent indicates alighting.
type AlightEvent struct {
	BusID             int
	Direction         string
	StopID            int
	Alighted          int
	BusOnboard        int
	PassengersOnboard int
	Generated         int
	OutboundGenerated int
	InboundGenerated  int
	Final             bool
	ServedPassengers  int64
}

func (AlightEvent) isEvent() {}

// BoardEvent indicates boarding.
type BoardEvent struct {
	BusID             int
	Direction         string
	StopID            int
	Boarded           int
	BusOnboard        int
	PassengersOnboard int
	StopOutbound      int
	StopInbound       int
	Generated         int
	OutboundGenerated int
	InboundGenerated  int
	ServedPassengers  int64
	AvgWaitMin        float64
}

func (BoardEvent) isEvent() {}

// MoveEvent indicates an in-transit update between two stops (optionally for reposition phase).
type MoveEvent struct {
	BusID     int
	Direction string
	Lat       float64
	Lng       float64
	T         float64
	From      int
	To        int
	Phase     string // "reposition" when repositioning
}

func (MoveEvent) isEvent() {}

// LayoverEvent indicates a bus is now laying over at a terminal.
type LayoverEvent struct {
	BusID          int
	TerminalStopID int
}

func (LayoverEvent) isEvent() {}

// RepositionStartEvent marks start of reposition phase.
type RepositionStartEvent struct {
	Buses          int
	LayoverIndices []int
}

func (RepositionStartEvent) isEvent() {}

// RepositionBusEvent indicates a bus chosen for reposition.
type RepositionBusEvent struct {
	BusID         int
	FromIndex     int
	TargetIndex   int
	CurrentStopID int
	AheadOnly     bool
}

func (RepositionBusEvent) isEvent() {}

// RepositionCompleteEvent marks end of reposition phase with elapsed ms.
type RepositionCompleteEvent struct {
	ElapsedMs int64
}

func (RepositionCompleteEvent) isEvent() {}

// DoneEvent signals completion and carries summary metrics and per-bus distances.
type DoneEvent struct {
	Completed         bool
	Generated         int
	OutboundGenerated int
	InboundGenerated  int
	ServedPassengers  int64
	AvgWaitMin        float64
	BusDistance       map[int]float64
}

func (DoneEvent) isEvent() {}
