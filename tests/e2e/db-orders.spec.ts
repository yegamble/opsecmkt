import { expect, test } from '@playwright/test';
import { ADMIN, FIXTURE_LISTING, uniqueHandle } from './db-fixtures';

// P3 order lifecycle against a real database with no payment provider configured.
test('draft cannot be paid without a wallet, cancellation is recorded, vendor sees the order', async ({ page }) => {
  const handle = uniqueHandle('orders');
  await page.goto('/register');
  await page.getByLabel('Handle', { exact: true }).fill(handle);
  await page.getByLabel('Password', { exact: true }).fill('browser-orders-password-123');
  await page.getByRole('button', { name: 'Create account' }).click();
  await expect(page.locator('.account-name')).toHaveText(handle);

  await page.getByRole('link', { name: FIXTURE_LISTING, exact: true }).click();
  await page.getByRole('link', { name: 'Review order draft' }).click();
  await page.getByLabel('Reference currency').selectOption('BTC');
  await page.getByRole('button', { name: 'Create unfunded draft' }).click();
  await expect(page).toHaveURL(/\/order\?id=/);
  const orderURL = page.url();

  // No provider: the payment step is visibly unavailable and there is no button for it.
  await expect(page.locator('.order-unavailable')).toContainText('Payment unavailable for BTC');
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
  await expect(page).toHaveURL(/saved=1/);
  await expect(page.locator('.page-head .badge')).toHaveText('Cancelled');
  await expect(page.locator('.order-timeline')).toContainText('Draft — unfunded → Cancelled');
  await expect(page.locator('.order-timeline')).toContainText(`${handle} (buyer)`);
  await expect(page.locator('.order-timeline')).toContainText('Ordered the wrong currency');
  await expect(page.getByText('This order is closed.')).toBeVisible();
  await expect(page.getByRole('button', { name: 'Cancel draft' })).toHaveCount(0);

  // The listing owner sees the order among incoming orders.
  await page.goto('/account');
  await page.getByRole('button', { name: 'Sign out', exact: true }).click();
  await page.goto('/login');
  await page.getByLabel('Handle', { exact: true }).fill(ADMIN.handle);
  await page.getByLabel('Password', { exact: true }).fill(ADMIN.password);
  await page.getByRole('button', { name: 'Sign in' }).click();
  await expect(page.locator('.account-name')).toHaveText(ADMIN.handle);
  await page.goto('/vendor-dashboard');
  await expect(page.getByRole('heading', { name: 'Incoming orders' })).toBeVisible();
  await expect(page.locator('tr', { hasText: handle }).first()).toContainText('Cancelled');
  await page.goto(orderURL);
  await expect(page.locator('.eyebrow').first()).toContainText('you are the vendor');
  await expect(page.locator('.order-timeline')).toContainText('Draft — unfunded → Cancelled');
});
