# Kimara–Kivukoni BRT Simulation (brt08)

Interactive bus rapid transit (BRT) corridor simulation with multi‑bus fleet, stochastic passenger demand and real‑time, speed‑scalable visualization.

Components:
- Go backend: passenger generation, multi‑bus movement, dwell/travel timing, live Server‑Sent Events (SSE), runtime control API, CSV reporting.
- Vite + TypeScript + Leaflet frontend: map, buses, stop queues, metrics overlay, speed & arrival rate UI (if integrated) or via API.

## Key Features

Core operations
- Multiple buses (fleet defined in `data/fleet.json`) auto‑scheduled with headway spacing per direction.
- Directional ping‑pong trips with turn‑back dwell at terminals.
- Boarding & alighting stages separated (explicit short pause after alight for clarity) with dwell time function capped.
- Speed‑scalable simulation time: all sleeps (dwell, travel slices, activation, alight/board pause, passenger generation) scale with live `time_scale`.

Demand generation
- Stochastic stepwise Poisson arrivals; small initial seed (5%) then continuous generation.
- Time period multiplier (`period`) and directional bias (`dir_bias`), plus spatial gradient (`spatial_gradient`) & baseline fraction (`baseline_demand`).
- Runtime adjustable arrival multiplier (`arrival_factor`) for accelerating/attenuating demand without restarting.

Layover & termination
- Hard passenger cap (`-passenger_cap`) triggers graceful wind‑down once all generated passengers are served and system cleared.
- Post‑service reposition phase: each bus moves (concurrently) to nearest allowed layover stop ahead in its direction; if none ahead, nearest overall (endpoints always valid).
- Stops can declare `"allow_layover": true` in route JSON.

Metrics & reporting
- Cumulative served passenger count & running average wait (minutes) sent in events.
- Per‑bus cumulative distance & cost (capacity & cost/km from fleet file) in final console + optional timestamped CSV report (`-report`).

Runtime control
- `/api/control` POST endpoint adjusts `speed` (time scale) and `arrival_factor` per active SSE connection atomically (no reconnect needed).

SSE event stream (extended)
- Standard: `init`, `arrive`, `alight`, `board`, `dwell`, `move`, `stop_update`, `done`.
- Added: `reposition_start`, `reposition_bus` (debug per bus), `layover`, `reposition_complete` for post‑service staging.

Frontend visualization
- Live bus markers with occupancy.
- Per‑stop outbound/inbound queue counts and generated totals in legend.
- (Optional future) UI controls to send `/api/control` updates.

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

Run (port 8080 default):

```
cd backend
go run . -period 2 -passenger_cap 120 -dir_bias 1.6 -spatial_gradient 0.8 -baseline_demand 0.3 -time_scale 1.0 -arrival_factor 1.0 -report ./reports
```

Flags:
- `-period int` (1..6) Morning=2, Evening=5 for demand multiplier.
- `-passenger_cap int` Total passengers to generate (0 = unlimited continuous mode).
- `-morning_toward_kivukoni bool` Peak direction orientation.
- `-dir_bias float` Directional demand bias (>1).
- `-spatial_gradient float` (0–1) Strength of taper along corridor.
- `-baseline_demand float` (0–1) Baseline share combined with gradient.
- `-time_scale float` (>0) Real‑time acceleration (affects all waits). Range effectively clamped internally.
- `-arrival_factor float` (>0) Initial global multiplier on passenger arrival rate (runtime adjustable).
- `-report path|dir` If set, writes timestamped CSV.

Passenger generation notes:
- Initial 5% seed ensures early boarding action then per‑second Poisson batches.
- All timing respects live `speed` (time scale) via chunked sleeps.

Notes:
- Passenger generation is gradual: a small initial seed (~5%) is added, then passengers arrive at random intervals (200–800ms) until the `-passenger_cap` target is reached (or forever if 0).
- Each SSE connection creates independent per-connection bus state and generator.

### Endpoints

- `GET /api/route` Route definition (stops + pins; includes `allow_layover`).
- `GET /api/stream` Start an SSE simulation connection (query `lambda` optional base rate).
- `POST /api/control` Adjust `speed` & `arrival_factor` for a specific connection id.

