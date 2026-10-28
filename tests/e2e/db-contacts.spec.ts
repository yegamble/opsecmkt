import { execFileSync } from 'node:child_process';
import { expect, test, type Browser, type Page } from '@playwright/test';
import { ADMIN, uniqueHandle } from './db-fixtures';
import { passwordSignIn, RECIPIENT_FINGERPRINT, saveEncryptionKey, setRole, seedPGPAccess } from './db-helpers';
import { waitForStyles } from './styles';

// Contacts and vendor demotion against a real database: an administrator promotes a new account to
// vendor, a buyer contacts it through a prefilled message form, a real role change appears as an unread
// notification in the main navigation at phone width, and demoting the vendor archives its listing.

async function fits(page: Page, label: string) {
  await waitForStyles(page);
  expect(await page.evaluate(() => document.documentElement.scrollWidth), label).toBeLessThanOrEqual(page.viewportSize()!.width);
}

async function signedIn(browser: Browser, baseURL: string | undefined, handle: string, password: string, register: boolean): Promise<Page> {
  const context = await browser.newContext({ baseURL, javaScriptEnabled: false, viewport: { width: 320, height: 800 } });
  const page = await context.newPage();
  if (!register) {
    await passwordSignIn(page, handle, password); // lifts a staff account's A-706 fixture factor for the password step
    return page;
  }
  await page.goto('/register');
  await page.getByLabel('Handle', { exact: true }).fill(handle);
  await page.getByLabel('Password', { exact: true }).fill(password);
  await page.getByRole('button', { name: 'Create account' }).click();
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

  await seedPGPAccess(buyer);
  await setRole(admin, vendorHandle, 'vendor');
  await vendor.goto('/notifications');
  await expect(vendor.locator('main')).toContainText('Your account role changed from buyer to vendor.');
  await vendor.getByRole('button', { name: 'Mark all as read', exact: true }).click();
  await expect(vendorNav.getByRole('link', { name: 'Notifications', exact: true })).toBeVisible();
  await vendor.goto('/vendor-dashboard');
  await vendor.getByText('+ Publish a new listing', { exact: true }).click();
  await vendor.getByLabel('Listing title').fill(title);
  await vendor.getByLabel('Description', { exact: true }).fill('Contacts regression listing.');
  await vendor.getByLabel('Price (XMR)').fill('0.5');
  await vendor.getByRole('button', { name: 'Publish listing' }).click();
  await expect(vendor.getByLabel('Listing title')).toHaveValue(title); // A-188: publishing lands on its edit page

  // The buyer contacts the vendor: the recipient is prefilled and the required public key is shown.
  await buyer.goto(`/?q=${encodeURIComponent(title)}`);
  await buyer.getByRole('link', { name: title, exact: true }).click();
  await buyer.waitForURL(/\/product\?id=/);
  const productURL = buyer.url();
  await buyer.getByRole('link', { name: vendorHandle, exact: true }).click();
  await expect(buyer.getByRole('region', { name: 'PGP public key' })).toContainText(RECIPIENT_FINGERPRINT);
  const vendorURL = buyer.url();
  for (const [page, url] of [[buyer, vendorURL], [vendor, vendorURL], [vendor, '/vendor-dashboard']] as const) {
    await page.goto(url);
    await fits(page, `${url} at 100% text`);
    await page.locator('html').evaluate(element => { element.style.fontSize = '200%'; });
    await fits(page, `${url} at 200% text`);
  }
  await buyer.goto(vendorURL);
  await buyer.getByRole('link', { name: 'Message vendor' }).click();
  await expect(buyer).toHaveURL(new RegExp(`/messages\\?to=${vendorHandle}$`));
  await expect(buyer.locator('input[name="recipient"]')).toHaveValue(vendorHandle);
  await expect(buyer.locator('.message-to')).toHaveText(`To: ${vendorHandle}`);
  await expect(buyer.locator('.contact-key .pgp-fingerprint')).toHaveText(RECIPIENT_FINGERPRINT);

  // Model legacy keyless accounts after checking contact visibility with today's required vendor key.
  // Explicitly clear their fixture proofs as well, so re-adding the same public key cannot revive old access.
  execFileSync('psql', [process.env.E2E_DATABASE_URL!, '-v', 'ON_ERROR_STOP=1', '-qtAc',
    `UPDATE sessions SET pgp_access_key='' WHERE user_id IN (SELECT id FROM users WHERE handle IN ('${buyerHandle}','${vendorHandle}'));
      UPDATE users SET pgp='',pgp_fingerprint='',pgp_verified_at=NULL WHERE handle IN ('${buyerHandle}','${vendorHandle}');`]);
  // Neither side has a usable saved key now, so the listing cannot take an order.
  // Saving and authenticating only the buyer's key does not bypass the vendor requirement.
  await buyer.goto(productURL);
  await expect(buyer.getByRole('link', { name: 'Review order draft' })).toHaveCount(0);
  const checkoutURL = productURL.replace('/product?', '/checkout?');
  await buyer.goto(checkoutURL);
  await expect(buyer.locator('form[action="/orders"]')).toHaveCount(0);
  await expect(buyer.locator('main')).toContainText('Add a usable PGP public key');
  await expect(buyer.locator('main')).toContainText(`${vendorHandle} needs a usable PGP encryption key before accepting new orders.`);
  await saveEncryptionKey(buyer, 'browser-contact-buyer-123');
  await seedPGPAccess(buyer);
  await buyer.goto(checkoutURL);
  await expect(buyer.locator('form[action="/orders"]')).toHaveCount(0);
  await expect(buyer.locator('main')).toContainText(`${vendorHandle} needs a usable PGP encryption key before accepting new orders.`);
  await saveEncryptionKey(vendor, 'browser-contact-vendor-123');
  // Saving the vendor's key creates its own account-security notice. Read it now
  // so the later unread-count checks isolate order and role-change notifications.
  await vendor.goto('/notifications');
  await vendor.getByRole('button', { name: 'Mark all as read', exact: true }).click();

  // Cancelling an unfunded draft does not create vendor noise; the buyer can still read its order history.
  await buyer.goto(productURL);
  await buyer.getByRole('link', { name: 'Review order draft' }).click();
  await buyer.getByLabel('Pay with').selectOption('XMR');
  await buyer.getByRole('button', { name: 'Create unfunded draft' }).click();
  await buyer.waitForURL(/\/order\?id=/);
  const orderURL = buyer.url();
  const orderKey = buyer.getByRole('region', { name: 'Vendor’s PGP key' });
  await expect(orderKey.locator('.pgp-fingerprint')).toHaveText(RECIPIENT_FINGERPRINT);
  await expect(orderKey).toContainText('Ownership not verified');
  await expect(buyer.locator(`a[href="/messages?to=${vendorHandle}"]`)).toHaveCount(0);
  await expect(buyer.locator('form[action="/orders/message"]')).toHaveCount(0);
  await buyer.getByRole('button', { name: 'Cancel draft' }).click();
  await expect(buyer.locator('.page-head .badge')).toHaveText('Cancelled');

  await vendor.goto('/');
  await expect(vendorNav.getByRole('link', { name: 'Notifications', exact: true })).toBeVisible();
  await expect(vendorNav.getByRole('link', { name: /Notifications \(\d+ unread\)/ })).toHaveCount(0);
  expect(await vendor.evaluate(() => document.documentElement.scrollWidth)).toBeLessThanOrEqual(320);

  // Demoting the vendor archives the listing: it leaves the catalog and the product page is gone for anyone who
  // cannot read an order on it (the administrator is not a party to the draft, which is not disputed).
  await setRole(admin, vendorHandle, 'buyer');
  await admin.goto('/admin/system'); // A-542: the audit trail is on System & audit
  await expect(admin.locator('main')).toContainText(`Changed role of ${vendorHandle} from vendor to buyer; archived 1 active listing(s)`);
  // The actual demotion notice remains a usable unread notification at 320px; drafts above did not create one.
  await vendor.goto('/');
  const unread = vendorNav.getByRole('link', { name: 'Notifications (1 unread)' });
  await expect(unread).toBeVisible();
  expect(await vendor.evaluate(() => document.documentElement.scrollWidth)).toBeLessThanOrEqual(320);
  await unread.click();
  await expect(vendor.locator('main')).toContainText('Your account role changed from vendor to buyer.');
  await vendor.getByRole('button', { name: 'Mark as read' }).first().click();
  await expect(vendorNav.getByRole('link', { name: 'Notifications', exact: true })).toBeVisible();

  await buyer.goto(`/?q=${encodeURIComponent(title)}`);
  await expect(buyer.getByRole('heading', { name: 'No listings found' })).toBeVisible();
  expect((await admin.goto(productURL))?.status()).toBe(404);

  // A-164: the buyer's (cancelled) order still links to the listing, shown archived with no order-draft link.
  await buyer.goto(orderURL);
  const listingRow = buyer.locator('.key-values > div', { has: buyer.getByText('Listing', { exact: true }) });
  await expect(listingRow).toContainText('Listing as it is now; it may have changed since purchase.');
  await expect(listingRow).toContainText('not recorded for this order');
  const currentListing = listingRow.getByRole('link');
  await expect(currentListing).toHaveAttribute('href', new URL(productURL).pathname + new URL(productURL).search);
  await currentListing.click();
  await buyer.waitForURL(/\/product\?id=/);
  await expect(buyer.locator('.detail-copy .badge')).toHaveText('Archived');
  await expect(buyer.locator('main')).toContainText('This listing accepts no new orders.');
  await expect(buyer.getByRole('link', { name: 'Review order draft' })).toHaveCount(0);
  await fits(buyer, 'archived product page at 320 px');

  for (const p of [vendor, buyer, admin]) await p.context().close();
});
