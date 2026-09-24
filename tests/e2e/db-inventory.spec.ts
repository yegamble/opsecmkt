import { expect, test } from '@playwright/test';
import { ADMIN, uniqueHandle } from './db-fixtures';
import { setRole, signedIn } from './db-helpers';

// P4 Inventory: the shared administrator publishes its own uniquely named listing, edits it,
// archives it (hidden from the catalog, closed to new drafts) and restores it, then edits a fresh
// vendor's listing without seeing its automatic delivery content.
test('edit, archive and restore a listing without client scripts', async ({ page, browser, baseURL }) => {
  const title = `Inventory ${uniqueHandle('item')}`;
  await page.goto('/login');
  await page.getByLabel('Handle', { exact: true }).fill(ADMIN.handle);
  await page.getByLabel('Password', { exact: true }).fill(ADMIN.password);
  await page.getByRole('button', { name: 'Sign in' }).click();
  await expect(page.locator('.account-name')).toHaveText(ADMIN.handle);

  await page.goto('/vendor-dashboard');
  await page.getByText('+ Publish a new listing', { exact: true }).click();
  await page.getByLabel('Listing title').fill(title);
  await page.getByLabel('Description', { exact: true }).fill('Inventory regression listing.');
  await page.getByLabel('Fulfillment type').selectOption('digital');
  await page.getByLabel('Bitcoin reference price').fill('0.001');
  await page.getByLabel('Monero reference price').fill('0.5');
  await expect(page.locator('main')).toContainText('Automatic delivery requires a payment provider');
  await page.getByRole('button', { name: 'Publish listing' }).click();
  await expect(page.locator('.listings-grid')).toContainText(title);

  await page.getByRole('link', { name: `Edit ${title}` }).click();
  await expect(page.getByRole('heading', { level: 1 })).toHaveText('Edit listing.');
  await expect(page.getByLabel('Listing title')).toHaveValue(title);
  await expect(page.locator('main')).toContainText('Stored unencrypted on the server');
  await page.getByLabel('Listing title').fill(`${title} v2`);
  await page.getByLabel('Stock').fill('3');
  await page.getByLabel('Automatic delivery content').fill('download-token-e2e');
  await page.getByRole('button', { name: 'Save changes' }).click();
  await expect(page.getByRole('status')).toHaveText('Changes saved.');
  await expect(page.getByLabel('Listing title')).toHaveValue(`${title} v2`);
  await expect(page.getByLabel('Stock')).toHaveValue('3');
  await expect(page.getByLabel('Automatic delivery content')).toHaveValue('download-token-e2e');

  await page.getByRole('button', { name: 'Archive listing' }).click();
  await expect(page).toHaveURL(/\/vendor-dashboard\?saved=1$/);
  const card = page.locator('.product-card', { hasText: `${title} v2` });
  await expect(card.locator('.badge')).toHaveText('Archived');

  await page.goto(`/?q=${encodeURIComponent(title)}`);
  await expect(page.getByRole('heading', { name: 'No listings found' })).toBeVisible();

  await page.goto('/vendor-dashboard');
  await page.getByRole('button', { name: `Restore ${title} v2` }).click();
  await expect(page).toHaveURL(/\/vendor-dashboard\?saved=1$/);
  await page.goto(`/?q=${encodeURIComponent(title)}`);
  await expect(page.locator('.listings-grid .product-card')).toHaveCount(1);
  await page.getByRole('link', { name: `${title} v2`, exact: true }).click();
  await expect(page.locator('main')).not.toContainText('download-token-e2e');
  await expect(page.getByRole('link', { name: 'Edit this listing' })).toBeVisible();

  // A-72: the administrator edits a fresh vendor's digital listing without seeing or replacing its
  // automatic delivery content (reuses this test's single administrator sign-in).
  const vendorHandle = uniqueHandle('invvendor');
  const vendorTitle = `Vendor ${uniqueHandle('item')}`;
  const secret = `vendor-secret-${uniqueHandle('e2e')}`;
  const vendor = await signedIn(browser, baseURL, vendorHandle, 'browser-inventory-vendor-123', true);
  await setRole(page, vendorHandle, 'vendor');
  await vendor.goto('/vendor-dashboard');
  await vendor.getByText('+ Publish a new listing', { exact: true }).click();
  await vendor.getByLabel('Listing title').fill(vendorTitle);
  await vendor.getByLabel('Description', { exact: true }).fill('Administrator delivery-content regression.');
  await vendor.getByLabel('Fulfillment type').selectOption('digital');
  await vendor.getByLabel('Bitcoin reference price').fill('0.001');
  await vendor.getByLabel('Monero reference price').fill('0.5');
  await vendor.getByLabel('Automatic delivery content').fill(secret);
  await vendor.getByRole('button', { name: 'Publish listing' }).click();
  await vendor.getByRole('link', { name: `Edit ${vendorTitle}` }).click();
  const editURL = vendor.url();

  await page.goto(editURL);
  await expect(page.locator('main')).toContainText('Automatic delivery content is set; only the vendor can view or change it.');
  await expect(page.locator('main')).not.toContainText(secret);
  await expect(page.getByLabel('Automatic delivery content')).toHaveCount(0);
  await page.getByLabel('Listing title').fill(`${vendorTitle} admin`);
  await page.getByRole('button', { name: 'Save changes' }).click();
  await expect(page.getByRole('status')).toHaveText('Changes saved.');
  await expect(page.getByLabel('Listing title')).toHaveValue(`${vendorTitle} admin`);

  await vendor.goto(editURL);
  await expect(vendor.getByLabel('Automatic delivery content')).toHaveValue(secret);
  await vendor.context().close();
});