Control request body:
```json
{ "conn_id": "<value from init event>", "speed": 2.5, "arrival_factor": 4 }
```

### SSE Event Reference

Common counters: `generated_passengers`, `outbound_generated`, `inbound_generated`, `served_passengers`, `avg_wait_min` (when present).

Lifecycle / operations:
- `init` Simulation start; includes `conn_id`, initial generated counts.
- `bus_add` (initial placement) bus metadata.
- `arrive` Bus reached a stop (pre‑alight).
- `alight` Passengers alighted at stop; updates served counts.
- `board` Passengers boarded; includes per‑event average wait contribution.
- `dwell` Dwell duration (ms) chosen for that stop.
- `move` Segment interpolation (during service or with `phase":"reposition"`).
- `stop_update` Queue length snapshot (deduplicated per changed stop).
- `reposition_start` Start of layover reposition phase (after service complete conditions).
- `reposition_bus` Debug: per bus chosen target layover index; `ahead_only` signals forward layover found.
- `layover` Bus reached its layover stop.
- `reposition_complete` All reposition moves finished.
- `done` Final summary (emitted after reposition phase).

## Frontend (Vite + TypeScript + Leaflet)

Development server:

```
cd frontend
npm install
npm run dev
```

Open the serving URL from Vite (typically http://localhost:5173). The frontend tries `GET /api/route` and `GET /api/stream` against the backend; if the route API isn’t reachable it falls back to static JSON under `public/data`.

What you’ll see:
- Route polyline + pins
- All active buses with direction & onboard count
- Per‑stop `outbound/inbound` queue counts and tooltips
- Legend with generated/served counts & (optionally) average wait
- Smooth bus motion (segment interpolation steps) scaled by current speed

## Data model

Stops:
- `stop_id`, `stop_name`
- `latitute`, `longtude`
- `distance_next_stop`
- `allow_layover` (bool) -> bus reposition target eligibility

Pins (for geometry smoothing):
- `left_stop_id`, `right_stop_id`, `latitute`, `longtude`

Direction semantics:
- `outbound`: from Kimara toward Kivukoni
- `inbound`: from Kivukoni toward Kimara

## Simulation logic (high level)

- Per‑second Poisson batch arrivals (mean = base λ * period multiplier * arrival_factor * stepMinutes) with spatial weighting.
- Directional bias chooses outbound vs inbound with probability derived from `dir_bias`.
- Gradient weight adjusts origin selection along corridor (favored origin tapering to destination).
- Boarding only from queue matching bus direction and route/destination validity.
- Dwell time = base + per‑passenger increments, capped; separate alight and board phases for UI fidelity.
- Travel broken into short interpolation steps (`move` events) for smooth animation.
- Termination detection when passenger cap served & system empty; then direction‑aware layover reposition and final report.
- All SSE writes serialized (mutex) to satisfy http.ResponseWriter concurrency safety.

## Runtime control examples

Increase speed to 3× and quadruple arrivals for a running connection:
```
curl -X POST http://localhost:8080/api/control -H 'Content-Type: application/json' \
	-d '{"conn_id":"<conn>","speed":3,"arrival_factor":4}'
```

## Troubleshooting

- Legend not visible: the legend is an absolutely positioned bottom‑left div injected by the frontend; ensure the frontend is served and the map container is visible.
- CORS issues: the frontend expects the backend on `http://localhost:8080`. If serving from another origin, configure CORS or proxy.
- Empty map/tiles: network issues to tile servers (OSM/Carto/OpenTopo); try switching the base layer from the control at top‑right.

## Roadmap / Ideas

- Frontend panel for live speed & arrival adjustments (invokes `/api/control`).
- Headway adherence or holding strategy (dynamic schedule recovery).
- Onboard load heatmap & per-stop performance charts.
- Refined cost model (energy, emissions, peak/off‑peak pricing).
- Export GTFS‑like snapshots or replay logs.

---

This project is a sandbox for exploring BRT operations, demand patterns, control strategies and visualization. Contributions & ideas welcome.

