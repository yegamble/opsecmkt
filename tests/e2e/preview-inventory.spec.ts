import { expect, test } from '@playwright/test';

// P4 Inventory preview surfaces: the listing editor and archived listings on the vendor desk.
for (const path of ['/listing-edit?id=encrypted-drive', '/vendor-dashboard']) {
  test(`inventory preview ${path} is HTML-only and fits the viewport`, async ({ page }) => {
    const scripts: string[] = [];
    page.on('request', request => { if (request.resourceType() === 'script') scripts.push(request.url()); });
    const response = await page.goto(path);
    expect(response?.status()).toBe(200);
    await expect(page.locator('script')).toHaveCount(0);
    expect(scripts).toEqual([]);
    expect(await page.evaluate(() => document.documentElement.scrollWidth)).toBeLessThanOrEqual(page.viewportSize()!.width);
    await expect(page.locator('main')).toContainText('Automatic delivery requires a payment provider');
    await expect(page.locator('main')).toContainText('Stored unencrypted on the server');
  });
}

test('listing editor is prefilled and states its limits', async ({ page }) => {
  await page.goto('/listing-edit?id=encrypted-drive');
  await expect(page.getByRole('heading', { level: 1 })).toHaveText('Edit listing.');
  await expect(page.getByLabel('Listing title')).toHaveValue('Encrypted USB Drive — 256GB');
  await expect(page.getByLabel('Fulfillment type')).toHaveValue('physical');
  await expect(page.getByLabel('Bitcoin reference price')).toHaveValue('0.00412');
  await expect(page.locator('main')).toContainText('1 open order(s) depend on this listing');
  await expect(page.getByRole('button', { name: 'Archive listing' })).toBeVisible();
});

test('vendor desk shows archived listings with a badge and restore control', async ({ page }) => {
  await page.goto('/vendor-dashboard');
  const archived = page.locator('.product-card.is-archived');
  await expect(archived).toHaveCount(1);
  await expect(archived.locator('.badge')).toHaveText('Archived');
  await expect(archived.getByRole('button', { name: /^Restore / })).toBeVisible();
  await expect(page.locator('.stats')).toContainText('1 archived');
  await expect(page.getByLabel('Fulfillment type')).not.toContainText('Service');
});
