import { expect, test, type Page } from '@playwright/test';
import { previewRoutes as routes } from './preview-routes';
import { waitForStyles } from './styles';

async function openFilters(page: Page) {
  const mobile = page.locator('.filter-details');
  await waitForStyles(page);
  if (await mobile.isVisible()) {
    await mobile.locator(':scope > summary').click();
    return mobile;
  }
  return page.locator('.desktop-filters');
}

for (const [path, heading] of routes) {
  test(`HTML-only responsive page ${path}`, async ({ page, baseURL }) => {
    const forbiddenRequests: string[] = [];
    page.on('request', request => {
      if (new URL(request.url()).origin !== new URL(baseURL!).origin || request.resourceType() === 'script') {
        forbiddenRequests.push(request.url());
      }
    });
    const response = await page.goto(path);
    expect(response?.status()).toBe(200);
    await expect(page.getByRole('heading', { level: 1 })).toHaveText(heading);
    await expect(page.locator('main')).toBeVisible();
    await expect(page.locator('script')).toHaveCount(0);
    expect(forbiddenRequests).toEqual([]);
    // Local scroll containers for tables/navigation are allowed; the page itself must fit.
    expect(await page.evaluate(() => document.documentElement.scrollWidth)).toBeLessThanOrEqual(page.viewportSize()!.width);
    const brand = page.getByRole('link', { name: 'OPSEC Market home' });
    await expect(brand).toHaveText('[OPSECMKT]');
    const brackets = brand.locator('.wordmark-bracket');
    await expect(brackets).toHaveCount(2);
    for (const bracket of await brackets.all()) await expect(bracket).toHaveCSS('color', 'rgb(245, 158, 11)');
    await expect(brand.locator('.wordmark-accent')).toHaveCSS('color', 'rgb(245, 158, 11)');
    const box = await brand.boundingBox();
    expect(box!.x).toBeGreaterThanOrEqual(0);
    expect(box!.x + box!.width).toBeLessThanOrEqual(page.viewportSize()!.width);
    await expect(page.locator('.preview-strip')).toContainText('READ-ONLY PREVIEW');
  });
}

test('keyboard skip link focuses main content with a visible focus indicator', async ({ page, browserName }) => {
  await page.goto('/');
  // macOS WebKit uses Option-Tab to include links in keyboard navigation.
  // Keep this a real keyboard traversal so missing/unreachable skip links fail.
  await page.keyboard.press(browserName === 'webkit' && process.platform === 'darwin' ? 'Alt+Tab' : 'Tab');
  const skip = page.getByRole('link', { name: 'Skip to main content' });
  await expect(skip).toBeFocused();
  await expect(skip).toBeInViewport();
  await expect(skip).not.toHaveCSS('outline-style', 'none');
  await page.keyboard.press('Enter');
  await expect(page.locator('main')).toBeFocused();
});

test('search, empty state, filter and reset work without client scripts', async ({ page }) => {
  await page.goto('/');
  const search = page.locator('form[role="search"]:visible');
  await search.getByRole('textbox').fill('no-matching-product-123');
  await search.getByRole('button').click();
  await expect(page.getByRole('heading', { name: 'No listings found' })).toBeVisible();
  await page.getByRole('link', { name: 'Clear filters' }).click();
  await expect(page.locator('.listings-grid .product-card')).toHaveCount(6);
  const filters = await openFilters(page);
  await filters.getByRole('combobox', { name: 'Category', exact: true }).selectOption('Hardware');
  await filters.getByLabel('Ships to / region').selectOption('United States');
  await filters.getByLabel('Display currency').selectOption('XMR');
  await filters.getByRole('button', { name: 'Apply filters' }).click();
  await expect(page.locator('.listings-grid .product-card')).toHaveCount(1);
  await expect(page.locator('.listings-grid')).toContainText('Faraday Laptop Bag');
  await expect(page.locator('.product-bottom .price')).toHaveText('0.103XMR');
  const resetFilters = await openFilters(page);
  await resetFilters.getByRole('link', { name: 'Reset filters' }).click();
  await expect(page.locator('.listings-grid .product-card')).toHaveCount(6);
  await waitForStyles(page);
  const newSearch = page.locator('form[role="search"]:visible');
  await newSearch.getByRole('textbox').fill('encrypted');
  await newSearch.getByRole('button').click();
  await expect(page.locator('.listings-grid .product-card')).toHaveCount(1);
  await expect(page.locator('.listings-grid')).toContainText('Encrypted USB Drive');
});

