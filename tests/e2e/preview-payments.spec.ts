import { expect, test } from '@playwright/test';
import { PREVIEW_DRAFT_ID, PREVIEW_PAID_ID } from './preview-routes';

// The preview server has no payment provider: every payment surface must say so and show no address.
test('preview pages state that payments are disabled', async ({ page }) => {
  for (const path of ['/', '/checkout?id=encrypted-drive', `/order?id=${PREVIEW_DRAFT_ID}`, `/order?id=${PREVIEW_PAID_ID}`, '/account', '/vendor-dashboard', '/admin']) {
    expect((await page.goto(path))?.status()).toBe(200);
    await expect(page.locator('footer')).toContainText('New payments paused');
    await expect(page.locator('body')).not.toContainText('TESTNET payments');
    await expect(page.locator('.payment-address')).toHaveCount(0);
  }
  await page.goto(`/order?id=${PREVIEW_DRAFT_ID}`);
  await expect(page.locator('.key-values')).toContainText('Draft — unfunded');
  await expect(page.locator('form[action="/orders/pay"]')).toHaveCount(0);
  await page.goto('/checkout?id=encrypted-drive');
  await expect(page.locator('main')).toContainText('A draft reserves no stock and requests no payment.');
  await page.goto('/account');
  // A-508: the saved pooled payout addresses are no longer read or shown.
  await expect(page.locator('main')).not.toContainText('payout address');
  await expect(page.getByLabel('Bitcoin payout address')).toHaveCount(0);
  await page.goto('/admin');
  const providers = page.locator('.admin-payments');
  await expect(providers.getByRole('heading', { name: 'Escrow status and payment intake' })).toBeVisible();
  // A-492: the pooled payout table is removed with pooled payments.
  await expect(providers).not.toContainText('Historical pooled');
  await expect(page.locator('#payouts')).toHaveCount(0);
  expect(await page.evaluate(() => document.documentElement.scrollWidth)).toBeLessThanOrEqual(page.viewportSize()!.width);
});

// A-492: the paid sample has no pooled payment record (its deposit table and copy were removed with pooled payments).
test('preview paid sample shows no pooled payment record', async ({ page }) => {
  await page.goto(`/order?id=${PREVIEW_PAID_ID}`);
  await expect(page.locator('.page-head p.mono')).toHaveText(PREVIEW_PAID_ID);
  await expect(page.locator('.page-head .badge')).toHaveText('Paid');
  await expect(page.locator('.payment-deposits')).toHaveCount(0);
  await expect(page.locator('main')).not.toContainText('retired pooled payment system');
  await expect(page.getByRole('heading', { name: 'No payment record' })).toBeVisible();
  expect(await page.evaluate(() => document.documentElement.scrollWidth)).toBeLessThanOrEqual(page.viewportSize()!.width);
});
