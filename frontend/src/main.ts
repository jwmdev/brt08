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
  const data = await loadRoute();
  const stops = data.stops;

  const map = L.map('map');
  const osm = L.tileLayer('https://{s}.tile.openstreetmap.org/{z}/{x}/{y}.png', {
    maxZoom: 19,
    attribution: '&copy; OpenStreetMap contributors'
  });
  osm.addTo(map);

  // Fit map to stops bounds
  const group = L.featureGroup(stops.map(createStopMarker));
  group.addTo(map);
  map.fitBounds(group.getBounds().pad(0.15));

  // Draw polyline
  const polylineLatLngs = stops.map(s => [s.latitute, s.longtude]) as [number, number][];
  const routeLine = L.polyline(polylineLatLngs, { color: '#1976d2', weight: 4, opacity: 0.8 }).addTo(map);

  // Build animation segments
  const segments = buildSegments(stops);
  const totalKm = segments.reduce((s, seg) => s + seg.lengthKm, 0);

  // Bus marker
  const bus = L.marker([stops[0].latitute, stops[0].longtude], { icon: createBusIcon() }).addTo(map);

  // Animation settings
  const speedKmph = 25; // simulation speed
  const realTimeFactor = 50; // higher = faster animation
  const metersPerMs = (speedKmph * 1000) / (3600 * realTimeFactor);

  let distMeters = 0;
  const totalMeters = totalKm * 1000;
  let lastTs: number | null = null;

  function updateLegend(progressPct: number, currentStop: Stop) {
    const el = document.getElementById('legend');
    if (!el) return;
    el.innerHTML = `Route: ${data.route}<br/>Progress: ${progressPct.toFixed(1)}%<br/>Current: ${currentStop.stop_name}`;
  }

  function frame(ts: number) {
    if (lastTs == null) lastTs = ts;
    const delta = ts - lastTs;
    lastTs = ts;

    distMeters += delta * metersPerMs;
    if (distMeters >= totalMeters) {
      distMeters = totalMeters;
    }

    // Determine segment
    const distKm = distMeters / 1000;
    let seg = segments[segments.length -1];
    for (const s of segments) {
      if (distKm >= s.cumulativeStart && distKm < s.cumulativeStart + s.lengthKm) { seg = s; break; }
    }
    const segProgress = Math.min(1, (distKm - seg.cumulativeStart) / seg.lengthKm);
    const lat = interpolate(seg.from.latitute, seg.to.latitute, segProgress);
    const lng = interpolate(seg.from.longtude, seg.to.longtude, segProgress);
    bus.setLatLng([lat, lng]);

    const progressPct = (distMeters / totalMeters) * 100;
    updateLegend(progressPct, seg.from);

    if (distMeters < totalMeters) requestAnimationFrame(frame);
    else updateLegend(100, segments[segments.length -1].to);
  }

  updateLegend(0, stops[0]);
  requestAnimationFrame(frame);

  // Optional: click to restart
  (routeLine as any).on('click', () => {
    distMeters = 0; lastTs = null; requestAnimationFrame(frame);
  });
}

init().catch(err => console.error(err));
