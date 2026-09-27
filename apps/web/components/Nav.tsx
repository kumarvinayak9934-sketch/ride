"use client";

import Link from "next/link";
import { useEffect, useState } from "react";
import { clearSession, currentUser } from "@/lib/api";

export function Nav() {
  const [user, setUser] = useState<{ id: string; role: string } | null>(null);

  useEffect(() => {
    setUser(currentUser());
    const onStorage = () => setUser(currentUser());
    window.addEventListener("storage", onStorage);
    return () => window.removeEventListener("storage", onStorage);
  }, []);

  return (
    <div className="flex items-center gap-4 text-sm">
      <Link href="/" className="text-slate-600 hover:text-brand-700">
        Find a ride
      </Link>
      <Link href="/publish" className="text-slate-600 hover:text-brand-700">
        Publish route
      </Link>
      <Link href="/bookings" className="text-slate-600 hover:text-brand-700">
        My bookings
      </Link>
      {user ? (
        <button
          className="btn-ghost"
          onClick={() => {
            clearSession();
            setUser(null);
          }}
        >
          Log out
        </button>
      ) : (
        <Link href="/login" className="btn-primary">
          Log in
        </Link>
      )}
    </div>
  );
}
