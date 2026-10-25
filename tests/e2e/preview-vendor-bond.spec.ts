import { expect, test } from '@playwright/test';
import { withinViewport } from './db-helpers';

test('bond controls and custody explanation remain readable without scripts', async ({ page }) => {
  await page.goto('/admin');
  const admin = page.getByRole('region', { name: 'Vendor bond requirement' });
  await expect(admin.getByLabel('Bond reference currency').locator('option')).toHaveText(['USD', 'GBP', 'EUR', 'BTC', 'XMR']);
  await expect(admin).toContainText('fixed five-minute schedule');
  await withinViewport(admin, page.viewportSize()!.width);
  await page.goto('/vendor-dashboard');
  await expect(page.getByRole('region', { name: 'Required vendor bond' })).toContainText('No vendor bond is currently required');
  expect(await page.locator('html').evaluate(e => e.scrollWidth)).toBeLessThanOrEqual(page.viewportSize()!.width);
});
