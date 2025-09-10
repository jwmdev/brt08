import L from 'leaflet';
import type { RouteData, Stop } from './types';

async function loadRoute(): Promise<RouteData> {
  const res = await fetch('/public/data/kimara_kivukoni_stops.json').catch(() => fetch('/data/kimara_kivukoni_stops.json'));
  if (!res.ok) throw new Error('Failed to load stops JSON');
  return res.json();
}

function createBusIcon() {
  return L.icon({
    iconUrl: '/img/bus.svg',
    iconSize: [32,32],
    iconAnchor: [16,16],
    className: 'bus-svg-icon'
  });
}

function createStopMarker(stop: Stop) {
  return L.circleMarker([stop.latitute, stop.longtude], {
    radius: 4,
    weight: 1,
    color: '#222',
    fillColor: '#ffb703',
    fillOpacity: 0.9
  }).bindTooltip(`<strong>${stop.stop_name}</strong><br/>ID: ${stop.stop_id}`, {direction: 'top'});
}

function interpolate(a: number, b: number, t: number) { return a + (b - a) * t; }

interface Segment { from: Stop; to: Stop; lengthKm: number; cumulativeStart: number; }

function buildSegments(stops: Stop[]): Segment[] {
  let cumulative = 0;
  const segs: Segment[] = [];
  for (let i=0; i<stops.length-1; i++) {
    const lengthKm = stops[i].distance_next_stop;
    segs.push({ from: stops[i], to: stops[i+1], lengthKm, cumulativeStart: cumulative });
    cumulative += lengthKm;
  }
  return segs;
}

