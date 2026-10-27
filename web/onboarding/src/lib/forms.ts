import type { FieldError } from "./api/types";

/** The first error at exactly this JSON Pointer. */
export function errorAt(errors: FieldError[] | undefined, path: string): string | undefined {
  return errors?.find((e) => e.path === path)?.message;
}

/** Every error at or below this JSON Pointer, e.g. all of `/policy/rules/2`. */
export function errorsUnder(errors: FieldError[] | undefined, prefix: string): FieldError[] {
  return errors?.filter((e) => e.path === prefix || e.path.startsWith(prefix + "/")) ?? [];
}

/** Errors no field claims, shown in the form-level summary. */
export function unclaimed(errors: FieldError[] | undefined, claimed: (path: string) => boolean): FieldError[] {
  return errors?.filter((e) => !claimed(e.path)) ?? [];
}

/** A DOM id for the input that owns a JSON Pointer, so the summary can link and focus it. */
export function fieldId(path: string): string {
  return "f" + path.replace(/[^a-zA-Z0-9]+/g, "-");
}

/** The JSON Pointer of a server path for a field the form names differently. */
export function pointer(...parts: (string | number)[]): string {
  return "/" + parts.map((p) => String(p).replace(/~/g, "~0").replace(/\//g, "~1")).join("/");
}

export function formatMinor(minor: number, currency: string): string {
  try {
    return new Intl.NumberFormat("en", { style: "currency", currency }).format(minor / 100);
  } catch {
    return `${(minor / 100).toFixed(2)} ${currency}`;
  }
}
