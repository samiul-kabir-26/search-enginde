import Link from "next/link";

export default function Home() {
  return (
    <main id="main" className="mx-auto flex w-full max-w-2xl flex-1 flex-col justify-center gap-6 px-4 py-10">
      <h1 className="text-3xl font-semibold tracking-tight">Search Engine onboarding</h1>
      <p className="text-zinc-600 dark:text-zinc-400">
        Set up product search for your marketplace: connect your catalog, keep the platform defaults or bring your own models, and try every change in a playground before it goes live.
      </p>
      <div className="flex flex-wrap gap-3">
        <Link href="/signup" className="rounded-md bg-indigo-600 px-4 py-2 text-sm font-medium text-white hover:bg-indigo-500">
          Create a workspace
        </Link>
        <Link href="/login" className="rounded-md border border-zinc-300 px-4 py-2 text-sm font-medium hover:bg-zinc-50 dark:border-zinc-700 dark:hover:bg-zinc-900">
          Sign in
        </Link>
      </div>
      <Link href="/admin/login" className="text-sm text-zinc-500 underline">
        Platform admin
      </Link>
    </main>
  );
}
