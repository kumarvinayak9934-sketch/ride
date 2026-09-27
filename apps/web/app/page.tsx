"use client";

import { useState } from "react";
import { api, ApiError, type Ride, type SearchResponse } from "@/lib/api";

// Leaflet-free MVP: lat/lng inputs + a preset for common corridors.
const PRESETS: Record<string, { pickup: [number, number]; drop: [number, number] }> = {
  "Koramangala → Whitefield": { pickup: [12.9352, 77.6245], drop: [12.9698, 77.750] },
  "Indiranagar → Electronic City": { pickup: [12.9719, 77.6412], drop: [12.8452, 77.6602] },
  "HSR → Manyata": { pickup: [12.9116, 77.6389], drop: [13.0358, 77.597] },
};

export default function SearchPage() {
  const [preset, setPreset] = useState<string>(Object.keys(PRESETS)[0]);
  const [pickup, setPickup] = useState(PRESETS[Object.keys(PRESETS)[0]].pickup);
  const [drop, setDrop] = useState(PRESETS[Object.keys(PRESETS)[0]].drop);
  const [date, setDate] = useState(() => new Date(Date.now() + 864e5).toISOString().slice(0, 10));
  const [time, setTime] = useState("09:00");
  const [seats, setSeats] = useState(1);

  const [loading, setLoading] = useState(false);
  const [error, setError] = useState<string | null>(null);
  const [resp, setResp] = useState<SearchResponse | null>(null);
  const [booking, setBooking] = useState<string | null>(null);

  function applyPreset(name: string) {
    setPreset(name);
    const p = PRESETS[name];
    setPickup(p.pickup);
    setDrop(p.drop);
  }

  async function search(e: React.FormEvent) {
    e.preventDefault();
    setLoading(true);
    setError(null);
    setResp(null);
    try {
      const out = await api<SearchResponse>("/v1/rides/search", {
        method: "POST",
        body: {
          pickup_lat: pickup[0],
          pickup_lng: pickup[1],
          drop_lat: drop[0],
          drop_lng: drop[1],
          date,
          time,
          seats,
        },
      });
      setResp(out);
    } catch (err) {
      setError(err instanceof ApiError ? err.problem.detail ?? err.problem.title : "Search failed");
    } finally {
      setLoading(false);
    }
  }

  async function book(ride: Ride) {
    setBooking(ride.ride_instance_id);
    setError(null);
    try {
      await api("/v1/bookings", {
        method: "POST",
        idempotencyKey: crypto.randomUUID(),
        body: { ride_instance_id: ride.ride_instance_id, seats },
      });
      window.location.href = "/bookings";
    } catch (err) {
      setError(
        err instanceof ApiError
          ? `${err.problem.title}${err.problem.detail ? ` — ${err.problem.detail}` : ""}`
          : "Booking failed",
      );
      setBooking(null);
    }
  }

  return (
    <div className="space-y-6">
      <section>
        <h1 className="text-2xl font-bold">Find a ride</h1>
        <p className="mt-1 text-sm text-slate-500">
          Corridor search within 2 km of both ends — ranked on detour, timing, rating and price.
        </p>
      </section>

      <form onSubmit={search} className="card grid gap-4 sm:grid-cols-2 lg:grid-cols-3">
        <div className="sm:col-span-2 lg:col-span-3">
          <label className="label" htmlFor="preset">
            Popular corridor
          </label>
          <select
            id="preset"
            className="input"
            value={preset}
            onChange={(e) => applyPreset(e.target.value)}
          >
            {Object.keys(PRESETS).map((name) => (
              <option key={name}>{name}</option>
            ))}
          </select>
        </div>

        <div>
          <label className="label">Pickup lat, lng</label>
          <div className="flex gap-2">
            <input
              className="input"
              type="number"
              step="any"
              value={pickup[0]}
              onChange={(e) => setPickup([Number(e.target.value), pickup[1]])}
              required
            />
            <input
              className="input"
              type="number"
              step="any"
              value={pickup[1]}
              onChange={(e) => setPickup([pickup[0], Number(e.target.value)])}
              required
            />
          </div>
        </div>

        <div>
          <label className="label">Drop lat, lng</label>
          <div className="flex gap-2">
            <input
              className="input"
              type="number"
              step="any"
              value={drop[0]}
              onChange={(e) => setDrop([Number(e.target.value), drop[1]])}
              required
            />
            <input
              className="input"
              type="number"
              step="any"
              value={drop[1]}
              onChange={(e) => setDrop([drop[0], Number(e.target.value)])}
              required
            />
          </div>
        </div>

        <div>
          <label className="label" htmlFor="date">
            Date
          </label>
          <input
            id="date"
            className="input"
            type="date"
            value={date}
            min={new Date().toISOString().slice(0, 10)}
            onChange={(e) => setDate(e.target.value)}
            required
          />
        </div>

        <div>
          <label className="label" htmlFor="time">
            Preferred time
          </label>
          <input
            id="time"
            className="input"
            type="time"
            value={time}
            onChange={(e) => setTime(e.target.value)}
          />
        </div>

        <div>
          <label className="label" htmlFor="seats">
            Seats
          </label>
          <input
            id="seats"
            className="input"
            type="number"
            min={1}
            max={11}
            value={seats}
            onChange={(e) => setSeats(Number(e.target.value))}
          />
        </div>

        <div className="sm:col-span-2 lg:col-span-3">
          <button className="btn-primary w-full sm:w-auto" disabled={loading}>
            {loading ? "Searching…" : "Search rides"}
          </button>
        </div>
      </form>

      {error && (
        <p role="alert" className="rounded-lg bg-red-50 px-4 py-3 text-sm text-red-700">
          {error}
        </p>
      )}

      {resp && resp.count === 0 && (
        <div className="card text-center text-sm text-slate-500">
          No rides on this corridor for {date}. Try another day — or{" "}
          <a className="text-brand-600 underline" href="/publish">
            publish your route
          </a>{" "}
          so others can find you.
        </div>
      )}

      {resp && resp.count > 0 && (
        <section className="space-y-3">
          <div className="flex items-baseline justify-between">
            <h2 className="font-semibold">{resp.count} ride(s)</h2>
            <span className="text-xs text-slate-400">
              radius {resp.radius_m} m · weights {resp.weights.detour}/
              {resp.weights.time}/{resp.weights.rating}/{resp.weights.price}
            </span>
          </div>
          {resp.results.map((r) => (
            <article key={r.ride_instance_id} className="card flex flex-wrap items-center gap-4">
              <div className="min-w-0 flex-1">
                <p className="font-semibold">
                  {r.origin_label} → {r.dest_label}
                </p>
                <p className="text-sm text-slate-500">
                  {r.departure_time} · {r.duration_min} min · {r.distance_km.toFixed(1)} km ·
                  detour {r.detour_km.toFixed(1)} km · ★ {r.driver_rating.toFixed(1)} ·{" "}
                  {r.seats_free} seat(s) free
                </p>
                <p className="mt-1 text-xs text-slate-400">
                  score {r.score.toFixed(2)} (detour {r.score_parts.detour.toFixed(2)} · time{" "}
                  {r.score_parts.time.toFixed(2)} · rating {r.score_parts.rating.toFixed(2)} ·
                  price {r.score_parts.price.toFixed(2)})
                </p>
              </div>
              <div className="text-right">
                <p className="text-xl font-bold">₹{r.total.toFixed(0)}</p>
                <p className="text-xs text-slate-400">per seat (₹{r.fare.toFixed(0)} + fees)</p>
              </div>
              <button
                className="btn-primary"
                disabled={booking === r.ride_instance_id}
                onClick={() => book(r)}
              >
                {booking === r.ride_instance_id ? "Booking…" : "Book"}
              </button>
            </article>
          ))}
        </section>
      )}
    </div>
  );
}
