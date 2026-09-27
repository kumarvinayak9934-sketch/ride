"use client";

import { useEffect, useState } from "react";
import Link from "next/link";
import { api, ApiError, type Booking } from "@/lib/api";

const STATUS_STYLE: Record<Booking["status"], string> = {
  PENDING_PAYMENT: "bg-amber-100 text-amber-800",
  CONFIRMED: "bg-green-100 text-green-800",
  CANCELLED: "bg-slate-100 text-slate-600",
  COMPLETED: "bg-blue-100 text-blue-800",
  EXPIRED: "bg-red-100 text-red-700",
};

export default function BookingsPage() {
  const [bookings, setBookings] = useState<Booking[] | null>(null);
  const [error, setError] = useState<string | null>(null);
  const [busy, setBusy] = useState<string | null>(null);

  async function load() {
    try {
      const out = await api<{ bookings: Booking[] }>("/v1/bookings");
      setBookings(out.bookings);
    } catch (err) {
      setError(
        err instanceof ApiError
          ? err.problem.status === 401
            ? "Please log in to see your bookings."
            : err.problem.title
          : "Failed to load bookings",
      );
      setBookings([]);
    }
  }

  useEffect(() => {
    load();
    // eslint-disable-next-line react-hooks/exhaustive-deps
  }, []);

  async function cancel(id: string) {
    if (!confirm("Cancel this booking?")) return;
    setBusy(id);
    setError(null);
    try {
      await api<Booking>(`/v1/bookings/${id}/cancel`, { method: "POST" });
      await load();
    } catch (err) {
      setError(err instanceof ApiError ? err.problem.detail ?? err.problem.title : "Cancel failed");
    } finally {
      setBusy(null);
    }
  }

  return (
    <div className="space-y-6">
      <div>
        <h1 className="text-2xl font-bold">My bookings</h1>
        <p className="mt-1 text-sm text-slate-500">
          Payment confirms via webhook — a booking sits at{" "}
          <span className="font-medium">PENDING_PAYMENT</span> until captured.
        </p>
      </div>

      {error && (
        <p role="alert" className="rounded-lg bg-red-50 px-4 py-3 text-sm text-red-700">
          {error}
        </p>
      )}

      {bookings === null && <p className="text-sm text-slate-400">Loading…</p>}

      {bookings?.length === 0 && (
        <div className="card text-center text-sm text-slate-500">
          No bookings yet.{" "}
          <Link href="/" className="text-brand-600 underline">
            Find a ride
          </Link>
        </div>
      )}

      {bookings?.map((b) => (
        <article key={b.id} className="card flex flex-wrap items-center gap-4">
          <div className="min-w-0 flex-1">
            <p className="font-semibold">
              {b.seats} seat{b.seats > 1 ? "s" : ""} · ₹{b.total.toFixed(0)}
              {b.payment && (
                <span className="ml-2 text-xs font-normal text-slate-400">
                  payment {b.payment.status}
                </span>
              )}
            </p>
            <p className="text-sm text-slate-500">
              booking {b.id.slice(0, 8)} · {new Date(b.created_at).toLocaleString()}
            </p>
          </div>
          <span
            className={`rounded-full px-3 py-1 text-xs font-semibold ${STATUS_STYLE[b.status]}`}
          >
            {b.status}
          </span>
          {(b.status === "PENDING_PAYMENT" || b.status === "CONFIRMED") && (
            <button
              className="btn-ghost"
              disabled={busy === b.id}
              onClick={() => cancel(b.id)}
            >
              {busy === b.id ? "Cancelling…" : "Cancel"}
            </button>
          )}
        </article>
      ))}
    </div>
  );
}
