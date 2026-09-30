import { execFileSync } from 'node:child_process';
import { randomBytes } from 'node:crypto';
import { expect, test, type Page } from '@playwright/test';
import { uniqueHandle } from './db-fixtures';
import { expectNotice, signedIn, submitAndLoad } from './db-helpers';

// A-163: an administrator takes down another vendor's listing with a reason, confirmed with their password. The
// vendor sees the reason on the listing and the vendor desk and has no restore control; only the administrator's
// confirmed restore brings it back. Fresh accounts only: a new administrator (promoted in SQL) and a new vendor
// whose listing is seeded in SQL, so the shared administrator's password budget is untouched.
async function fits(page: Page, label: string) {
  for (const width of [390, 320]) {
    await page.setViewportSize({ width, height: 900 });
    expect(await page.evaluate(() => document.documentElement.scrollWidth), `${label} at ${width}px`).toBeLessThanOrEqual(width);
  }
}

test('administrator takes down a listing with a reason and only an administrator restores it', async ({ browser, baseURL }) => {
  const database = process.env.E2E_DATABASE_URL!;
  const sql = (query: string) => execFileSync('psql', [database, '-v', 'ON_ERROR_STOP=1', '-qtAc', query], { encoding: 'utf8' }).trim();
  const adminPassword = 'browser-takedown-admin-password-123';
  const adminHandle = uniqueHandle('takedown_admin');
  const vendorHandle = uniqueHandle('takedown_vendor');
  const productID = randomBytes(16).toString('hex');
  const title = `Takedown ${uniqueHandle('item')}`;
  // A long unbroken word checks that the reason wraps on a phone.
  const reason = `Counterfeit goods: copies a trademarked product. Ref ${'X'.repeat(60)}`;
  const admin = await signedIn(browser, baseURL, adminHandle, adminPassword, true);
  const vendor = await signedIn(browser, baseURL, vendorHandle, 'browser-takedown-vendor-123', true);
  const visitorContext = await browser.newContext({ baseURL, javaScriptEnabled: false, viewport: { width: 390, height: 844 } });
  const visitor = await visitorContext.newPage();
  try {
    sql(`UPDATE users SET role='admin' WHERE handle='${adminHandle}';
      UPDATE users SET role='vendor' WHERE handle='${vendorHandle}';
      INSERT INTO products(id,vendor_id,title,description,category,region,kind,btc,xmr,stock)
        SELECT '${productID}',id,'${title}','Seeded by db-listing-takedown.spec.ts','Digital','Worldwide','digital',100000,500000000000,1 FROM users WHERE handle='${vendorHandle}';`);
    const editURL = `/listing-edit?id=${productID}`;

    // The administrator opens the takedown form on the listing's edit page; there is no plain archive button.
    await admin.goto(editURL);
    await expect(admin.getByRole('button', { name: 'Archive listing' })).toHaveCount(0);
    await admin.locator('summary', { hasText: 'Take down listing' }).click();
    await admin.getByLabel('Reason for the vendor').fill(reason);
    await admin.getByLabel('Current password').fill(adminPassword);
    await fits(admin, 'open takedown form');
    await submitAndLoad(admin, admin.getByRole('button', { name: 'Take down listing' }));
    await expect(admin).toHaveURL(new RegExp(`/listing-edit\\?id=${productID}$`));
    await expectNotice(admin, 'Listing taken down.');
    await admin.reload();
    await expect(admin.locator('.badge', { hasText: 'Taken down' })).toBeVisible();
    await expect(admin.locator('.takedown-reason')).toContainText(reason);

    // Gone publicly, and the reason is not shown anywhere public.
    const product = await visitor.goto(`/product?id=${productID}`);
    expect(product?.status()).toBe(404);
    await visitor.goto(`/?q=${encodeURIComponent(title)}`);
    await expect(visitor.getByRole('heading', { name: 'No listings found' })).toBeVisible();

    // The vendor sees the reason on the desk card and the edit page, with no restore control, and is notified.
    await vendor.goto('/vendor-dashboard');
    const card = vendor.locator('.product-card', { hasText: title });
    await expect(card.locator('.badge')).toHaveText('Taken down');
    await expect(card).toContainText(reason);
    await expect(card).toContainText('contact the market staff');
    await expect(card.getByRole('button', { name: `Restore ${title}` })).toHaveCount(0);
    await fits(vendor, 'vendor desk with a taken-down listing');
    await vendor.goto(editURL);
    await expect(vendor.locator('.takedown-reason')).toContainText(reason);
    await expect(vendor.locator('main')).toContainText('Only an administrator can restore this listing. If you think this is wrong, contact the market staff.');
    await expect(vendor.getByRole('button', { name: 'Restore listing' })).toHaveCount(0);
    await fits(vendor, 'vendor edit page of a taken-down listing');
    await vendor.goto('/notifications');
    await expect(vendor.locator('main')).toContainText(reason);

    // The administrator restores it with their password; it is public again.
    await admin.goto(editURL);
    await admin.getByLabel('Current password').fill(adminPassword);
    await submitAndLoad(admin, admin.getByRole('button', { name: 'Restore listing' }));
    await expect(admin).toHaveURL(new RegExp(`/listing-edit\\?id=${productID}$`));
    await expectNotice(admin, 'Listing restored.');
    const restored = await visitor.goto(`/product?id=${productID}`);
    expect(restored?.status()).toBe(200);
    await vendor.goto('/vendor-dashboard');
    await expect(vendor.locator('.product-card', { hasText: title }).locator('.badge')).toHaveCount(0);

    expect(sql(`SELECT count(*) FROM notifications n JOIN users u ON u.id=n.user_id WHERE u.handle='${vendorHandle}' AND n.body NOT LIKE '%http%'`)).toBe('2');
    expect(sql(`SELECT count(*) FROM audit_events a JOIN users u ON u.id=a.user_id WHERE u.handle='${adminHandle}' AND a.action LIKE '% listing ${productID.slice(0, 8)} of ${vendorHandle}%(confirmed with password)'`)).toBe('2');
  } finally {
    // Leave no public fixture listing and no extra administrator for the other specs.
    sql(`UPDATE products SET archived=true,archived_at=now(),admin_archive_reason=NULL WHERE id='${productID}';
      DELETE FROM sessions WHERE user_id=(SELECT id FROM users WHERE handle='${adminHandle}');
      UPDATE users SET role='buyer' WHERE handle='${adminHandle}';`);
    await Promise.allSettled([admin.context(), vendor.context(), visitorContext].map(context => context.close()));
  }
});
