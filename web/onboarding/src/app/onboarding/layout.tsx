import Link from "next/link";
import { Suspense } from "react";

import { logout } from "@/app/actions/auth";
import { Stepper } from "@/components/stepper";
import { onboardingState } from "@/lib/api/data";

// Every wizard page reads the session; navigations block on the server by design.
export const instant = false;

async function Progress() {
  const state = await onboardingState("/onboarding");
  return <Stepper state={state} />;
}

export default function OnboardingLayout({ children }: LayoutProps<"/onboarding">) {
  return (
    <div className="flex min-h-full flex-1 flex-col bg-zinc-50 dark:bg-zinc-900">
      <a href="#main" className="sr-only focus:not-sr-only focus:absolute focus:left-2 focus:top-2 focus:rounded focus:bg-white focus:px-3 focus:py-2">
        Skip to content
      </a>
      <header className="flex items-center justify-between border-b border-zinc-200 bg-white px-4 py-3 dark:border-zinc-800 dark:bg-zinc-950">
        <Link href="/onboarding" className="text-sm font-semibold tracking-tight text-indigo-700 dark:text-indigo-300">
          Search Engine setup
        </Link>
        <form action={logout}>
          <button type="submit" className="text-sm text-zinc-600 underline dark:text-zinc-400">
            Sign out
          </button>
        </form>
      </header>
      <Suspense fallback={<div className="h-12 border-b border-zinc-200 bg-white dark:border-zinc-800 dark:bg-zinc-950" />}>
        <Progress />
      </Suspense>
      <main id="main" className="flex flex-1 flex-col">
        {children}
      </main>
    </div>
  );
}
