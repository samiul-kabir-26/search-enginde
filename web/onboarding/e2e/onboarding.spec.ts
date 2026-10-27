import { readFileSync } from "node:fs";

import { expect, test, type Page } from "@playwright/test";

import { CATALOG, META } from "./global-setup";

/**
 * The M9 acceptance test (docs/ONBOARDING.md §6): a new tenant goes from sign-up to a live
 * search playground and activates its version. Needs the dev stack: `make up-models`, the
 * control-api, search-api and indexer (`make run-*`), and `make seed` once for the demo images.
 */

const MAILPIT = process.env.MAILPIT_URL ?? "http://localhost:8025";
const PASSWORD = "correct horse battery staple";

type Meta = { query: string; image: string; productId: string };

async function verificationLink(email: string): Promise<string> {
  for (let i = 0; i < 30; i++) {
    const res = await fetch(`${MAILPIT}/api/v1/search?query=${encodeURIComponent(`to:"${email}"`)}`);
    const list = (await res.json()) as { messages?: { ID: string }[] };
    const id = list.messages?.[0]?.ID;
    if (id) {
      const msg = (await (await fetch(`${MAILPIT}/api/v1/message/${id}`)).json()) as { Text?: string; HTML?: string };
      const m = /https?:\/\/\S+\/verify-email\?token=[A-Za-z0-9_\-.%]+/.exec(`${msg.Text ?? ""} ${msg.HTML ?? ""}`);
      if (m) return m[0];
    }
    await new Promise((r) => setTimeout(r, 1000));
  }
  throw new Error(`no verification email for ${email}`);
}

async function continueStep(page: Page, next: string) {
  await page.getByRole("button", { name: "Save and continue" }).click();
  await expect(page).toHaveURL(new RegExp(`/onboarding/${next}$`));
}

test("a new tenant goes from sign-up to a live search playground", async ({ page }) => {
  test.setTimeout(6 * 60_000);
  const meta = JSON.parse(readFileSync(META, "utf8")) as Meta;
  const slug = `e2e-${Date.now().toString(36)}`;
  const email = `owner@${slug}.test`;

  // 1. Sign up.
  await page.goto("/signup");
  await page.getByLabel("Company name").fill("E2E Outfitters");
  await page.getByLabel("Workspace slug").fill(slug);
  await page.getByLabel("Work email").fill(email);
  await page.getByLabel("Password").fill(PASSWORD);
  await page.getByRole("button", { name: "Create workspace" }).click();
  await expect(page).toHaveURL(/\/onboarding\/engine_tour$/);

  // 2. Complete the tour: open a stage panel, then finish.
  await expect(page.getByRole("heading", { level: 1, name: "Engine tour" })).toBeFocused();
  const stage = page.getByRole("button", { name: /Apply your rules/ }).first();
  await stage.click();
  await expect(stage).toHaveAttribute("aria-expanded", "true");
  await expect(page.getByRole("heading", { name: /Apply your rules/, level: 3 })).toBeVisible();
  await page.getByRole("button", { name: "Finish the tour" }).click();
  await expect(page).toHaveURL(/\/onboarding\/catalog$/);

  // 3. Upload the sample JSONL and map fields (the default mapping matches the storefront shape).
  await page.getByLabel("Batch upload").check();
  await page.getByLabel("Catalog file (JSONL)").setInputFiles(CATALOG);
  await expect(page.getByText(/Uploaded 100 products/)).toBeVisible({ timeout: 30_000 });
  await expect(page.getByLabel("product_id")).toHaveValue("$.id");
  await expect(page.locator("pre").filter({ hasText: meta.productId })).toBeVisible({ timeout: 15_000 });
  await continueStep(page, "retrieval");

  // 4. Keep the defaults.
  await continueStep(page, "policies");

  // 5. Add one CEL filter, test it against the samples, save.
  await page.getByRole("button", { name: "Add a rule" }).click();
  await page.getByLabel("CEL expression").fill("item.price_minor > 0");
  await page.getByRole("button", { name: "Test against sample products" }).click();
  await expect(page.getByText(/Keeps 20 of 20 samples/)).toBeVisible({ timeout: 15_000 });
  await continueStep(page, "ranking");
  await continueStep(page, "personalization");
  await continueStep(page, "index_build");

  // 6. Publish & build.
  await page.getByRole("button", { name: "Publish & build" }).click();
  await expect(page.getByTestId("index-status")).toContainText("Index built", { timeout: 4 * 60_000 });
  await page.getByRole("link", { name: "Try it in the playground" }).click();
  await expect(page).toHaveURL(/\/onboarding\/playground$/);

  // 7. One text and one image search, with results.
  await page.getByRole("searchbox", { name: "Search query" }).fill(meta.query);
  const results = page.getByTestId("playground-results");
  // A freshly started search-api can time out its first queries while connections open
  // (PLAN.md D38); the explain trace shows it, and a retry succeeds.
  await expect(async () => {
    await page.getByRole("button", { name: "Search", exact: true }).click();
    await expect(results.getByRole("listitem").first()).toBeVisible({ timeout: 10_000 });
  }).toPass({ timeout: 60_000 });
  await expect(page.getByRole("table", { name: /Explain trace/ })).toBeVisible();

  const imageHeading = page.getByRole("heading", { name: /^Version 1 · \d+ results/ });
  await expect(async () => {
    await page.getByLabel("Or search by image").setInputFiles([]);
    await page.getByLabel("Or search by image").setInputFiles(meta.image);
    await expect(imageHeading).toHaveText(/· [1-9]\d* results/, { timeout: 15_000 });
  }).toPass({ timeout: 60_000 });
  await expect(results.getByRole("listitem").first()).toBeVisible();
  await page.getByRole("button", { name: "Looks good: go live" }).click();
  await expect(page).toHaveURL(/\/onboarding\/go_live$/);

  // 8. Verify the email, then activate the version.
  await page.goto(await verificationLink(email));
  await expect(page.getByText("Your email address is verified.")).toBeVisible();
  await page.goto("/onboarding/go_live");
  await page.getByRole("button", { name: "Activate and go live" }).click();
  await expect(page.getByText(/Version 1 is serving live traffic/)).toBeVisible({ timeout: 30_000 });
  await expect(page.getByTestId("api-key")).not.toBeEmpty();
});

test("a skipped-ahead step redirects to the current step", async ({ page }) => {
  const slug = `e2e-guard-${Date.now().toString(36)}`;
  await page.goto("/signup");
  await page.getByLabel("Company name").fill("Guard Test");
  await page.getByLabel("Workspace slug").fill(slug);
  await page.getByLabel("Work email").fill(`owner@${slug}.test`);
  await page.getByLabel("Password").fill(PASSWORD);
  await page.getByRole("button", { name: "Create workspace" }).click();
  await expect(page).toHaveURL(/\/onboarding\/engine_tour$/);
  await page.goto("/onboarding/index_build");
  await expect(page).toHaveURL(/\/onboarding\/engine_tour$/);
});

test("server field errors map to their inputs", async ({ page }) => {
  await page.goto("/signup");
  await page.getByLabel("Company name").fill("Taken Slug");
  await page.getByLabel("Workspace slug").fill("demo"); // the seeded tenant
  await page.getByLabel("Work email").fill("someone@taken.test");
  await page.getByLabel("Password").fill(PASSWORD);
  await page.getByRole("button", { name: "Create workspace" }).click();
  const slug = page.getByLabel("Workspace slug");
  await expect(slug).toHaveAttribute("aria-invalid", "true");
  await expect(page.getByText("choose another slug")).toBeVisible();
  await expect(slug).toHaveValue("demo");
});
