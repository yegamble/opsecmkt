import { expect, test } from '@playwright/test';

// The preview server has no payment provider: every payment surface must say so and show no address.
test('preview pages state that payments are disabled', async ({ page }) => {
  for (const path of ['/', '/checkout?id=encrypted-drive', '/order?id=sample-draft', '/account', '/vendor-dashboard', '/admin']) {
    expect((await page.goto(path))?.status()).toBe(200);
    await expect(page.locator('footer')).toContainText('Payments disabled');
    await expect(page.locator('body')).not.toContainText('TESTNET payments');
    await expect(page.locator('.payment-address')).toHaveCount(0);
  }
  await page.goto('/order?id=sample-draft');
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
