import { expect, test, type Browser, type Page } from '@playwright/test';
import { ADMIN, uniqueHandle } from './db-fixtures';

// Contacts and vendor demotion against a real database: an administrator promotes a new account to
// vendor, a buyer contacts it through a prefilled message form, the vendor sees an unread notification
// in the main navigation at phone width, and demoting the vendor archives its listing.

async function signedIn(browser: Browser, baseURL: string | undefined, handle: string, password: string, register: boolean): Promise<Page> {
  const context = await browser.newContext({ baseURL, javaScriptEnabled: false, viewport: { width: 320, height: 800 } });
  const page = await context.newPage();
  await page.goto(register ? '/register' : '/login');
  await page.getByLabel('Handle', { exact: true }).fill(handle);
  await page.getByLabel('Password', { exact: true }).fill(password);
  await page.getByRole('button', { name: register ? 'Create account' : 'Sign in' }).click();
  await expect(page.locator('.account-name')).toHaveText(handle);
  return page;
}

async function setRole(admin: Page, handle: string, role: string) {
  await admin.goto('/admin');
  const form = admin.locator('form', { has: admin.getByRole('button', { name: 'Update role' }) });
  const option = form.locator('option', { hasText: `${handle} ·` });
  await form.getByLabel('Account').selectOption(await option.getAttribute('value'));
  await form.getByLabel('Role').selectOption(role);
  await form.getByRole('button', { name: 'Update role' }).click();
  await expect(admin).toHaveURL(/\/admin\?saved=1$/);
}

test('vendor contact, unread notifications and demotion archive', async ({ browser, baseURL }) => {
  const vendorHandle = uniqueHandle('contactv');
  const buyerHandle = uniqueHandle('contactb');
  const title = `Contacts ${uniqueHandle('item')}`;
  const vendor = await signedIn(browser, baseURL, vendorHandle, 'browser-contact-vendor-123', true);
  const buyer = await signedIn(browser, baseURL, buyerHandle, 'browser-contact-buyer-123', true);
  const admin = await signedIn(browser, baseURL, ADMIN.handle, ADMIN.password, false);

  // Notifications are reachable at 320px with no unread count yet.
  const vendorNav = vendor.getByRole('navigation', { name: 'Main navigation' });
  await expect(vendorNav.getByRole('link', { name: 'Notifications', exact: true })).toBeVisible();

  await setRole(admin, vendorHandle, 'vendor');
  await vendor.goto('/vendor-dashboard');
  await vendor.getByText('+ Publish a new listing', { exact: true }).click();
  await vendor.getByLabel('Listing title').fill(title);
  await vendor.getByLabel('Description', { exact: true }).fill('Contacts regression listing.');
  await vendor.getByLabel('Bitcoin reference price').fill('0.001');
  await vendor.getByLabel('Monero reference price').fill('0.5');
  await vendor.getByRole('button', { name: 'Publish listing' }).click();
  await expect(vendor.locator('.listings-grid')).toContainText(title);

  // The buyer contacts the vendor: the recipient is prefilled and the missing key is stated plainly.
  await buyer.goto(`/?q=${encodeURIComponent(title)}`);
  await buyer.getByRole('link', { name: title, exact: true }).click();
  const productURL = buyer.url();
  await buyer.getByRole('link', { name: vendorHandle, exact: true }).click();
  await expect(buyer.getByRole('region', { name: 'PGP public key' })).toContainText(`${vendorHandle} has not added a PGP public key`);
  await buyer.getByRole('link', { name: 'Send encrypted message' }).click();
  await expect(buyer).toHaveURL(new RegExp(`/messages\\?to=${vendorHandle}$`));
  await expect(buyer.getByLabel('Recipient handle')).toHaveValue(vendorHandle);
  await expect(buyer.locator('main')).toContainText('You cannot send them an encrypted message until they add one.');

  // A cancelled draft notifies the vendor, who sees the count in the navigation at 320px.
  await buyer.goto(productURL);
  await buyer.getByRole('link', { name: 'Review order draft' }).click();
  await buyer.getByLabel('Reference currency').selectOption('BTC');
  await buyer.getByRole('button', { name: 'Create unfunded draft' }).click();
  await expect(buyer.getByRole('region', { name: 'Vendor’s PGP key' })).toContainText(`${vendorHandle} has not added a PGP public key`);
  await expect(buyer.getByRole('link', { name: 'Message vendor' })).toHaveAttribute('href', `/messages?to=${vendorHandle}`);
  await buyer.getByRole('button', { name: 'Cancel draft' }).click();
  await expect(buyer.locator('.page-head .badge')).toHaveText('Cancelled');

  await vendor.goto('/');
  const unread = vendorNav.getByRole('link', { name: 'Notifications (1 unread)' });
  await expect(unread).toBeVisible();
  expect(await vendor.evaluate(() => document.documentElement.scrollWidth)).toBeLessThanOrEqual(320);
  await unread.click();
  await vendor.getByRole('button', { name: 'Mark as read' }).first().click();
  await expect(vendorNav.getByRole('link', { name: 'Notifications', exact: true })).toBeVisible();

  // Demoting the vendor archives the listing: it leaves the catalog and the product page is gone.
  await setRole(admin, vendorHandle, 'buyer');
  await expect(admin.locator('main')).toContainText('Changed user role to buyer; archived 1 active listing(s)');
  await buyer.goto(`/?q=${encodeURIComponent(title)}`);
  await expect(buyer.getByRole('heading', { name: 'No listings found' })).toBeVisible();
  expect((await buyer.goto(productURL))?.status()).toBe(404);

  for (const p of [vendor, buyer, admin]) await p.context().close();
});
