import type { ConnectionTestResult } from "@/lib/api/types";

/** Probe checks from a model validation or connection test. */
export function Checks({ checks }: { checks: ConnectionTestResult["checks"] }) {
  return (
    <ul className="flex flex-col gap-1 text-sm">
      {checks.map((c, i) => (
        <li key={`${c.name}-${i}`} className="flex items-start gap-2">
          <span aria-hidden className={c.ok ? "text-emerald-600" : "text-red-600"}>
            {c.ok ? "✓" : "✕"}
          </span>
          <span>
            <span className="font-medium">{c.name}</span>
            <span className="sr-only">{c.ok ? " passed" : " failed"}</span>
            {c.latency_ms !== undefined && <span className="text-zinc-500"> · {c.latency_ms} ms</span>}
            {c.message && <span className="block text-xs text-zinc-600 dark:text-zinc-400">{c.message}</span>}
          </span>
        </li>
      ))}
    </ul>
  );
}
