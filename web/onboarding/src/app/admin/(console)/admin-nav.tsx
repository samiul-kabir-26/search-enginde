"use client";

import Link from "next/link";
import { usePathname } from "next/navigation";

import { cx } from "@/components/ui";

const SECTIONS = [
  ["settings", "Settings"],
  ["ads", "Ad controls"],
  ["plans", "Plans"],
  ["keys", "Signing keys"],
  ["tenants", "Tenants"],
] as const;

export function AdminNav() {
  const path = usePathname();
  return (
    <nav aria-label="Admin sections" className="border-b border-zinc-200 bg-white px-4 dark:border-zinc-800 dark:bg-zinc-950">
      <ul className="flex flex-wrap gap-1">
        {SECTIONS.map(([id, label]) => {
          const current = path === `/admin/${id}`;
          return (
            <li key={id}>
              <Link
                href={`/admin/${id}`}
                aria-current={current ? "page" : undefined}
                className={cx(
                  "inline-block border-b-2 px-3 py-2 text-sm",
                  current ? "border-indigo-600 font-semibold text-indigo-800 dark:text-indigo-200" : "border-transparent text-zinc-600 hover:text-zinc-900 dark:text-zinc-400",
                )}
              >
                {label}
              </Link>
            </li>
          );
        })}
      </ul>
    </nav>
  );
}
