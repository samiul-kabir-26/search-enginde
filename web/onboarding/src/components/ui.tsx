"use client";

import { useEffect, useRef, type ComponentProps, type ReactNode } from "react";

import type { FieldError } from "@/lib/api/types";
import { errorAt, fieldId } from "@/lib/forms";

export function cx(...classes: (string | false | null | undefined)[]): string {
  return classes.filter(Boolean).join(" ");
}

const inputClass =
  "w-full rounded-md border border-zinc-300 bg-white px-3 py-2 text-sm text-zinc-900 shadow-sm " +
  "focus:border-indigo-500 focus:outline-none focus:ring-2 focus:ring-indigo-500/40 " +
  "aria-[invalid=true]:border-red-500 dark:border-zinc-700 dark:bg-zinc-900 dark:text-zinc-100";

type FieldProps = {
  /** The server JSON Pointer this input owns; errors at it render below the input. */
  path: string;
  label: string;
  hint?: ReactNode;
  errors?: FieldError[];
  children?: (props: { id: string; "aria-invalid": boolean; "aria-describedby"?: string; className: string }) => ReactNode;
} & Omit<ComponentProps<"input">, "id" | "children">;

/** A labelled input whose server errors (by JSON Pointer) render below it. */
export function Field({ path, label, hint, errors, children, className, ...input }: FieldProps) {
  const id = fieldId(path);
  const error = errorAt(errors, path);
  const describedBy = [hint ? `${id}-hint` : null, error ? `${id}-error` : null].filter(Boolean).join(" ") || undefined;
  const props = { id, "aria-invalid": Boolean(error), "aria-describedby": describedBy, className: cx(inputClass, className) };
  return (
    <div className="flex flex-col gap-1">
      <label htmlFor={id} className="text-sm font-medium">
        {label}
      </label>
      {children ? children(props) : <input {...input} {...props} />}
      {hint && (
        <p id={`${id}-hint`} className="text-xs text-zinc-500 dark:text-zinc-400">
          {hint}
        </p>
      )}
      {error && (
        <p id={`${id}-error`} className="text-xs text-red-600 dark:text-red-400">
          {error}
        </p>
      )}
    </div>
  );
}

export function Select(props: ComponentProps<"select">) {
  return <select {...props} className={cx(inputClass, props.className)} />;
}

export function TextArea(props: ComponentProps<"textarea">) {
  return <textarea {...props} className={cx(inputClass, "font-mono", props.className)} />;
}

export function Button({ variant = "primary", className, ...props }: ComponentProps<"button"> & { variant?: "primary" | "secondary" | "ghost" | "danger" }) {
  const styles = {
    primary: "bg-indigo-600 text-white hover:bg-indigo-500 disabled:bg-indigo-300",
    secondary:
      "border border-zinc-300 bg-white text-zinc-900 hover:bg-zinc-50 dark:border-zinc-700 dark:bg-zinc-900 dark:text-zinc-100 dark:hover:bg-zinc-800",
    ghost: "text-indigo-700 hover:bg-indigo-50 dark:text-indigo-300 dark:hover:bg-zinc-800",
    danger: "bg-red-600 text-white hover:bg-red-500",
  }[variant];
  return (
    <button
      type="button"
      {...props}
      className={cx(
        "inline-flex items-center justify-center gap-2 rounded-md px-4 py-2 text-sm font-medium",
        "focus-visible:outline-none focus-visible:ring-2 focus-visible:ring-indigo-500 focus-visible:ring-offset-2 disabled:cursor-not-allowed",
        styles,
        className,
      )}
    />
  );
}

/**
 * The form-level summary: a message and every error, each linking to its field. It takes
 * focus when it appears so keyboard and screen-reader users land on it.
 */
export function ErrorSummary({ message, errors, title = "Please fix the following" }: { message?: string; errors?: FieldError[]; title?: string }) {
  const ref = useRef<HTMLDivElement>(null);
  const show = Boolean(message) || (errors?.length ?? 0) > 0;
  useEffect(() => {
    if (show) ref.current?.focus();
  }, [show, message, errors]);
  if (!show) return null;
  return (
    <div
      ref={ref}
      tabIndex={-1}
      role="alert"
      className="rounded-md border border-red-300 bg-red-50 p-3 text-sm text-red-800 focus:outline-none focus:ring-2 focus:ring-red-400 dark:border-red-800 dark:bg-red-950 dark:text-red-200"
    >
      <p className="font-medium">{message ?? title}</p>
      {errors && errors.length > 0 && (
        <ul className="mt-1 list-disc pl-5">
          {errors.map((e, i) => (
            <li key={`${e.path}-${i}`}>
              <a
                href={`#${fieldId(e.path)}`}
                className="underline"
                onClick={(ev) => {
                  const el = document.getElementById(fieldId(e.path));
                  if (el) {
                    ev.preventDefault();
                    el.focus();
                  }
                }}
              >
                {e.path ? <code className="mr-1">{e.path}</code> : null}
                {e.message}
              </a>
            </li>
          ))}
        </ul>
      )}
    </div>
  );
}

export function Warnings({ warnings }: { warnings?: FieldError[] }) {
  if (!warnings?.length) return null;
  return (
    <div role="status" className="rounded-md border border-amber-300 bg-amber-50 p-3 text-sm text-amber-900 dark:border-amber-800 dark:bg-amber-950 dark:text-amber-100">
      <p className="font-medium">Saved with warnings</p>
      <ul className="mt-1 list-disc pl-5">
        {warnings.map((w, i) => (
          <li key={`${w.path}-${i}`}>
            <code className="mr-1">{w.path}</code>
            {w.message}
          </li>
        ))}
      </ul>
    </div>
  );
}

export function Card({ title, children, className }: { title?: ReactNode; children: ReactNode; className?: string }) {
  return (
    <section className={cx("rounded-lg border border-zinc-200 bg-white p-4 shadow-sm dark:border-zinc-800 dark:bg-zinc-950", className)}>
      {title && <h2 className="mb-3 text-base font-semibold">{title}</h2>}
      {children}
    </section>
  );
}

export function Badge({ children, tone = "zinc" }: { children: ReactNode; tone?: "zinc" | "green" | "indigo" | "amber" | "red" }) {
  const tones = {
    zinc: "bg-zinc-100 text-zinc-700 dark:bg-zinc-800 dark:text-zinc-200",
    green: "bg-emerald-100 text-emerald-800 dark:bg-emerald-900 dark:text-emerald-100",
    indigo: "bg-indigo-100 text-indigo-800 dark:bg-indigo-900 dark:text-indigo-100",
    amber: "bg-amber-100 text-amber-800 dark:bg-amber-900 dark:text-amber-100",
    red: "bg-red-100 text-red-800 dark:bg-red-900 dark:text-red-100",
  }[tone];
  return <span className={cx("inline-flex items-center rounded-full px-2 py-0.5 text-xs font-medium", tones)}>{children}</span>;
}
