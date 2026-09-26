import { expect, test } from '@playwright/test';
import { PREVIEW_DEPOSIT_TXID, PREVIEW_DRAFT_ID, PREVIEW_PAID_ID } from './preview-routes';

// The preview server has no payment provider: every payment surface must say so and show no address.
test('preview pages state that payments are disabled', async ({ page }) => {
  for (const path of ['/', '/checkout?id=encrypted-drive', `/order?id=${PREVIEW_DRAFT_ID}`, `/order?id=${PREVIEW_PAID_ID}`, '/account', '/vendor-dashboard', '/admin']) {
    expect((await page.goto(path))?.status()).toBe(200);
    await expect(page.locator('footer')).toContainText('Payments disabled');
    await expect(page.locator('body')).not.toContainText('TESTNET payments');
    await expect(page.locator('.payment-address')).toHaveCount(0);
  }
  await page.goto(`/order?id=${PREVIEW_DRAFT_ID}`);
  await expect(page.getByRole('heading', { name: 'Payment is unavailable' })).toBeVisible();
  await page.goto('/checkout?id=encrypted-drive');
  await expect(page.locator('main')).toContainText('UNFUNDED DRAFT ONLY.');
  await page.goto('/account');
  await expect(page.locator('main')).toContainText('Unavailable — payments disabled');
  await expect(page.getByLabel('Bitcoin payout address')).toHaveCount(0);
  await page.goto('/admin');
  const providers = page.locator('.admin-payments');
  await expect(providers.getByRole('heading', { name: 'Payment providers' })).toBeVisible();
  await expect(providers).toContainText('No provider is running.');
  await expect(providers).toContainText('No payouts recorded.');
  expect(await page.evaluate(() => document.documentElement.scrollWidth)).toBeLessThanOrEqual(page.viewportSize()!.width);
});

// A-115: the paid sample shows one sample deposit row with a 64-hex txid; the table scrolls inside its wrapper
// instead of widening the page, and the order's status says no wallet is connected.
test('preview paid sample shows one sample deposit inside a scrolling table', async ({ page }) => {
  await page.goto(`/order?id=${PREVIEW_PAID_ID}`);
  await expect(page.locator('.page-head p.mono')).toHaveText(PREVIEW_PAID_ID);
  await expect(page.locator('.page-head .badge')).toHaveText('Paid');
  const deposits = page.locator('.payment-deposits');
  await expect(deposits.locator('tbody tr')).toHaveCount(1);
  await expect(deposits.locator('td.mono')).toHaveText(`${PREVIEW_DEPOSIT_TXID}:0`);
  await expect(page.locator('.payment-figures')).toContainText('Preview sample: no wallet is connected and no funds exist');
  const width = page.viewportSize()!.width;
  expect(await page.evaluate(() => document.documentElement.scrollWidth)).toBeLessThanOrEqual(width);
  const box = (await deposits.boundingBox())!;
  expect(box.x + box.width).toBeLessThanOrEqual(width);
  // A-148: the txid wraps, so from tablet width up every column fits without scrolling; where the table does
  // scroll (phones), the scroller is a named region a keyboard user can focus and scroll.
  await expect(page.getByRole('region', { name: 'Deposits seen by the wallet' })).toHaveAttribute('tabindex', '0');
  expect(await deposits.locator('td.mono').evaluate(e => getComputedStyle(e).whiteSpace)).toBe('normal');
  if (width >= 768) {
    expect(await deposits.evaluate(e => e.scrollWidth - e.clientWidth), `deposit table overflow at ${width}px`).toBeLessThanOrEqual(0);
  }
});