async function init() {
  // try backend API first, fallback to static
  let data: any; // we'll normalize to RouteData shape
  try {
    const r = await fetch('/api/route');
    if (!r.ok) throw new Error('backend route fetch failed');
    data = await r.json();
  } catch {
    data = await loadRoute();
  }

  // Normalize stop field names (backend uses id/name, frontend expects stop_id/stop_name)
  const stops: Stop[] = (data.stops || []).map((s: any) => ({
    stop_id: s.stop_id ?? s.id,
    stop_name: s.stop_name ?? s.name,
    latitute: s.latitute,
    longtude: s.longtude,
    distance_next_stop: s.distance_next_stop ?? s.distance_to_next ?? s.DistanceToNext ?? 0,
  }));
  const pins = (data.pins || []).map((p: any) => ({
    left_stop_id: p.left_stop_id,
    right_stop_id: p.right_stop_id,
    latitute: p.latitute,
    longtude: p.longtude,
  }));

  // Provide route level fallbacks
  data.route = data.route || data.name || 'Route';
  data.direction = data.direction || 'outbound';

  const map = L.map('map');
  // Define multiple detailed base layers
  const osmStandard = L.tileLayer('https://{s}.tile.openstreetmap.org/{z}/{x}/{y}.png', {
    maxZoom: 20,
    attribution: '&copy; OpenStreetMap contributors'
  });
  const osmHot = L.tileLayer('https://{s}.tile.openstreetmap.fr/hot/{z}/{x}/{y}.png', {
    maxZoom: 20,
    attribution: '© OpenStreetMap contributors, Tiles style by Humanitarian OpenStreetMap Team hosted by OSM France'
  });
  const cartoVoyager = L.tileLayer('https://{s}.basemaps.cartocdn.com/rastertiles/voyager/{z}/{x}/{y}{r}.png', {
    maxZoom: 20,
    attribution: '© OpenStreetMap contributors © CARTO'
  });
  const openTopo = L.tileLayer('https://{s}.tile.opentopomap.org/{z}/{x}/{y}.png', {
    maxZoom: 17,
    attribution: '© OpenStreetMap contributors, SRTM | Style: © OpenTopoMap (CC-BY-SA)'
  });
  // Choose a more detailed / legible default (Carto Voyager)
  cartoVoyager.addTo(map);
  const baseLayers: Record<string, L.TileLayer> = {
    'Carto Voyager': cartoVoyager,
    'OSM Standard': osmStandard,
    'OSM HOT': osmHot,
    'OpenTopoMap': openTopo,
  };

  // Fit map to stops bounds
  const visibleStopMarkers = stops.map(createStopMarker);
  const group = L.featureGroup(visibleStopMarkers);
  group.addTo(map);
  map.fitBounds(group.getBounds().pad(0.15));

  // Original centripetal Catmull-Rom spline that passes through every original point.
  function catmullRom(points: [number, number][], segmentsPerEdge = 16): [number, number][] {
    if (points.length < 3) return points;
    const pts: [number, number][] = [];
    const clamp = (i: number) => {
      if (i < 0) return points[0];
      if (i >= points.length) return points[points.length - 1];
      return points[i];
    };
    for (let i = 0; i < points.length - 1; i++) {
      const P0 = clamp(i - 1);
      const P1 = clamp(i);
      const P2 = clamp(i + 1);
      const P3 = clamp(i + 2);
      if (i === 0) pts.push([P1[0], P1[1]]);
      for (let s = 1; s <= segmentsPerEdge; s++) {
        const t = s / segmentsPerEdge;
        const t2 = t * t;
        const t3 = t2 * t;
        const x = 0.5 * ((2 * P1[0]) + (-P0[0] + P2[0]) * t + (2*P0[0] - 5*P1[0] + 4*P2[0] - P3[0]) * t2 + (-P0[0] + 3*P1[0] - 3*P2[0] + P3[0]) * t3);
        const y = 0.5 * ((2 * P1[1]) + (-P0[1] + P2[1]) * t + (2*P0[1] - 5*P1[1] + 4*P2[1] - P3[1]) * t2 + (-P0[1] + 3*P1[1] - 3*P2[1] + P3[1]) * t3);
        if (Number.isFinite(x) && Number.isFinite(y)) {
          pts.push([x, y]);
        } else {
          pts.push([P2[0], P2[1]]);
        }
      }
    }
    const last = points[points.length - 1];
    const tail = pts[pts.length - 1];
    if (!tail || tail[0] !== last[0] || tail[1] !== last[1]) pts.push(last);
    return pts;
  }
  // Build geometry list inserting pins between their referenced stops
  const stopIndex: Record<number, number> = {};
  stops.forEach((s, i) => { stopIndex[s.stop_id] = i; });
  const rawPoints: [number, number][] = [];
  for (let i=0; i<stops.length; i++) {
    const s = stops[i];
    rawPoints.push([s.latitute, s.longtude]);
    // find pins whose left_stop_id is this stop and right matches next stop
    const next = stops[i+1];
    if (next) {
  (pins as {left_stop_id:number; right_stop_id:number; latitute:number; longtude:number;}[]).forEach(p => {
        if (p.left_stop_id === s.stop_id && p.right_stop_id === next.stop_id) {
          rawPoints.push([p.latitute, p.longtude]);
        }
      });
    }
  }
  let polyPoints: [number, number][] = rawPoints;
  try {
    // filter consecutive duplicates
    const dedup: [number, number][] = [];
    for (const p of rawPoints) {
      if (!dedup.length || dedup[dedup.length - 1][0] !== p[0] || dedup[dedup.length - 1][1] !== p[1]) dedup.push(p);
    }
  polyPoints = catmullRom(dedup, 16); // revert: stronger sampling with original spline
    // Ensure all original stops are in the polyline sequence (for hit-testing / pass-through guarantee)
    for (const orig of dedup) {
      let contained = false;
      for (const pp of polyPoints) { if (Math.abs(pp[0]-orig[0]) < 1e-9 && Math.abs(pp[1]-orig[1]) < 1e-9) { contained = true; break; } }
      if (!contained) polyPoints.push(orig);
    }
  } catch (e) {
    console.warn('smoothing failed, using raw path', e);
    polyPoints = rawPoints;
  }
  L.polyline(polyPoints, { color: '#1976d2', weight: 8, opacity: 0.9, lineJoin: 'round', lineCap: 'round' }).addTo(map);
  L.control.layers(baseLayers, {}, { position: 'topright', collapsed: true }).addTo(map);

  // Dual bus markers (outbound + inbound)
  interface BusState { id:number; direction:string; marker:L.Marker; label:L.Marker; capacity:number; onboard:number; }
  const buses: Record<number, BusState> = {};
  function makeBusLabelHTML(cap:number, onboard:number) {
    const ratio = cap > 0 ? onboard / cap : 0;
    let color = '#2e7d32';
    if (ratio >= 0.9) color = '#c62828';
    else if (ratio >= 0.7) color = '#ef6c00';
    else if (ratio >= 0.5) color = '#f9a825';
    const capTxt = cap || '?';
    return `<div style=\"transform:translate(-40%, 80%);background:#fff;border:1px solid #111;padding:3px 8px;border-radius:6px;font-size:12px;line-height:1.05;font-family:monospace;font-weight:600;min-width:30px;text-align:center;box-shadow:0 1px 5px rgba(0,0,0,.45);\"><span style='color:${color};'>${onboard}</span>/<span>${capTxt}</span></div>`;
  }
  function createBusState(id:number, direction:string, lat:number, lng:number, capacity:number, onboard:number): BusState {
    const m = L.marker([lat,lng], { icon: createBusIcon(), title: `Bus ${id} (${direction})` }).addTo(map);
    const lbl = L.marker([lat,lng], { interactive:false, icon: L.divIcon({ className:'bus-occupancy-label', html: makeBusLabelHTML(capacity,onboard) }) }).addTo(map);
    return { id, direction, marker:m, label:lbl, capacity, onboard };
  }
  function refreshBus(b:BusState) {
    b.label.setLatLng(b.marker.getLatLng());
    b.label.setIcon(L.divIcon({ className:'bus-occupancy-label', html: makeBusLabelHTML(b.capacity,b.onboard) }));
  }

  // transient bubble overlays removed per request

  // Add passenger count labels
  const stopLabels: Record<number, L.Marker> = {};
  stops.forEach(s => {
    const id = s.stop_id;
    const label = L.marker([s.latitute, s.longtude], {
      icon: L.divIcon({ className: 'stop-count-label', html: `<div data-stop="${id}" style="transform: translate(-50%, -135%); background:#fff; padding:4px 6px; border:1px solid #222; border-radius:6px; font-size:11px; font-family:monospace; min-width:70px; text-align:center; box-shadow:0 1px 4px rgba(0,0,0,.4);"><div style='font-weight:600; white-space:nowrap;'>${s.stop_name}</div><div class='count' data-stop-count='${id}' style='margin-top:2px; font-size:12px;'>0</div></div>` })
    });
    label.addTo(map);
    stopLabels[id] = label;
  });

  function updateStopCount(id: number, outbound: number, inbound?: number) {
    const el = document.querySelector(`div[data-stop='${id}'] div[data-stop-count='${id}']`);
    if (el) {
      if (inbound == null) el.textContent = String(outbound);
      else el.textContent = `${outbound}/${inbound}`; // show outbound/inbound
    }
  }

  // transient labels removed per new behavior request

  let totals = { total: 0, outbound: 0, inbound: 0, served: 0, avgWaitMin: 0 };
  let legendState = 'Waiting for simulation...';
  // Plain absolute legend (simpler & guaranteed visibility)
  if (!document.getElementById('legend-style')) {
    const style = document.createElement('style');
    style.id = 'legend-style';
    style.textContent = `
      #legend { position:absolute; left:12px; bottom:12px; background:#fff; padding:8px 10px; font-size:12px; line-height:1.2; box-shadow:0 2px 6px rgba(0,0,0,0.25); border-radius:4px; z-index:1000; font-family:system-ui,Arial,sans-serif; display:inline-block; width:auto; max-width:260px; top:auto!important; height:auto!important; }
      #legend strong { font-weight:600; }
    `;
    document.head.appendChild(style);
  }
  let legendEl = document.getElementById('legend');
  if (!legendEl) {
    legendEl = document.createElement('div');
    legendEl.id = 'legend';
    map.getContainer().appendChild(legendEl);
  }
  function renderLegend(state?: string) {
    if (state) legendState = state;
    const el = document.getElementById('legend');
    if (!el) return;
    el.innerHTML = `<div style='font-weight:600;margin-bottom:4px;min-width:150px;'>${data.route}</div>`+
      `<div style='margin-bottom:4px;'>${legendState}</div>`+
      `<div>Passengers generated: <strong>${totals.total}</strong></div>`+
      `<div>Passengers served: <strong>${totals.served}</strong></div>`+
      `<div>Avg wait: <strong>${totals.avgWaitMin.toFixed(2)} min</strong></div>`+
      `<div style='margin-top:2px;'>`+
      `<span style='color:#1976d2;font-weight:600;'>Outbound: ${totals.outbound}</span><br/>`+
      `<span style='color:#c62828;font-weight:600;'>Inbound: ${totals.inbound}</span>`+
      `</div>`;
  }
  renderLegend();

  // SSE connection
    function openStream(): EventSource {
      let es: EventSource;
      try {
        es = new EventSource('/api/stream');
      } catch {
        es = new EventSource('http://localhost:8080/api/stream');
      }
      return es;
    }
    let es = openStream();
    let reconnectAttempts = 0;
    function scheduleReconnect() {
      if (reconnectAttempts > 5) return;
      reconnectAttempts++;
      const backoff = 1000 * reconnectAttempts;
  renderLegend(`Reconnecting... attempt ${reconnectAttempts}`);
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
  renderLegend('Simulation started');
        try {
          const d = JSON.parse((ev as MessageEvent).data);
          if (Array.isArray(d.buses)) {
            d.buses.forEach((b: any) => {
              const id = b.id ?? b.ID;
              const dir = b.direction ?? b.Direction ?? 'outbound';
              const cap = b?.type?.capacity ?? b?.Type?.capacity ?? b?.Type?.Capacity ?? 0;
              const onboard = b.passengers_onboard ?? b.PassengersOnboard ?? 0;
              // initial position: first stop for outbound, last stop for inbound
              let lat = stops[0].latitute, lng = stops[0].longtude;
              if (dir === 'inbound') { lat = stops[stops.length-1].latitute; lng = stops[stops.length-1].longtude; }
              if (!buses[id]) buses[id] = createBusState(id, dir, lat, lng, cap, onboard);
            });
          }
          if (typeof d.generated_passengers === 'number') {
            totals.total = d.generated_passengers;
            totals.outbound = d.outbound_generated ?? totals.outbound;
            totals.inbound = d.inbound_generated ?? totals.inbound;
            if (typeof d.served_passengers === 'number') totals.served = d.served_passengers;
            if (typeof d.avg_wait_min === 'number') totals.avgWaitMin = d.avg_wait_min;
            renderLegend('Simulation started');
          }
        } catch {}
      });
      // New bus added later (staggered activation)
      es.addEventListener('bus_add', ev => {
        try {
          const d = JSON.parse((ev as MessageEvent).data);
          const id = d.bus_id;
          const dir = d.direction ?? 'outbound';
          if (typeof id === 'number' && !buses[id]) {
            let lat = stops[0].latitute, lng = stops[0].longtude;
            if (dir === 'inbound') { lat = stops[stops.length-1].latitute; lng = stops[stops.length-1].longtude; }
            const cap = typeof d.capacity === 'number' ? d.capacity : 0;
            buses[id] = createBusState(id, dir, lat, lng, cap, 0);
          }
        } catch {}
      });
      es.addEventListener('arrive', ev => {
        try {
          const d = JSON.parse((ev as MessageEvent).data);
          const st = stops.find(s => s.stop_id === d.stop_id);
          if (st) {
            // subtle pulse effect on arrival
            const el = document.querySelector(`div[data-stop='${st.stop_id}']`);
            if (el) {
              el.classList.add('pulse');
              setTimeout(()=> el.classList.remove('pulse'), 600);
            }
            if (typeof d.outbound_queue === 'number') {
              updateStopCount(st.stop_id, d.outbound_queue, d.inbound_queue);
            }
            if (typeof d.generated_passengers === 'number') {
              totals.total = d.generated_passengers;
              if (typeof d.outbound_generated === 'number') totals.outbound = d.outbound_generated;
              if (typeof d.inbound_generated === 'number') totals.inbound = d.inbound_generated;
              renderLegend('Running');
            }
            // also refresh bus tag if onboard provided
            if (typeof d.bus_id === 'number') {
              const b = buses[d.bus_id];
              if (b && typeof d.bus_onboard === 'number') { b.onboard = d.bus_onboard; refreshBus(b); }
            }
          }
        } catch {}
      });
      es.addEventListener('move', ev => {
        try {
          const d = JSON.parse((ev as MessageEvent).data);
          const b = buses[d.bus_id];
          if (b) { b.marker.setLatLng([d.lat, d.lng]); refreshBus(b); }
        } catch {}
      });
      es.addEventListener('stop_update', ev => {
        try {
          const d = JSON.parse((ev as MessageEvent).data);
          const stp = stops.find(s => s.stop_id === d.stop_id);
          if (stp) updateStopCount(d.stop_id, d.outbound_queue, d.inbound_queue);
          if (typeof d.generated_passengers === 'number') {
            totals.total = d.generated_passengers;
            if (typeof d.outbound_generated === 'number') totals.outbound = d.outbound_generated;
            if (typeof d.inbound_generated === 'number') totals.inbound = d.inbound_generated;
            renderLegend('Running');
          }
        } catch {}
      });
      es.addEventListener('alight', ev => {
        try {
          const d = JSON.parse((ev as MessageEvent).data);
          if (typeof d.alighted === 'number') {
            const b = buses[d.bus_id];
            if (b) { b.onboard = d.bus_onboard ?? (b.onboard - d.alighted); if (b.onboard < 0) b.onboard = 0; refreshBus(b); }
            if (typeof d.served_passengers === 'number') { totals.served = d.served_passengers; renderLegend('Running'); }
          }
        } catch {}
      });
      es.addEventListener('board', ev => {
        try {
          const d = JSON.parse((ev as MessageEvent).data);
          if (typeof d.boarded === 'number') {
            const b = buses[d.bus_id];
            if (b) {
              b.onboard = d.bus_onboard ?? (b.onboard + d.boarded);
              if (b.capacity && b.onboard > b.capacity) b.onboard = b.capacity;
              refreshBus(b);
            }
            const outboundQ = d.stop_outbound ?? d.outbound_queue ?? d.stop_queue;
            const inboundQ = d.stop_inbound ?? d.inbound_queue;
            if (typeof outboundQ === 'number') updateStopCount(d.stop_id, outboundQ, typeof inboundQ === 'number' ? inboundQ : undefined);
            // no dwell bubbles or deltas
            if (typeof d.generated_passengers === 'number') {
              totals.total = d.generated_passengers;
              if (typeof d.outbound_generated === 'number') totals.outbound = d.outbound_generated;
              if (typeof d.inbound_generated === 'number') totals.inbound = d.inbound_generated;
              if (typeof d.served_passengers === 'number') totals.served = d.served_passengers;
              if (typeof d.avg_wait_min === 'number') totals.avgWaitMin = d.avg_wait_min;
              renderLegend('Running');
            }
          }
        } catch {}
      });
      es.addEventListener('dwell', ev => {
        try {
          const d = JSON.parse((ev as MessageEvent).data);
          if (typeof d.bus_id === 'number') {
            const b = buses[d.bus_id];
            if (b && typeof d.bus_onboard === 'number') { b.onboard = d.bus_onboard; refreshBus(b); }
          }
        } catch {}
      });
      es.addEventListener('done', () => {
  renderLegend('Trip complete');
        es.close();
      });
    }
    attachHandlers();
}

init().catch(err => console.error(err));
