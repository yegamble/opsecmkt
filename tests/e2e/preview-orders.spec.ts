import { expect, test } from '@playwright/test';
import { PREVIEW_DRAFT_ID } from './preview-routes';

// P3 order views in the read-only preview (sample data, JavaScript disabled).
test('order page shows history, unavailable payment and the buyer actions', async ({ page }) => {
  await page.goto(`/order?id=${PREVIEW_DRAFT_ID}`);
  await expect(page.getByRole('heading', { name: 'History' })).toBeVisible();
  await expect(page.locator('.order-timeline li')).toHaveCount(1);
  await expect(page.locator('.key-values')).toContainText('Draft — unfunded');
  await expect(page.locator('form[action="/orders/pay"]')).toHaveCount(0);
  await expect(page.getByRole('button', { name: 'Request payment address' })).toHaveCount(0);
  await expect(page.getByRole('button', { name: 'Cancel draft' })).toBeVisible();
  // A-164: the order names its listing and links to it as it is now.
  const listing = page.locator('.key-values > div', { has: page.getByText('Listing', { exact: true }) }).locator('dd');
  await expect(listing.getByRole('link', { name: 'Encrypted USB Drive — 256GB', exact: true })).toHaveAttribute('href', '/product?id=encrypted-drive');
  await expect(listing).toContainText('Listing as it is now; it may have changed since purchase.');
  await expect(page.locator('script')).toHaveCount(0);
  expect(await page.evaluate(() => document.documentElement.scrollWidth)).toBeLessThanOrEqual(page.viewportSize()!.width);
});

test('plaintext order notes say who reads them and to send addresses encrypted', async ({ page }) => {
  // A-80: the cancel reason is stored unencrypted; its warning is the field's accessible description. The
  // preview draft carries only this note; the ship, dispute and review warnings are checked by the Go
  // database tests and the wallet journey.
  await page.goto(`/order?id=${PREVIEW_DRAFT_ID}`);
  await expect(page.getByLabel('Reason (optional)')).toHaveAccessibleDescription(
    /^Stored unencrypted\. The vendor reads it in the order history, .*moderators and administrators if the order is disputed .*the operator and anyone with a backup\. Never include an address, real name or tracking number: send those encrypted with Message vendor on this page\.$/);
  await expect(page.locator('#note-help-cancelled').getByRole('link', { name: 'Stored unencrypted.', exact: true })).toHaveAttribute('href', '/canary#records');
});

test('reviews are labelled verified purchases without reviewer handles', async ({ page }) => {
  await page.goto('/product?id=encrypted-drive');
  const reviews = page.locator('.order-reviews');
  await expect(reviews.getByRole('heading', { name: 'Verified reviews' })).toBeVisible();
  await expect(reviews.locator('.badge')).toHaveText('Completed order (test network)');
  await expect(reviews).toContainText('Rated 4 out of 5');
  await expect(reviews).not.toContainText('preview_user');
});

test('moderator desk offers no outcome for a dispute without escrow', async ({ page }) => {
  await page.goto('/moderator');
  await expect(page.locator('form[action="/resolve"]')).toHaveCount(0);
  await expect(page.getByRole('group', { name: 'Outcome' })).toHaveCount(0);
  await expect(page.locator('main')).toContainText('This order has no escrow, so no outcome can be recorded.');
  // A-124 and A-162 (sample data): the reply with its role and line breaks, whom each party messaged, and the
  // resolve help that says whether the other side replied and to ask the evidence holders first.
  await expect(page.locator('.dispute-text').nth(1)).toHaveText('Sample reply from the vendor.\nNothing was stored.');
  await expect(page.locator('.dispute-text').first()).toHaveCSS('white-space', 'pre-wrap');
  await expect(page.locator('.dispute-evidence li')).toHaveText(['Buyer → sample_moderator (1, latest Sample)', 'Vendor → none']);
  expect(await page.evaluate(() => document.documentElement.scrollWidth)).toBeLessThanOrEqual(page.viewportSize()!.width);
  await page.goto('/disputes');
  await expect(page.locator('.order-unavailable')).toContainText('Only paid, shipped, or delivered orders qualify');
  expect(await page.evaluate(() => document.documentElement.scrollWidth)).toBeLessThanOrEqual(page.viewportSize()!.width);
});

test('preview rejects order actions', async ({ page }) => {
  await page.goto(`/order?id=${PREVIEW_DRAFT_ID}`);
  await page.getByRole('button', { name: 'Cancel draft' }).click();
  await expect(page.locator('body')).toContainText('Read-only preview');
});
