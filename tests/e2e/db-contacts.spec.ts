import { expect, test, type Browser, type Page } from '@playwright/test';
import { ADMIN, uniqueHandle } from './db-fixtures';
import { setRole } from './db-helpers';

// Contacts and vendor demotion against a real database: an administrator promotes a new account to
// vendor, a buyer contacts it through a prefilled message form, the vendor sees an unread notification
// in the main navigation at phone width, and demoting the vendor archives its listing.

async function fits(page: Page, label: string) {
  expect(await page.evaluate(() => document.documentElement.scrollWidth), label).toBeLessThanOrEqual(page.viewportSize()!.width);
}

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

test('vendor contact, unread notifications and demotion archive', async ({ browser, baseURL }) => {
  // A-115: the longest allowed handle (32 characters) in the header and the vendor page's monospace h1.
  const vendorHandle = uniqueHandle('contactv').padEnd(32, 'x');
  expect(vendorHandle).toHaveLength(32);
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
  await buyer.waitForURL(/\/product\?id=/);
  const productURL = buyer.url();
  await buyer.getByRole('link', { name: vendorHandle, exact: true }).click();
  await expect(buyer.getByRole('region', { name: 'PGP public key' })).toContainText(`${vendorHandle} has not added a PGP public key`);
  const vendorURL = buyer.url();
  for (const [page, url] of [[buyer, vendorURL], [vendor, vendorURL], [vendor, '/vendor-dashboard']] as const) {
    await page.goto(url);
    await fits(page, `${url} at 100% text`);
    await page.locator('html').evaluate(element => { element.style.fontSize = '200%'; });
    await fits(page, `${url} at 200% text`);
  }
  await buyer.goto(vendorURL);
  await buyer.getByRole('link', { name: 'Send encrypted message' }).click();
  await expect(buyer).toHaveURL(new RegExp(`/messages\\?to=${vendorHandle}$`));
  await expect(buyer.getByLabel('Recipient handle')).toHaveValue(vendorHandle);
  await expect(buyer.locator('main')).toContainText('Ask them to add a public key, or obtain and confirm their key through a trusted channel before encrypting.');

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
  await expect(admin.locator('main')).toContainText(`Changed role of ${vendorHandle} from vendor to buyer; archived 1 active listing(s)`);
  await buyer.goto(`/?q=${encodeURIComponent(title)}`);
  await expect(buyer.getByRole('heading', { name: 'No listings found' })).toBeVisible();
  expect((await buyer.goto(productURL))?.status()).toBe(404);

  for (const p of [vendor, buyer, admin]) await p.context().close();
});
