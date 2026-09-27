// Typed browser client for the Safar Share API. All calls go through the
// same-origin /api proxy (next.config.mjs rewrites) — never a hardcoded host.

export type Problem = {
  type?: string;
  title: string;
  status: number;
  detail?: string;
};

export type Session = {
  token: string;
  token_type: string;
  expires_at: string;
  user: { id: string; role: string };
};

export type Ride = {
  ride_instance_id: string;
  route_id: string;
  driver_id: string;
  driver_rating: number;
  origin_label: string;
  dest_label: string;
  departure_time: string;
  duration_min: number;
  distance_km: number;
  seats_free: number;
  fare: number;
  total: number;
  detour_km: number;
  detour_min: number;
  price_estimate: number;
  score: number;
  score_parts: { detour: number; time: number; rating: number; price: number };
};

export type SearchResponse = {
  results: Ride[];
  count: number;
  weights: { detour: number; time: number; rating: number; price: number };
  radius_m: number;
};

export type Booking = {
  id: string;
  ride_instance_id: string;
  passenger_id: string;
  seats: number;
  status: "PENDING_PAYMENT" | "CONFIRMED" | "CANCELLED" | "COMPLETED" | "EXPIRED";
  fare: number;
  total: number;
  idempotency_key?: string;
  created_at: string;
  payment?: { id: string; amount: number; status: string; provider?: string };
};

export type Waypoint = { lat: number; lng: number };

export type Route = {
  id: string;
  driver_id: string;
  vehicle_id: string;
  origin_label: string;
  dest_label: string;
  waypoints?: Waypoint[];
  departure_time: string;
  weekday_mask: number;
  duration_min: number;
  distance_km: number;
  seats: number;
  price_estimate: number;
  status: string;
};

const BASE = process.env.NEXT_PUBLIC_API_BASE ?? "/api";

export class ApiError extends Error {
  problem: Problem;
  constructor(problem: Problem) {
    super(problem.detail ?? problem.title);
    this.problem = problem;
  }
}

function token(): string | null {
  if (typeof window === "undefined") return null;
  return window.localStorage.getItem("safar.token");
}

export function saveSession(s: Session) {
  window.localStorage.setItem("safar.token", s.token);
  window.localStorage.setItem("safar.user", JSON.stringify(s.user));
}

export function currentUser(): { id: string; role: string } | null {
  if (typeof window === "undefined") return null;
  const raw = window.localStorage.getItem("safar.user");
  return raw ? (JSON.parse(raw) as { id: string; role: string }) : null;
}

export function clearSession() {
  window.localStorage.removeItem("safar.token");
  window.localStorage.removeItem("safar.user");
}

export async function api<T>(
  path: string,
  opts: { method?: string; body?: unknown; idempotencyKey?: string } = {},
): Promise<T> {
  const headers: Record<string, string> = { "Content-Type": "application/json" };
  const t = token();
  if (t) headers.Authorization = `Bearer ${t}`;
  if (opts.idempotencyKey) headers["Idempotency-Key"] = opts.idempotencyKey;

  const res = await fetch(`${BASE}${path}`, {
    method: opts.method ?? "GET",
    headers,
    body: opts.body === undefined ? undefined : JSON.stringify(opts.body),
  });

  const text = await res.text();
  const data = text ? JSON.parse(text) : null;

  if (!res.ok) {
    // RFC 7807 problem+json (or a bare status as fallback)
    const problem: Problem =
      data && typeof data === "object" && "title" in data
        ? (data as Problem)
        : { title: res.statusText || "Request failed", status: res.status };
    throw new ApiError(problem);
  }
  return data as T;
}
