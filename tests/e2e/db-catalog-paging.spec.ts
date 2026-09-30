import { expect, test } from '@playwright/test';
import { ADMIN, uniqueHandle } from './db-fixtures';
import { setRole, signedIn } from './db-helpers';
import { waitForStyles } from './styles';

// A-181 / A-16: the catalog shows 25 listings a page. Paging is plain links that keep the search, filters and
// display currency, works at 320 px with scripts disabled, and marks the current page with aria-current.
// Listings are published through the vendor form; the search term scopes the catalog to this test's own.
test('catalog pages through search results with plain links at 320 px', async ({ browser, baseURL }) => {
  test.setTimeout(240_000);
  const seed = uniqueHandle('pager');
  const password = 'paging-browser-password-123';
  const vendorHandle = uniqueHandle('pagevendor');
  const vendor = await signedIn(browser, baseURL, vendorHandle, password, true);
  const admin = await signedIn(browser, baseURL, ADMIN.handle, ADMIN.password, false);
  const visitorContext = await browser.newContext({ baseURL, javaScriptEnabled: false, viewport: { width: 320, height: 700 } });
  const visitor = await visitorContext.newPage();
  try {
    await setRole(admin, vendorHandle, 'vendor');
    const long = `${'A long description for paging. '.repeat(20)}END-OF-DESCRIPTION`;
    for (let i = 1; i <= 26; i++) {
      await vendor.goto('/vendor-dashboard');
      await vendor.getByText('+ Publish a new listing', { exact: true }).click();
      await vendor.getByLabel('Listing title').fill(`${seed} item ${String(i).padStart(2, '0')}`);
      await vendor.getByLabel('Description', { exact: true }).fill(long);
      await vendor.getByLabel('Fulfillment type').selectOption('digital');
      await vendor.getByLabel('Bitcoin reference price').fill('0.001');
      await vendor.getByLabel('Monero reference price').fill('0.5');
      await vendor.getByRole('button', { name: 'Publish listing' }).click();
      // A-188: publishing lands on the new listing's edit page.
      await expect(vendor.getByLabel('Listing title')).toHaveValue(`${seed} item ${String(i).padStart(2, '0')}`);
    }

    await visitor.goto('/');
    const search = visitor.locator('form.mobile-search');
    await search.getByLabel('Search listings').fill(seed);
    await search.getByRole('button', { name: 'Search', exact: true }).click();
    await visitor.getByText('Browse & filter listings', { exact: true }).click();
    const filters = visitor.locator('.filter-details form');
    await filters.getByLabel('Display currency').selectOption('XMR');
    await filters.getByRole('button', { name: 'Apply filters' }).click();

    const pager = visitor.getByRole('navigation', { name: 'Catalog pages' });
    await expect(visitor.locator('.catalog-tools')).toContainText('26 listings');
    await expect(visitor.locator('.listings-grid .product-card')).toHaveCount(25);
    await expect(visitor.locator('.product-card').first()).toContainText(`${seed} item 26`);
    await expect(visitor.locator('.product-description').first()).not.toContainText('END-OF-DESCRIPTION');
    await expect(pager.locator('[aria-current="page"]')).toHaveText('Page 1 of 2');
    await expect(pager.getByRole('link', { name: /Previous/ })).toHaveCount(0);
    await waitForStyles(visitor);
    expect(await visitor.evaluate(() => document.documentElement.scrollWidth)).toBeLessThanOrEqual(320);

    const next = pager.getByRole('link', { name: 'Next page, page 2 of 2' });
    await next.focus();
    await expect(next).toBeFocused();
    await expect(next).not.toHaveCSS('outline-style', 'none');
    await next.click();
    const url = new URL(visitor.url());
    expect(url.searchParams.get('q')).toBe(seed);
    expect(url.searchParams.get('currency')).toBe('XMR');
    expect(url.searchParams.get('page')).toBe('2');
    await expect(visitor.locator('.listings-grid .product-card')).toHaveCount(1);
    await expect(visitor.locator('.listings-grid')).toContainText(`${seed} item 01`);
    await expect(visitor.locator('.product-card .price')).toHaveText('0.5XMR');
    await expect(pager.locator('[aria-current="page"]')).toHaveText('Page 2 of 2');
    await expect(pager.getByRole('link', { name: /Next/ })).toHaveCount(0);

    // The product page still shows the full description.
    await visitor.locator('.product-card h2 a').click();
    await expect(visitor.locator('main')).toContainText('END-OF-DESCRIPTION');
    await visitor.goBack();

    await pager.getByRole('link', { name: 'Previous page, page 1 of 2' }).click();
    await expect(visitor.locator('.listings-grid .product-card')).toHaveCount(25);
    expect(new URL(visitor.url()).searchParams.get('q')).toBe(seed);

    // Past the end: the last page, not an error or an empty grid.
    await visitor.goto(`/?q=${encodeURIComponent(seed)}&page=50`);
    await expect(pager.locator('[aria-current="page"]')).toHaveText('Page 2 of 2');
    await expect(visitor.locator('.listings-grid .product-card')).toHaveCount(1);
  } finally {
    for (const p of [vendor, admin, visitor]) await p.context().close();
  }
});
