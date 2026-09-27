"use client";

import { useState } from "react";
import { api, ApiError, saveSession, type Session } from "@/lib/api";

export default function LoginPage() {
  const [phone, setPhone] = useState("+91");
  const [code, setCode] = useState("");
  const [age, setAge] = useState(false);
  const [sent, setSent] = useState(false);
  const [devCode, setDevCode] = useState<string | null>(null);
  const [error, setError] = useState<string | null>(null);
  const [busy, setBusy] = useState(false);

  async function requestOtp(e: React.FormEvent) {
    e.preventDefault();
    setBusy(true);
    setError(null);
    try {
      const out = await api<{ status: string; dev_code?: string }>("/v1/auth/otp/request", {
        method: "POST",
        body: { phone },
      });
      setSent(true);
      setDevCode(out.dev_code ?? null);
    } catch (err) {
      setError(err instanceof ApiError ? err.problem.detail ?? err.problem.title : "Request failed");
    } finally {
      setBusy(false);
    }
  }

  async function verifyOtp(e: React.FormEvent) {
    e.preventDefault();
    setBusy(true);
    setError(null);
    try {
      const session = await api<Session>("/v1/auth/otp/verify", {
        method: "POST",
        body: { phone, code: code.trim(), age_confirmed: age },
      });
      saveSession(session);
      window.location.href = "/";
    } catch (err) {
      setError(err instanceof ApiError ? err.problem.detail ?? err.problem.title : "Verify failed");
    } finally {
      setBusy(false);
    }
  }

  return (
    <div className="mx-auto max-w-md space-y-6">
      <div>
        <h1 className="text-2xl font-bold">Log in</h1>
        <p className="mt-1 text-sm text-slate-500">
          OTP on your phone — we store only an HMAC hash and an encrypted number (DPDP).
        </p>
      </div>

      {!sent ? (
        <form onSubmit={requestOtp} className="card space-y-4">
          <div>
            <label className="label" htmlFor="phone">
              Mobile number
            </label>
            <input
              id="phone"
              className="input"
              type="tel"
              placeholder="+919876543210"
              value={phone}
              onChange={(e) => setPhone(e.target.value)}
              pattern="\\+91[6-9][0-9]{9}"
              title="E.164 format: +91 followed by 10 digits"
              required
            />
          </div>
          <button className="btn-primary w-full" disabled={busy}>
            {busy ? "Sending…" : "Send OTP"}
          </button>
        </form>
      ) : (
        <form onSubmit={verifyOtp} className="card space-y-4">
          <div>
            <label className="label" htmlFor="code">
              6-digit code
            </label>
            <input
              id="code"
              className="input tracking-[0.4em]"
              inputMode="numeric"
              maxLength={6}
              minLength={6}
              value={code}
              onChange={(e) => setCode(e.target.value.replace(/\D/g, ""))}
              required
            />
            {devCode && (
              <p className="mt-2 rounded bg-amber-50 px-3 py-2 text-xs text-amber-700">
                Dev echo: <strong>{devCode}</strong> (OTP_DEV_ECHO is on)
              </p>
            )}
          </div>
          <label className="flex items-start gap-2 text-sm text-slate-600">
            <input
              type="checkbox"
              checked={age}
              onChange={(e) => setAge(e.target.checked)}
              required
              className="mt-0.5"
            />
            I confirm I am 18 or older.
          </label>
          <button className="btn-primary w-full" disabled={busy}>
            {busy ? "Verifying…" : "Verify & continue"}
          </button>
          <button
            type="button"
            className="btn-ghost w-full"
            onClick={() => {
              setSent(false);
              setCode("");
            }}
          >
            Change number
          </button>
        </form>
      )}

      {error && (
        <p role="alert" className="rounded-lg bg-red-50 px-4 py-3 text-sm text-red-700">
          {error}
        </p>
      )}
    </div>
  );
}
