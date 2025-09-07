export interface Stop {
  stop_id: number;
  stop_name: string;
  latitute: number; // intentionally keeping backend field names
  longtude: number;
  distance_next_stop: number; // km
}

export interface RouteData {
  route: string;
  direction: string;
  unit_distance: string;
  total_distance_km: number;
  stops: Stop[];
}
