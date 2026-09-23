import { expect, test } from '@playwright/test';
import { ADMIN, uniqueHandle } from './db-fixtures';
import { setRole, signedIn } from './db-helpers';

// All seed records below are created through public forms against PostgreSQL.
// No wallet is configured here; funded journeys live in wallet-journey.spec.ts.
test('seeded catalog discovery, role boundaries and persistent unfunded orders', async ({ browser, baseURL }) => {
  test.setTimeout(90_000);
  const seed = uniqueHandle('seed');
  const password = 'seeded-browser-password-123';
  const vendorHandle = uniqueHandle('seedvendor');
  const buyerHandle = uniqueHandle('seedbuyer');
  const moderatorHandle = uniqueHandle('seedmod');
  const vendor = await signedIn(browser, baseURL, vendorHandle, password, true);
  const buyer = await signedIn(browser, baseURL, buyerHandle, password, true);
  const moderator = await signedIn(browser, baseURL, moderatorHandle, password, true);
  const admin = await signedIn(browser, baseURL, ADMIN.handle, ADMIN.password, false);
  const visitorContext = await browser.newContext({ baseURL, javaScriptEnabled: false, viewport: { width: 390, height: 844 } });
  const visitor = await visitorContext.newPage();
  try {
    await setRole(admin, vendorHandle, 'vendor');
    await setRole(admin, moderatorHandle, 'moderator');
    const publish = async (suffix: string, kind: string, region: string, stock: string) => {
      const title = `${seed} ${suffix}`;
      await vendor.goto('/vendor-dashboard');
      await vendor.getByText('+ Publish a new listing', { exact: true }).click();
      await vendor.getByLabel('Listing title').fill(title);
      await vendor.getByLabel('Description', { exact: true }).fill(`Seeded discovery fixture ${seed}: ${suffix}.`);
      await vendor.getByRole('combobox', { name: 'Category', exact: true }).selectOption(kind === 'digital' ? 'Digital' : 'Hardware');
      await vendor.getByRole('combobox', { name: 'Region', exact: true }).selectOption(region);
      await vendor.getByLabel('Fulfillment type').selectOption(kind);
      await vendor.getByLabel('Stock', { exact: true }).fill(stock);
      await vendor.getByLabel('Bitcoin reference price').fill('0.002');
      await vendor.getByLabel('Monero reference price').fill('0.75');
      await vendor.getByRole('button', { name: 'Publish listing' }).click();
      await expect(vendor.locator('.listings-grid')).toContainText(title);
      return title;
    };
    const hardware = await publish('European hardware', 'physical', 'Europe', '5');
    const digital = await publish('Worldwide handbook', 'digital', 'Worldwide', '8');
    const soldOut = await publish('Sold out hardware', 'physical', 'United States', '0');
    const archived = await publish('Archived handbook', 'digital', 'Worldwide', '3');
    await vendor.getByRole('button', { name: `Archive ${archived}`, exact: true }).click();

    // Search and filters are actual GET forms at phone width with scripts disabled.
    await visitor.goto('/');
    const search = visitor.locator('form.mobile-search');
    await search.getByLabel('Search listings').fill(seed);
    await search.getByRole('button', { name: 'Search', exact: true }).click();
    await expect(visitor.locator('.listings-grid .product-card')).toHaveCount(3);
    await expect(visitor.locator('.listings-grid')).not.toContainText(archived);
    await visitor.getByText('Browse & filter listings', { exact: true }).click();
    const filters = visitor.locator('.filter-details form');
    await filters.getByRole('combobox', { name: 'Category', exact: true }).selectOption('Digital');
    await filters.getByLabel('Ships to / region').selectOption('Europe');
    await filters.getByLabel('Display currency').selectOption('XMR');
    await filters.getByRole('button', { name: 'Apply filters' }).click();
    await expect(visitor.locator('.listings-grid .product-card', { hasText: digital })).toHaveCount(1);
    const handbook = visitor.locator('.product-card', { hasText: digital });
    await expect(handbook.locator('.price')).toHaveText('0.75XMR');
    await expect(visitor.locator('.listings-grid')).not.toContainText(hardware);
    await handbook.getByRole('link', { name: digital, exact: true }).click();
    await visitor.getByRole('link', { name: 'Review order draft' }).click();
    await expect(visitor).toHaveURL(/\/login$/);

    await buyer.goto(`/?q=${encodeURIComponent(soldOut)}`);
    await buyer.getByRole('link', { name: soldOut, exact: true }).click();
    await expect(buyer.getByText('Out of stock.', { exact: true })).toBeVisible();
    await expect(buyer.getByRole('link', { name: 'Review order draft' })).toHaveCount(0);

    await buyer.goto(`/?q=${encodeURIComponent(hardware)}`);
    await buyer.getByRole('link', { name: hardware, exact: true }).click();
    await buyer.getByRole('link', { name: vendorHandle, exact: true }).click();
    await expect(buyer.locator('.listings-grid')).toContainText(digital);
    await expect(buyer.locator('.listings-grid')).not.toContainText(archived);
    await buyer.getByRole('link', { name: hardware, exact: true }).click();
    await buyer.getByRole('link', { name: 'Review order draft' }).click();
    const checkoutURL = buyer.url();
    await buyer.getByRole('button', { name: 'Create unfunded draft' }).click();
    const orderURL = buyer.url();
    await buyer.goto(checkoutURL);
    await buyer.getByRole('button', { name: 'Create unfunded draft' }).click();
    expect(buyer.url()).toBe(orderURL);
    await buyer.reload();
    await expect(buyer.locator('.page-head .badge')).toHaveText('Draft — unfunded');
    for (const path of ['/admin', '/moderator', '/vendor-dashboard']) expect((await buyer.goto(path))?.status()).toBe(403);
    expect((await moderator.goto('/admin'))?.status()).toBe(403);
    expect((await moderator.goto('/vendor-dashboard'))?.status()).toBe(403);
    expect((await moderator.goto(orderURL))?.status()).toBe(404);
    await moderator.goto('/moderator');
    await expect(moderator.getByRole('heading', { level: 1 })).toContainText('Moderation desk');
    await vendor.goto(orderURL);
    await expect(vendor.locator('.eyebrow').first()).toContainText('you are the vendor');
    await expect(vendor.getByRole('button', { name: 'Cancel draft' })).toHaveCount(0);
    await buyer.goto(orderURL);
    await buyer.getByRole('button', { name: 'Cancel draft' }).click();
    await vendor.reload();
    await expect(vendor.locator('.page-head .badge')).toHaveText('Cancelled');
  } finally {
    await Promise.allSettled([vendor.context(), buyer.context(), moderator.context(), admin.context(), visitorContext].map(context => context.close()));
  }
});