// A-181: one page of samples needs no pager; an out-of-range or malformed page still lands on the catalog,
// and the search term is capped at 64 characters (the box says so with maxlength and echoes the capped term).
test('catalog page numbers and long search terms are handled without client scripts', async ({ page }) => {
  for (const path of ['/?page=9', '/?page=-1', '/?page=abc']) {
    expect((await page.goto(path))?.status()).toBe(200);
    await expect(page.locator('.listings-grid .product-card')).toHaveCount(6);
    await expect(page.getByRole('navigation', { name: 'Catalog pages' })).toHaveCount(0);
  }
  const search = page.locator('form[role="search"]:visible');
  await expect(search.getByRole('textbox')).toHaveAttribute('maxlength', '64');
  await page.goto(`/?q=${'e'.repeat(70)}`);
  await expect(page.locator('form[role="search"]:visible').getByRole('textbox')).toHaveValue('e'.repeat(64));
  await page.goto('/?q=%25');
  await expect(page.getByRole('heading', { name: 'No listings found' })).toBeVisible();
});

test('product, vendor and checkout navigation preserves listing identity', async ({ page }) => {
  await page.goto('/');
  await page.getByRole('link', { name: 'Faraday Laptop Bag — 15 inch', exact: true }).click();
  await expect(page).toHaveURL(/\/product\?id=faraday-bag$/);
  await expect(page.getByRole('heading', { level: 1 })).toHaveText('Faraday Laptop Bag — 15 inch');
  await page.getByRole('link', { name: 'iron_cage', exact: true }).click();
  await expect(page.getByRole('heading', { level: 1 })).toHaveText('iron_cage');
  await expect(page.locator('.listings-grid .product-card')).toHaveCount(1);
  await page.getByRole('link', { name: 'Faraday Laptop Bag — 15 inch', exact: true }).click();
  await page.getByRole('link', { name: 'Review order draft' }).click();
  await expect(page).toHaveURL(/\/checkout\?id=faraday-bag$/);
  await expect(page.getByRole('heading', { name: 'Faraday Laptop Bag — 15 inch' })).toBeVisible();
  await expect(page.locator('input[name="product_id"]')).toHaveValue('faraday-bag');
  await page.getByRole('button', { name: 'Create unfunded draft' }).click();
  await expect(page.locator('body')).toContainText('Read-only preview');
});

test('navigation accessibility structure stays stable', async ({ page }) => {
  await page.goto('/');
  await expect(page.getByRole('navigation', { name: 'Main navigation' })).toMatchAriaSnapshot(`
    - navigation "Main navigation":
      - link "Marketplace"
      - link "Orders"
      - link "Messages"
      - link "Notifications (1 unread)"
      - link "Disputes"
      - link "Account"
      - link "Transparency"
  `);
});

// A-188: the avatar form on /account in the read-only preview (a vendor's Ready sample; ?scanner=off shows the
// state without CLAMD_ADDRESS). Markup and layout only: db-listing-photos.spec.ts uploads a real avatar.
test('avatar form on /account: separate, multipart, csrf first, limits and warning before the input', async ({ page }) => {
  await page.goto('/account');
  const panel = page.getByRole('region', { name: 'Avatar' });
  const form = panel.locator('form[action="/account/avatar"]');
  await expect(form).toHaveAttribute('method', 'post');
  await expect(form).toHaveAttribute('enctype', 'multipart/form-data');
  await expect(page.locator('form[enctype="multipart/form-data"]')).toHaveCount(1);
  expect(await page.locator('form[action="/account"]').getAttribute('enctype')).toBeNull();
  expect(await form.locator('input').evaluateAll(inputs => inputs.map(i => i.getAttribute('name')))).toEqual(['csrf', 'photo']);
  await expect(form.locator('input').first()).toHaveAttribute('type', 'hidden');
  const file = form.getByLabel('Avatar', { exact: true });
  await expect(file).toHaveAttribute('type', 'file');
  await expect(file).toHaveAttribute('accept', 'image/jpeg,image/png');
  expect(await file.getAttribute('capture')).toBeNull();
  await expect(file).toHaveAttribute('aria-describedby', 'avatar-help avatar-privacy');
  await expect(page.locator('#avatar-help')).toContainText('4096 pixels a side and 4096×3072 in all (about 12.6 megapixels)');
  await expect(page.locator('#avatar-help')).toContainText('Uploading can take up to 90 seconds over Tor');
  await expect(page.locator('#avatar-privacy')).toContainText('Screenshots can show your clock and timezone');
  await expect(page.locator('#avatar-privacy')).toContainText('public to anyone, including visitors who are not signed in, on your vendor page');
  for (const id of ['avatar-help', 'avatar-privacy']) {
    expect(await file.evaluate((input, before) => !!(document.getElementById(before)!.compareDocumentPosition(input) & Node.DOCUMENT_POSITION_FOLLOWING), id)).toBe(true);
  }
  await expect(panel).toContainText('No avatar.');
  // The sample account is not staff: no staff list choice.
  await expect(page.getByRole('region', { name: 'Public staff list' })).toHaveCount(0);
  await expect(page.locator('script')).toHaveCount(0);
  expect(await page.evaluate(() => document.documentElement.scrollWidth)).toBeLessThanOrEqual(page.viewportSize()!.width);
  await page.goto('/account?scanner=off');
  await expect(panel).toContainText('Photo uploads are off on this market');
  await expect(page.locator('input[type="file"]')).toHaveCount(0);
});
