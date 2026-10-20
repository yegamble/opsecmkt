import { execFileSync } from 'node:child_process';
import { expect, test, type Page } from '@playwright/test';
import { uniqueHandle } from './db-fixtures';
import { expectNotice, signedIn, submitAndLoad } from './db-helpers';
import { waitForStyles } from './styles';

// MOD72-6 listing hold mode, with JavaScript off: a fresh administrator holds listings for review, a fresh vendor
// publishes one, a buyer can neither find nor open it, a fresh moderator approves it on the desk and the buyer then
// sees it. Fresh accounts are promoted in SQL so the shared administrator's password budget is untouched, and the
// mode is put back to publish afterwards because the database specs share global settings.
async function fits(page: Page, label: string) {
  for (const width of [390, 320]) {
    await page.setViewportSize({ width, height: 900 });
    await waitForStyles(page);
    expect(await page.evaluate(() => document.documentElement.scrollWidth), `${label} at ${width}px`).toBeLessThanOrEqual(width);
  }
  await expect(page.locator('script')).toHaveCount(0);
}

test('administrator holds listings, a moderator approves one and only then buyers see it', async ({ browser, baseURL }) => {
  const database = process.env.E2E_DATABASE_URL!;
  const sql = (query: string) => execFileSync('psql', [database, '-v', 'ON_ERROR_STOP=1', '-qtAc', query], { encoding: 'utf8' }).trim();
  const adminPassword = 'browser-review-admin-password-123';
  const modPassword = 'browser-review-mod-password-123';
  const adminHandle = uniqueHandle('review_admin');
  const modHandle = uniqueHandle('review_mod');
  const vendorHandle = uniqueHandle('review_vendor');
  const title = `Held ${uniqueHandle('item')}`;
  const admin = await signedIn(browser, baseURL, adminHandle, adminPassword, true);
  const moderator = await signedIn(browser, baseURL, modHandle, modPassword, true);
  const vendor = await signedIn(browser, baseURL, vendorHandle, 'browser-review-vendor-123', true);
  const buyer = await signedIn(browser, baseURL, uniqueHandle('review_buyer'), 'browser-review-buyer-123', true);
  try {
    sql(`UPDATE users SET role='admin' WHERE handle='${adminHandle}';
      UPDATE users SET role='moderator' WHERE handle='${modHandle}';
      UPDATE users SET role='vendor' WHERE handle='${vendorHandle}';`);

    // The administrator chooses hold for review on Admin → Vendors, confirmed with their password.
    await admin.goto('/admin/vendors#listing-review');
    const mode = admin.locator('form', { has: admin.getByRole('button', { name: 'Save listing review' }) });
    await mode.getByLabel('Hold for review by a moderator or administrator').check();
    await mode.getByLabel('Current password').fill(adminPassword);
    await submitAndLoad(admin, mode.getByRole('button', { name: 'Save listing review' }));
    await expect(admin).toHaveURL(/\/admin\/vendors#listing-review$/);
    await expectNotice(admin, 'Listing review saved. Changing it approves nothing: listings already waiting for review still need a moderator or administrator.');
    await expect(admin.getByLabel('Hold for review by a moderator or administrator')).toBeChecked();
    await fits(admin, 'admin listing review panel');

    // The vendor is told before publishing, and the new listing waits marked "Awaiting review".
    await vendor.goto('/vendor-dashboard');
    await vendor.getByText('+ Publish a new listing', { exact: true }).click();
    await expect(vendor.locator('.listing-held')).toContainText('a new listing stays hidden from buyers until a moderator or administrator approves it');
    await vendor.getByLabel('Listing title').fill(title);
    await vendor.getByLabel('Description', { exact: true }).fill('Listing review browser regression.');
    await vendor.getByLabel('Fulfillment type').selectOption('digital');
    await vendor.getByLabel('Price (XMR)').fill('0.5');
    await submitAndLoad(vendor, vendor.getByRole('button', { name: 'Publish listing' }));
    await expectNotice(vendor, 'Listing created. It waits for staff review: buyers cannot see it until a moderator or administrator approves it. Add photos here.');
    const id = new URL(vendor.url()).searchParams.get('id')!;
    expect(id).toMatch(/^[0-9a-f]{64}$/);
    await expect(vendor.locator('.page-head .badge')).toHaveText('Awaiting review');
    await vendor.goto('/vendor-dashboard');
    await expect(vendor.locator('.product-card', { hasText: title }).locator('.badge')).toHaveText('Awaiting review');
    await fits(vendor, 'vendor desk with a listing awaiting review');

    // The buyer can neither find nor open it.
    await buyer.goto(`/?q=${encodeURIComponent(title)}`);
    await expect(buyer.getByRole('heading', { name: 'No listings found' })).toBeVisible();
    expect((await buyer.goto(`/product?id=${id}`))?.status()).toBe(404);

    // The moderator approves it on the desk with their password.
    await moderator.goto('/moderator#listing-review');
    const entry = moderator.locator('.listing-review', { hasText: title });
    await expect(entry).toContainText(vendorHandle);
    await expect(entry.locator('summary')).toHaveText(`Reject ${title}`);
    const approve = entry.locator('form', { has: moderator.getByRole('button', { name: `Approve ${title}` }) });
    await approve.getByLabel('Current password').fill(modPassword);
    await fits(moderator, 'moderation desk review queue');
    await submitAndLoad(moderator, approve.getByRole('button', { name: `Approve ${title}` }));
    await expect(moderator).toHaveURL(/\/moderator#listing-review$/);
    await expectNotice(moderator, 'Listing approved. It is visible in the marketplace; the vendor was notified.');
    await expect(moderator.locator('.listing-review', { hasText: title })).toHaveCount(0);

    // Now the buyer finds and opens it.
    await buyer.goto(`/?q=${encodeURIComponent(title)}`);
    await expect(buyer.getByRole('link', { name: title })).toBeVisible();
    expect((await buyer.goto(`/product?id=${id}`))?.status()).toBe(200);
    await expect(buyer.locator('main')).not.toContainText('Awaiting review');

    expect(sql(`SELECT count(*) FROM audit_events a JOIN users u ON u.id=a.user_id WHERE u.handle='${modHandle}' AND a.action='Approved listing ${id.slice(0, 8)} of ${vendorHandle} (confirmed with password)'`)).toBe('1');
    expect(sql(`SELECT count(*) FROM audit_events a JOIN users u ON u.id=a.user_id WHERE u.handle='${adminHandle}' AND a.action='Listing review mode changed from publish to hold (confirmed with password)'`)).toBe('1');
  } finally {
    // Put the shared setting back and leave no public fixture listing or extra staff for the other specs.
    sql(`UPDATE settings SET value='publish' WHERE key='listing_review_mode';
      UPDATE products SET archived=true,archived_at=now() WHERE vendor_id=(SELECT id FROM users WHERE handle='${vendorHandle}');
      DELETE FROM sessions WHERE user_id IN (SELECT id FROM users WHERE handle IN ('${adminHandle}','${modHandle}'));
      UPDATE users SET role='buyer' WHERE handle IN ('${adminHandle}','${modHandle}');`);
    await Promise.allSettled([admin, moderator, vendor, buyer].map(page => page.context().close()));
  }
});
