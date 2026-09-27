"use client";

import { useState } from "react";
import { api, ApiError, type Route } from "@/lib/api";

// Waypoints as "lat,lng" lines — >=2 required by the API.
const DEFAULT_WAYPOINTS = "12.9352,77.6245\n12.9550,77.6800\n12.9698,77.7500";

export default function PublishPage() {
  const [origin, setOrigin] = useState("Koramangala");
  const [dest, setDest] = useState("Whitefield");
  const [waypoints, setWaypoints] = useState(DEFAULT_WAYPOINTS);
  const [departure, setDeparture] = useState("08:30");
  const [mask, setMask] = useState(127);
  const [duration, setDuration] = useState(45);
  const [seats, setSeats] = useState(3);
  const [estimate, setEstimate] = useState(150);

  const [vehicleId, setVehicleId] = useState("");
  const [routes, setRoutes] = useState<Route[] | null>(null);
  const [error, setError] = useState<string | null>(null);
  const [done, setDone] = useState<Route | null>(null);
  const [busy, setBusy] = useState(false);

  function parseWaypoints(): { lat: number; lng: number }[] {
    return waypoints
      .split("\n")
      .map((l) => l.trim())
      .filter(Boolean)
      .map((line) => {
        const [lat, lng] = line.split(",").map((s) => Number(s.trim()));
        return { lat, lng };
      });
  }

  const wp = parseWaypoints();
  const wpValid = wp.length >= 2 && wp.every((p) => Number.isFinite(p.lat) && Number.isFinite(p.lng));

  async function publish(e: React.FormEvent) {
    e.preventDefault();
    setBusy(true);
    setError(null);
    setDone(null);
    try {
      const out = await api<Route>("/v1/routes/publish", {
        method: "POST",
        body: {
          vehicle_id: vehicleId,
          origin_label: origin,
          dest_label: dest,
          waypoints: wp,
          departure_time: departure,
          weekday_mask: mask,
          duration_min: duration,
          seats,
          price_estimate: estimate,
        },
      });
      setDone(out);
    } catch (err) {
      setError(err instanceof ApiError ? err.problem.detail ?? err.problem.title : "Publish failed");
    } finally {
      setBusy(false);
    }
  }

  async function loadMine() {
    setError(null);
    try {
      const out = await api<{ routes: Route[] }>("/v1/routes/mine");
      setRoutes(out.routes);
    } catch (err) {
      setError(err instanceof ApiError ? err.problem.title : "Failed to load routes");
      setRoutes([]);
    }
  }

  const dayNames = ["M", "T", "W", "T", "F", "S", "S"];

  return (
    <div className="space-y-6">
      <div>
        <h1 className="text-2xl font-bold">Publish your route</h1>
        <p className="mt-1 text-sm text-slate-500">
          Drivers only — your corridor appears in passenger search for the chosen days.
        </p>
      </div>

      <form onSubmit={publish} className="card grid gap-4 sm:grid-cols-2">
        <div className="sm:col-span-2">
          <label className="label" htmlFor="vehicle">
            Vehicle ID (UUID from your vehicle profile)
          </label>
          <input
            id="vehicle"
            className="input font-mono"
            value={vehicleId}
            onChange={(e) => setVehicleId(e.target.value)}
            placeholder="xxxxxxxx-xxxx-xxxx-xxxx-xxxxxxxxxxxx"
            required
          />
        </div>

        <div>
          <label className="label">Origin label</label>
          <input className="input" value={origin} onChange={(e) => setOrigin(e.target.value)} required />
        </div>
        <div>
          <label className="label">Destination label</label>
          <input className="input" value={dest} onChange={(e) => setDest(e.target.value)} required />
        </div>

        <div className="sm:col-span-2">
          <label className="label" htmlFor="wp">
            Waypoints — one &quot;lat,lng&quot; per line (≥2)
          </label>
          <textarea
            id="wp"
            className="input font-mono"
            rows={4}
            value={waypoints}
            onChange={(e) => setWaypoints(e.target.value)}
            required
          />
          {!wpValid && <p className="mt-1 text-xs text-red-600">Need ≥2 valid lat,lng lines.</p>}
        </div>

        <div>
          <label className="label">Departure (local)</label>
          <input
            className="input"
            type="time"
            value={departure}
            onChange={(e) => setDeparture(e.target.value)}
            required
          />
        </div>
        <div>
          <label className="label">Duration (min)</label>
          <input
            className="input"
            type="number"
            min={1}
            value={duration}
            onChange={(e) => setDuration(Number(e.target.value))}
            required
          />
        </div>

        <div className="sm:col-span-2">
          <span className="label">Weekdays (bit0 = Monday)</span>
          <div className="flex gap-2">
            {dayNames.map((d, i) => (
              <button
                key={i}
                type="button"
                aria-pressed={(mask & (1 << i)) !== 0}
                className={`h-9 w-9 rounded-full text-xs font-bold ${
                  (mask & (1 << i)) !== 0
                    ? "bg-brand-600 text-white"
                    : "bg-slate-100 text-slate-400"
                }`}
                onClick={() => setMask(mask ^ (1 << i))}
              >
                {d}
              </button>
            ))}
          </div>
        </div>

        <div>
          <label className="label">Seats offered</label>
          <input
            className="input"
            type="number"
            min={1}
            max={11}
            value={seats}
            onChange={(e) => setSeats(Number(e.target.value))}
            required
          />
        </div>
        <div>
          <label className="label">Price estimate (₹)</label>
          <input
            className="input"
            type="number"
            min={0}
            value={estimate}
            onChange={(e) => setEstimate(Number(e.target.value))}
            required
          />
        </div>

        <div className="sm:col-span-2 flex gap-3">
          <button className="btn-primary" disabled={busy || !wpValid}>
            {busy ? "Publishing…" : "Publish route"}
          </button>
          <button type="button" className="btn-ghost" onClick={loadMine}>
            My routes
          </button>
        </div>
      </form>

      {error && (
        <p role="alert" className="rounded-lg bg-red-50 px-4 py-3 text-sm text-red-700">
          {error}
        </p>
      )}

      {done && (
        <p className="rounded-lg bg-green-50 px-4 py-3 text-sm text-green-700">
          Published <strong>{done.origin_label} → {done.dest_label}</strong> ({done.id.slice(0, 8)}).
        </p>
      )}

      {routes && (
        <section className="space-y-3">
          <h2 className="font-semibold">Your routes ({routes.length})</h2>
          {routes.length === 0 && <p className="text-sm text-slate-400">Nothing published yet.</p>}
          {routes.map((r) => (
            <article key={r.id} className="card flex items-center justify-between">
              <div>
                <p className="font-semibold">
                  {r.origin_label} → {r.dest_label}
                </p>
                <p className="text-sm text-slate-500">
                  {r.departure_time} · mask {r.weekday_mask} · {r.duration_min} min · {r.seats}{" "}
                  seats · ₹{r.price_estimate}
                </p>
              </div>
              <span className="rounded-full bg-slate-100 px-3 py-1 text-xs font-semibold text-slate-600">
                {r.status}
              </span>
            </article>
          ))}
        </section>
      )}
    </div>
  );
}
