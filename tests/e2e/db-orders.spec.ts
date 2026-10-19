import { expect, test } from '@playwright/test';
import { ADMIN, FIXTURE_LISTING, uniqueHandle } from './db-fixtures';
import { submitAndLoad } from './db-helpers';
import { waitForStyles } from './styles';

// P3 order lifecycle against a real database with no payment provider configured.
test('draft cannot be paid without a wallet, cancellation is recorded, vendor sees the order', async ({ page }) => {
  const handle = uniqueHandle('orders');
  await page.goto('/register');
  await page.getByLabel('Handle', { exact: true }).fill(handle);
  await page.getByLabel('Password', { exact: true }).fill('browser-orders-password-123');
  await page.getByRole('button', { name: 'Create account' }).click();
  await expect(page.locator('.account-name')).toHaveText(handle);

  // The catalog shows 25 listings a page (A-181) and other specs publish more: search for the setup fixture.
  await page.goto(`/?q=${encodeURIComponent(FIXTURE_LISTING)}`);
  await page.getByRole('link', { name: FIXTURE_LISTING, exact: true }).click();
  await page.getByRole('link', { name: 'Review order draft' }).click();
  await page.getByLabel('Pay with').selectOption('XMR');
  await page.getByRole('button', { name: 'Create unfunded draft' }).click();
  await expect(page).toHaveURL(/\/order\?id=/);
  const orderURL = page.url();
  // A-115: the 64-hex order ID in the page head and breadcrumb wraps instead of widening the page.
  await waitForStyles(page);
  const viewport = page.viewportSize()!;
  for (const width of [390, 320]) {
    await page.setViewportSize({ width, height: viewport.height });
    expect(await page.evaluate(() => document.documentElement.scrollWidth), `draft at ${width}px`).toBeLessThanOrEqual(width);
  }
  await page.setViewportSize(viewport);

  // Intake is explicitly paused: the draft is unfunded and has no retired pooled payment step.
  await expect(page.getByRole('heading', { name: 'Unfunded draft' })).toBeVisible();
  await expect(page.locator('form[action="/orders/pay"]')).toHaveCount(0);
  await expect(page.getByRole('button', { name: 'Request payment address' })).toHaveCount(0);
  await expect(page.getByRole('heading', { name: 'History' })).toBeVisible();
  // A-82: times are UTC and say so, whatever the database server's zone.
  const created = page.locator('.key-values > div', { has: page.getByText('Created', { exact: true }) }).locator('dd');
  await expect(created).toHaveText(/^\d{4}-\d{2}-\d{2} \d{2}:\d{2} UTC$/);
  await expect(page.locator('.order-timeline .order-when').first()).toHaveText(/ UTC$/);

  // Disputes need a paid, shipped or delivered order.
  await page.goto('/disputes');
  await expect(page.locator('.order-unavailable')).toContainText('None of your orders can be disputed');

  await page.goto('/orders');
  const row = page.locator('tr', { hasText: FIXTURE_LISTING }).first();
  await expect(row).toContainText('Buying');

  await page.goto(orderURL);
  await page.getByLabel('Reason (optional)').fill('Ordered the wrong currency');
  await page.getByRole('button', { name: 'Cancel draft' }).click();
  await expect(page.getByRole('status')).toHaveText('Order cancelled.');
  await expect(page).toHaveTitle(/^Order cancelled\. – Order [0-9a-f]{8} – /);
  await expect(page.locator('.page-head .badge')).toHaveText('Cancelled');
  await expect(page.locator('.order-timeline')).toContainText('Draft — unfunded → Cancelled');
  await expect(page.locator('.order-timeline')).toContainText(`${handle} (buyer)`);
  await expect(page.locator('.order-timeline')).toContainText('Ordered the wrong currency');
  await expect(page.getByText('This order is closed.')).toBeVisible();
  await expect(page.getByRole('button', { name: 'Cancel draft' })).toHaveCount(0);

  // The listing owner can still read the cancelled draft, which stays off incoming work.
  await page.goto('/account');
  await submitAndLoad(page, page.getByRole('main').getByRole('button', { name: 'Sign out', exact: true }));
  await page.goto('/login');
  await page.getByLabel('Handle', { exact: true }).fill(ADMIN.handle);
  await page.getByLabel('Password', { exact: true }).fill(ADMIN.password);
  await page.getByRole('button', { name: 'Sign in' }).click();
  await expect(page.locator('.account-name')).toHaveText(ADMIN.handle);
  await page.goto('/vendor-dashboard');
  await expect(page.getByRole('heading', { name: 'Incoming orders' })).toBeVisible();
  await expect(page.locator('main')).toContainText('Unfunded drafts and cancelled drafts are kept in your order history but do not appear as incoming work.');
  await expect(page.locator('tr', { hasText: handle })).toHaveCount(0);
  // The cancelled draft remains directly readable by its vendor despite staying off the work queue.
  await page.goto(orderURL);
  await expect(page.locator('.eyebrow').first()).toContainText('you are the vendor');
  await expect(page.locator('.order-timeline')).toContainText('Draft — unfunded → Cancelled');
});
