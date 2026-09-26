import { expect, test } from '@playwright/test';
import { PREVIEW_DRAFT_ID } from './preview-routes';

// P3 order views in the read-only preview (sample data, JavaScript disabled).
test('order page shows history, unavailable payment and the buyer actions', async ({ page }) => {
  await page.goto(`/order?id=${PREVIEW_DRAFT_ID}`);
  await expect(page.getByRole('heading', { name: 'History' })).toBeVisible();
  await expect(page.locator('.order-timeline li')).toHaveCount(1);
  await expect(page.locator('.order-unavailable')).toContainText('Payment unavailable for BTC');
  await expect(page.getByRole('button', { name: 'Request payment address' })).toHaveCount(0);
  await expect(page.getByRole('button', { name: 'Cancel draft' })).toBeVisible();
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
  await expect(reviews.locator('.badge')).toHaveText('Verified purchase');
  await expect(reviews).toContainText('Rated 4 out of 5');
  await expect(reviews).not.toContainText('preview_user');
});

test('moderator desk requires an outcome and disputes explain eligibility', async ({ page }) => {
  await page.goto('/moderator');
  await expect(page.getByRole('group', { name: 'Outcome' })).toBeVisible();
  // A-161: each outcome states the amount, recipient, difference from the price and the recipient's payout state
  // (sample figures); resolving needs a ticked confirmation and the decision field warns against identifying text.
  await expect(page.getByRole('radio', { name: 'Release 0.00412 BTC (test network) to ghost_circuit — exactly the price', exact: true })).toBeVisible();
  await expect(page.getByRole('radio', { name: 'Refund 0.00412 BTC (test network) to sample_buyer — exactly the price; no payout address: the payout waits for one', exact: true })).toBeVisible();
  await expect(page.getByRole('checkbox', { name: 'I checked the amount and who receives it for the outcome I chose. Resolving is final.' })).toHaveAttribute('required', '');
  await expect(page.locator('input[type="hidden"][name="amount_seen"]')).toHaveValue('412000');
  await expect(page.getByLabel('Decision', { exact: true })).toHaveAccessibleDescription(/^Stored unencrypted\..*Never include an address, real name or tracking number/);
  await expect(page.locator('main')).toContainText('Resolving queues one payout of the whole counted amount to one party; the payment watcher sends it automatically. There is no split.');
  await expect(page.locator('main')).not.toContainText('moves no funds');
  await page.goto('/disputes');
  await expect(page.locator('.order-unavailable')).toContainText('Only paid, shipped, or delivered orders qualify');
  expect(await page.evaluate(() => document.documentElement.scrollWidth)).toBeLessThanOrEqual(page.viewportSize()!.width);
});

test('preview rejects order actions', async ({ page }) => {
  await page.goto(`/order?id=${PREVIEW_DRAFT_ID}`);
  await page.getByRole('button', { name: 'Cancel draft' }).click();
  await expect(page.locator('body')).toContainText('Read-only preview');
});
