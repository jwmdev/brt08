export interface Stop {
  stop_id: number;
  stop_name: string;
  latitute: number;
  longtude: number;
  distance_next_stop: number;
}

export interface Pin {
  left_stop_id: number;
  right_stop_id: number;
  latitute: number;
  longtude: number;
}

export interface RouteData {
  route: string;
  direction: string;
  unit_distance: string;
  total_distance_km: number;
  stops: Stop[];
  pins?: Pin[];
}
