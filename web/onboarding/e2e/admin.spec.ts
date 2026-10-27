import { readFileSync } from "node:fs";
import path from "node:path";

import { expect, test } from "@playwright/test";

/** The platform admin settings flow (PLAN.md M9): sign in, change settings, see them persist. */

function adminCredentials(): { email: string; password: string } {
  const env = Object.fromEntries(
    readFileSync(path.resolve(__dirname, "../../../.env"), "utf8")
      .split("\n")
      .filter((l) => l.includes("=") && !l.startsWith("#"))
      .map((l) => [l.slice(0, l.indexOf("=")), l.slice(l.indexOf("=") + 1)]),
  );
  return {
    email: process.env.PLATFORM_ADMIN_EMAIL ?? env.PLATFORM_ADMIN_EMAIL,
    password: process.env.PLATFORM_ADMIN_PASSWORD ?? env.PLATFORM_ADMIN_PASSWORD,
  };
}

test("a platform admin changes settings and ad controls", async ({ page }) => {
  const { email, password } = adminCredentials();
  await page.goto("/admin/login");
  await page.getByLabel("Email").fill(email);
  await page.getByLabel("Password").fill(password);
  await page.getByRole("button", { name: "Sign in" }).click();
  await expect(page).toHaveURL(/\/admin\/settings$/);

  // Authentication settings: an out-of-range value is refused at its field, a valid one saves.
  const minLength = page.getByLabel("Minimum password length");
  const original = await minLength.inputValue();
  await minLength.fill("4");
  await page.getByRole("button", { name: "Save settings" }).click();
  await expect(minLength).toHaveAttribute("aria-invalid", "true");
  await minLength.fill("13");
  await page.getByRole("button", { name: "Save settings" }).click();
  await expect(page.getByText(/Settings saved/).filter({ visible: true })).toBeVisible();
  await page.reload();
  await expect(page.getByLabel("Minimum password length")).toHaveValue("13");
  await page.getByLabel("Minimum password length").fill(original);
  await page.getByRole("button", { name: "Save settings" }).click();
  await expect(page.getByText(/Settings saved/).filter({ visible: true })).toBeVisible();

  // Ad controls.
  await page.getByRole("link", { name: "Ad controls" }).click();
  const maxSlots = page.getByLabel("Max ad slots per tenant");
  const slots = await maxSlots.inputValue();
  await maxSlots.fill("7");
  await page.getByRole("button", { name: "Save ad controls" }).click();
  await expect(page.getByText(/Settings saved/).filter({ visible: true })).toBeVisible();
  await page.reload();
  await expect(page.getByLabel("Max ad slots per tenant")).toHaveValue("7");
  await page.getByLabel("Max ad slots per tenant").fill(slots);
  await page.getByRole("button", { name: "Save ad controls" }).click();
  await expect(page.getByText(/Settings saved/).filter({ visible: true })).toBeVisible();

  // The other sections render.
  for (const [link, heading] of [
    ["Plans", "Plans"],
    ["Signing keys", "Signing keys"],
    ["Tenants", "Tenants"],
  ]) {
    await page.getByRole("link", { name: link, exact: true }).click();
    await expect(page.getByRole("heading", { level: 1, name: heading })).toBeVisible();
  }
});
