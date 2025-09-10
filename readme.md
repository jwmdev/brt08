# Kimara–Kivukoni BRT Simulation (brt08)

Interactive bus rapid transit (BRT) simulation for the Kimara–Kivukoni corridor.

It consists of:
- A Go backend that simulates two buses (outbound and inbound), generates passengers with time/direction/spatial biases, and streams live state via Server‑Sent Events (SSE).
- A Vite + TypeScript + Leaflet frontend that visualizes the route, buses, stop queues, and live counters.

## Features

- Dual-direction buses (outbound/inbound) running concurrently
- Boarding/alighting with dwell time calculated per stop
- Passenger arrivals generated stochastically over time (not in a single burst)
- Time period multipliers (e.g., morning/evening peaks)
- Directional bias (favor demand in the peak direction)
- Spatial gradient (more passengers near the favored origin, tapering along the route)
- Global passenger target: specify the exact number of passengers to be served in a simulation
- Live SSE updates: init, arrive, board, alight, dwell, move, stop_update, done
- Frontend map with stops and dynamic per-stop queues (outbound/inbound) and bus occupancy labels
- Bottom‑left legend with total/outbound/inbound generated passenger counters

## Repository layout

```
brt08/
	backend/           # Go HTTP server + simulator
		data/            # Route JSON (kimara_kivukoni_stops.json)
	frontend/          # Vite + TypeScript + Leaflet UI
		public/          # Static assets (images, data fallback)
	readme.md          # This file
```

## Backend (Go)

Run the backend (port 8080):

```
cd backend
go run . -period 2 -passenger_cap 120 -dir_bias 1.6 -spatial_gradient 0.8 -baseline_demand 0.3
```

Key flags:
- `-period` (int): time period affecting demand (1..6). 2=morning peak, 5=evening peak.
- `-passenger_cap` (int): TOTAL passengers to generate/serve in this simulation. 0 = unlimited.
- `-morning_toward_kivukoni` (bool): if true, morning peak favors outbound toward Kivukoni; evening inverts.
- `-dir_bias` (float): directional bias factor (>1 favors the peak direction). Typical: 1.2–2.0.
- `-spatial_gradient` (float 0..1): concentrates demand near the favored origin along the corridor.
- `-baseline_demand` (float 0..1): baseline fraction used with the spatial gradient.

Notes:
- Passenger generation is gradual: a small initial seed (~5%) is added, then passengers arrive at random intervals (200–800ms) until the `-passenger_cap` target is reached (or forever if 0).
- Each SSE connection creates independent per-connection bus state and generator.

### Endpoints

- `GET /api/route` → Returns the route with stops/pins for map rendering.
- `GET /api/stream` → SSE stream of events for one simulation run. Optional query: `?lambda=1.2` to tweak base arrival rate.

### Event types (SSE)

All events are JSON objects. Common fields include counters:
- `generated_passengers`, `outbound_generated`, `inbound_generated`

Event kinds:
- `init`: initial time, bus list, counters
- `move`: bus_id, direction, lat, lng, from, to, t
- `arrive`: bus_id, stop_id, direction, stop queues, onboard
- `board`: bus_id, stop_id, boarded, updated onboard, stop queues
- `alight`: bus_id, stop_id, alighted, updated onboard
- `dwell`: bus_id, stop_id, dwell_ms, boarded/alighted counts
- `stop_update`: stop_id, outbound_queue, inbound_queue, counters
- `done`: simulation complete + final counters

## Frontend (Vite + TypeScript + Leaflet)

Development server:

```
cd frontend
npm install
npm run dev
```

Open the serving URL from Vite (typically http://localhost:5173). The frontend tries `GET /api/route` and `GET /api/stream` against the backend; if the route API isn’t reachable it falls back to static JSON under `public/data`.

What you’ll see:
- Map with the Kimara–Kivukoni route polyline
- Stop markers with tooltips
- Two bus markers (outbound and inbound) with occupancy labels
- Per-stop queue labels showing `outbound/inbound`
- Legend (bottom‑left) with total and per-direction passenger counts

## Data model

Stops (frontend-normalized fields):
- `stop_id`, `stop_name`
- `latitute`, `longtude` (as provided)
- `distance_next_stop`

Pins (for geometry smoothing):
- `left_stop_id`, `right_stop_id`, `latitute`, `longtude`

Direction semantics:
- `outbound`: from Kimara toward Kivukoni
- `inbound`: from Kivukoni toward Kimara

## Simulation logic (high level)

- Poisson-like arrivals parameterized by `lambda`, time period multiplier, direction bias, and spatial gradient
- Separate queues per stop per direction; last stop forces full alighting
- Dwell time depends on number of boarding/alighting passengers
- SSE serializes writes (mutex) to avoid concurrent write issues

## Troubleshooting

- Legend not visible: the legend is an absolutely positioned bottom‑left div injected by the frontend; ensure the frontend is served and the map container is visible.
- CORS issues: the frontend expects the backend on `http://localhost:8080`. If serving from another origin, configure CORS or proxy.
- Empty map/tiles: network issues to tile servers (OSM/Carto/OpenTopo); try switching the base layer from the control at top‑right.

## Roadmap / Ideas

- Apply spatial gradient also to any remaining dynamic generation paths (already applied to generator; can be extended as needed)
- Expose average wait time and per-stop boarded/alighted totals in the UI
- Add configurable bus schedules/fleet sizes

---

This project is a sandbox for exploring BRT operations, demand patterns, and visualization. Contributions and ideas are welcome.

