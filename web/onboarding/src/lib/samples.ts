import { useSyncExternalStore } from "react";

/**
 * The catalog step's sample products, kept in this browser tab for the policies step's rule
 * tests. They are test data, not config, and the pages work without them: when storage is
 * unavailable (private mode, quota) they live in memory for the session.
 */
const KEY = "se:catalog-samples";
const EMPTY: unknown[] = [];

let memory: unknown[] = EMPTY;
let cache: { raw: string | null; value: unknown[] } = { raw: null, value: EMPTY };
const listeners = new Set<() => void>();

function read(): unknown[] {
  let raw: string | null;
  try {
    raw = sessionStorage.getItem(KEY);
  } catch {
    return memory;
  }
  if (raw === null) return memory;
  if (raw !== cache.raw) {
    let value: unknown[] = EMPTY;
    try {
      const v: unknown = JSON.parse(raw);
      if (Array.isArray(v)) value = v;
    } catch {
      // A corrupt entry reads as no samples.
    }
    cache = { raw, value };
  }
  return cache.value;
}

function subscribe(cb: () => void) {
  listeners.add(cb);
  return () => {
    listeners.delete(cb);
  };
}

export function saveSamples(samples: unknown[]) {
  memory = samples.slice(0, 20);
  try {
    sessionStorage.setItem(KEY, JSON.stringify(memory));
  } catch {
    // Kept in memory instead.
  }
  for (const l of listeners) l();
}

/** The stored samples; empty during server rendering. */
export function useSamples(): unknown[] {
  return useSyncExternalStore(subscribe, read, () => EMPTY);
}
