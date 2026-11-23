import { expect, test } from "@playwright/test";
import { clearMailbox } from "./helpers.js";

test.beforeEach(async ({ request }) => {
	await clearMailbox(request);
});

test("desktop mailbox layout fits the viewport and shows the mailbox navigation", async ({ page }) => {
	await page.setViewportSize({ width: 1440, height: 900 });
	await page.goto("/");
	await expect(page.getByRole("img", { name: "Mailpit" })).toBeVisible();
	await expect(page.getByText("No messages in your mailbox")).toBeVisible();
	await expect(page.locator(".navbar form")).toBeVisible();
	await expect(page.locator(".row.flex-fill > .d-md-flex")).toBeVisible();
	expect(await page.evaluate(() => document.documentElement.scrollWidth)).toBeLessThanOrEqual(1440);
});

test("mobile layout keeps the mailbox menu usable without horizontal overflow", async ({ page }) => {
	await page.setViewportSize({ width: 390, height: 844 });
	await page.goto("/");
	await expect(page.getByText("No messages in your mailbox")).toBeVisible();
	const menu = page.locator('button.btn-outline-light[data-bs-target="#offcanvas"]');
	await expect(menu).toBeVisible();
	await menu.click();
	await expect(page.locator("#offcanvas")).toHaveClass(/show/);
	expect(await page.evaluate(() => document.documentElement.scrollWidth)).toBeLessThanOrEqual(390);
});
