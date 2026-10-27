import { adminLogout } from "@/app/actions/auth";

import { AdminNav } from "./admin-nav";

// Every admin page reads the admin session; navigations block on the server by design.
export const instant = false;

export default function AdminLayout({ children }: { children: React.ReactNode }) {
  return (
    <div className="flex min-h-full flex-1 flex-col bg-zinc-50 dark:bg-zinc-900">
      <header className="flex items-center justify-between border-b border-zinc-200 bg-white px-4 py-3 dark:border-zinc-800 dark:bg-zinc-950">
        <span className="text-sm font-semibold tracking-tight text-indigo-700 dark:text-indigo-300">Search Engine · Platform admin</span>
        <form action={adminLogout}>
          <button type="submit" className="text-sm text-zinc-600 underline dark:text-zinc-400">
            Sign out
          </button>
        </form>
      </header>
      <AdminNav />
      <main id="main" className="mx-auto flex w-full max-w-5xl flex-1 flex-col gap-6 px-4 py-6">
        {children}
      </main>
    </div>
  );
}
