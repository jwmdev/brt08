import L from 'leaflet';
async function loadRoute() {
    const res = await fetch('/public/data/kimara_kivukoni_stops.json').catch(() => fetch('/data/kimara_kivukoni_stops.json'));
    if (!res.ok)
        throw new Error('Failed to load stops JSON');
    return res.json();
}
function createBusIcon() {
    return L.icon({
        iconUrl: '/img/bus.svg',
        iconSize: [32, 32],
        iconAnchor: [16, 16],
        className: 'bus-svg-icon'
    });
}
function createStopMarker(stop) {
    return L.circleMarker([stop.latitute, stop.longtude], {
        radius: 4,
        weight: 1,
        color: '#222',
        fillColor: '#ffb703',
        fillOpacity: 0.9
    }).bindTooltip(`<strong>${stop.stop_name}</strong><br/>ID: ${stop.stop_id}`, { direction: 'top' });
}
function interpolate(a, b, t) { return a + (b - a) * t; }
function buildSegments(stops) {
    let cumulative = 0;
    const segs = [];
    for (let i = 0; i < stops.length - 1; i++) {
        const lengthKm = stops[i].distance_next_stop;
        segs.push({ from: stops[i], to: stops[i + 1], lengthKm, cumulativeStart: cumulative });
        cumulative += lengthKm;
    }
    return segs;
}
async function init() {
    // try backend API first, fallback to static
    let data; // we'll normalize to RouteData shape
    try {
        const r = await fetch('/api/route');
        if (!r.ok)
            throw new Error('backend route fetch failed');
        data = await r.json();
    }
    catch {
        data = await loadRoute();
    }
    // Normalize stop field names (backend uses id/name, frontend expects stop_id/stop_name)
    const stops = (data.stops || []).map((s) => ({
        stop_id: s.stop_id ?? s.id,
        stop_name: s.stop_name ?? s.name,
        latitute: s.latitute,
        longtude: s.longtude,
        distance_next_stop: s.distance_next_stop ?? s.distance_to_next ?? s.DistanceToNext ?? 0,
        is_pin: s.is_pin === true
    }));
    // Provide route level fallbacks
    data.route = data.route || data.name || 'Route';
    data.direction = data.direction || 'outbound';
    const map = L.map('map');
    const osm = L.tileLayer('https://{s}.tile.openstreetmap.org/{z}/{x}/{y}.png', {
        maxZoom: 19,
        attribution: '&copy; OpenStreetMap contributors'
    });
    osm.addTo(map);
    // Fit map to stops bounds
    const visibleStopMarkers = stops.filter(s => !s.is_pin).map(createStopMarker);
    const group = L.featureGroup(visibleStopMarkers);
    group.addTo(map);
    map.fitBounds(group.getBounds().pad(0.15));
    // Catmull-Rom spline (centripetal variant simplified) that passes through original stop points.
    function catmullRom(points, segmentsPerEdge = 16) {
        if (points.length < 3)
            return points;
        const pts = [];
        const clamp = (i) => {
            if (i < 0)
                return points[0];
            if (i >= points.length)
                return points[points.length - 1];
            return points[i];
        };
        for (let i = 0; i < points.length - 1; i++) {
            const P0 = clamp(i - 1);
            const P1 = clamp(i);
            const P2 = clamp(i + 1);
            const P3 = clamp(i + 2);
            // Include the actual stop point (except after first iteration will be added by previous segment end)
            if (i === 0)
                pts.push([P1[0], P1[1]]);
            for (let s = 1; s <= segmentsPerEdge; s++) {
                const t = s / segmentsPerEdge; // 0..1
                const t2 = t * t;
                const t3 = t2 * t;
                const x = 0.5 * ((2 * P1[0]) + (-P0[0] + P2[0]) * t + (2 * P0[0] - 5 * P1[0] + 4 * P2[0] - P3[0]) * t2 + (-P0[0] + 3 * P1[0] - 3 * P2[0] + P3[0]) * t3);
                const y = 0.5 * ((2 * P1[1]) + (-P0[1] + P2[1]) * t + (2 * P0[1] - 5 * P1[1] + 4 * P2[1] - P3[1]) * t2 + (-P0[1] + 3 * P1[1] - 3 * P2[1] + P3[1]) * t3);
                if (Number.isFinite(x) && Number.isFinite(y)) {
                    pts.push([x, y]);
                }
                else {
                    // fallback to raw point if numeric issue
                    pts.push([P2[0], P2[1]]);
                }
            }
        }
        // Guarantee last original point present
        const last = points[points.length - 1];
        const tail = pts[pts.length - 1];
        if (!tail || tail[0] !== last[0] || tail[1] !== last[1])
            pts.push(last);
        return pts;
    }
    const rawPoints = stops.map(s => [s.latitute, s.longtude]); // includes pins for geometry
    let polyPoints = rawPoints;
    try {
        // filter consecutive duplicates
        const dedup = [];
        for (const p of rawPoints) {
            if (!dedup.length || dedup[dedup.length - 1][0] !== p[0] || dedup[dedup.length - 1][1] !== p[1])
                dedup.push(p);
        }
        polyPoints = catmullRom(dedup, 14);
        // Ensure all original stops are in the polyline sequence (for hit-testing / pass-through guarantee)
        for (const orig of dedup) {
            let contained = false;
            for (const pp of polyPoints) {
                if (Math.abs(pp[0] - orig[0]) < 1e-9 && Math.abs(pp[1] - orig[1]) < 1e-9) {
                    contained = true;
                    break;
                }
            }
            if (!contained)
                polyPoints.push(orig);
        }
    }
    catch (e) {
        console.warn('smoothing failed, using raw path', e);
        polyPoints = rawPoints;
    }
    L.polyline(polyPoints, { color: '#1976d2', weight: 8, opacity: 0.9, lineJoin: 'round', lineCap: 'round' }).addTo(map);
    // Bus marker for live updates
    const firstNonPin = stops.find(s => !s.is_pin) || stops[0];
    const bus = L.marker([firstNonPin.latitute, firstNonPin.longtude], { icon: createBusIcon() }).addTo(map);
    // Bus occupancy label (div icon hovering above the bus)
    let busCapacity = 0;
    let busOnboard = 0;
    const makeBusLabelHTML = () => {
        const cap = busCapacity || '?';
        const occ = busOnboard;
        const ratio = busCapacity > 0 ? occ / busCapacity : 0;
        let color = '#2e7d32';
        if (ratio >= 0.9)
            color = '#c62828';
        else if (ratio >= 0.7)
            color = '#ef6c00';
        else if (ratio >= 0.5)
            color = '#f9a825';
        return `<div style=\"transform:translate(-40%, 80%);background:#fff;border:1px solid #111;padding:3px 8px;border-radius:6px;font-size:12px;line-height:1.05;font-family:monospace;font-weight:600;min-width:30px;text-align:center;box-shadow:0 1px 5px rgba(0,0,0,.45);\"><span style='color:${color};'>${occ}</span>/<span>${cap}</span></div>`;
    };
    const busLabel = L.marker(bus.getLatLng(), { interactive: false, icon: L.divIcon({ className: 'bus-occupancy-label', html: makeBusLabelHTML() }) }).addTo(map);
    function refreshBusLabel() {
        busLabel.setLatLng(bus.getLatLng());
        busLabel.setIcon(L.divIcon({ className: 'bus-occupancy-label', html: makeBusLabelHTML() }));
    }
    // Add passenger count labels
    const stopLabels = {};
    stops.forEach(s => {
        if (s.is_pin)
            return; // skip labeling geometry-only pins
        const id = s.stop_id;
        const label = L.marker([s.latitute, s.longtude], {
            icon: L.divIcon({ className: 'stop-count-label', html: `<div data-stop="${id}" style="transform: translate(-50%, -135%); background:#fff; padding:4px 6px; border:1px solid #222; border-radius:6px; font-size:11px; font-family:monospace; min-width:70px; text-align:center; box-shadow:0 1px 4px rgba(0,0,0,.4);"><div style='font-weight:600; white-space:nowrap;'>${s.stop_name}</div><div class='count' data-stop-count='${id}' style='margin-top:2px; font-size:12px;'>0</div></div>` })
        });
        label.addTo(map);
        stopLabels[id] = label;
    });
    function updateStopCount(id, value) {
        const el = document.querySelector(`div[data-stop='${id}'] div[data-stop-count='${id}']`);
        if (el)
            el.textContent = String(value);
    }
    // transient labels removed per new behavior request
    function updateLegendText(text) {
        const el = document.getElementById('legend');
        if (el)
            el.innerHTML = text;
    }
    updateLegendText(`Route: ${data.route}<br/>Waiting for simulation...`);
    // SSE connection
    function openStream() {
        let es;
        try {
            es = new EventSource('/api/stream');
        }
        catch {
            es = new EventSource('http://localhost:8080/api/stream');
        }
        return es;
    }
    let es = openStream();
    let reconnectAttempts = 0;
    function scheduleReconnect() {
        if (reconnectAttempts > 5)
            return;
        reconnectAttempts++;
        const backoff = 1000 * reconnectAttempts;
        updateLegendText(`Reconnecting... attempt ${reconnectAttempts}`);
        setTimeout(() => {
            es.close();
            es = openStream();
            attachHandlers();
        }, backoff);
    }
    function attachHandlers() {
        es.addEventListener('error', () => scheduleReconnect());
        es.addEventListener('init', ev => {
            reconnectAttempts = 0;
            updateLegendText(`Route: ${data.route}<br/>Simulation started`);
            try {
                const d = JSON.parse(ev.data);
                if (d.bus) {
                    // Go JSON uses lower-case tags we normalized above; handle both shapes
                    const b = d.bus;
                    busCapacity = b?.type?.capacity ?? b?.Type?.capacity ?? b?.Type?.Capacity ?? busCapacity;
                    busOnboard = b?.passengers_onboard ?? b?.PassengersOnboard ?? 0;
                    refreshBusLabel();
                }
            }
            catch { }
        });
        es.addEventListener('arrive', ev => {
            try {
                const d = JSON.parse(ev.data);
                const st = stops.find(s => s.stop_id === d.stop_id && !s.is_pin);
                if (st) {
                    // subtle pulse effect on arrival
                    const el = document.querySelector(`div[data-stop='${st.stop_id}']`);
                    if (el) {
                        el.classList.add('pulse');
                        setTimeout(() => el.classList.remove('pulse'), 600);
                    }
                }
            }
            catch { }
        });
        es.addEventListener('move', ev => {
            try {
                const d = JSON.parse(ev.data);
                bus.setLatLng([d.lat, d.lng]);
                refreshBusLabel();
            }
            catch { }
        });
        es.addEventListener('stop_update', ev => {
            try {
                const d = JSON.parse(ev.data);
                const stp = stops.find(s => s.stop_id === d.stop_id && !s.is_pin);
                if (stp)
                    updateStopCount(d.stop_id, d.outbound_queue);
            }
            catch { }
        });
        es.addEventListener('alight', ev => {
            try {
                const d = JSON.parse(ev.data);
                if (typeof d.alighted === 'number') {
                    busOnboard = d.bus_onboard ?? (busOnboard - d.alighted);
                    if (busOnboard < 0)
                        busOnboard = 0;
                    refreshBusLabel();
                }
            }
            catch { }
        });
        es.addEventListener('board', ev => {
            try {
                const d = JSON.parse(ev.data);
                if (typeof d.boarded === 'number') {
                    busOnboard = d.bus_onboard ?? (busOnboard + d.boarded);
                    if (busCapacity && busOnboard > busCapacity)
                        busOnboard = busCapacity;
                    if (typeof d.stop_queue === 'number')
                        updateStopCount(d.stop_id, d.stop_queue);
                    refreshBusLabel();
                }
            }
            catch { }
        });
        es.addEventListener('done', () => {
            updateLegendText(`Route: ${data.route}<br/>Trip complete`);
            es.close();
        });
    }
    attachHandlers();
}
init().catch(err => console.error(err));
