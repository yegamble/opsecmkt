import { execFileSync } from 'node:child_process';
import { expect, test, type Page } from '@playwright/test';
import { uniqueHandle } from './db-fixtures';
import { expectNotice, signedIn, submitStatus } from './db-helpers';
import { waitForStyles } from './styles';

// A-544: under a bond + manual approval policy, a new account signs up as a vendor, sees its bond and the
// administrator's payment instructions, cannot list yet, and an administrator records the bond (no wallet is
// involved: the bond is recorded by hand) and approves; the vendor then publishes a listing, and the
// administrator's verified mark shows on it. A fresh account is made administrator for this test only, so the
// shared administrator's password budget is untouched; the vendor policy is reset to its default at the end. The
// admin page is reopened as /admin, not /admin#vendors: after a post lands on that URL, going to it again is a
// same-document fragment navigation that does not reload the page.

async function noOverflow(page: Page) {
  await expect(page.locator('script')).toHaveCount(0);
  await waitForStyles(page);
  expect(await page.evaluate(() => document.documentElement.scrollWidth)).toBeLessThanOrEqual(page.viewportSize()!.width);
}

test('vendor sign-up under bond and approval, bond recorded, approved, then publishes', async ({ browser, baseURL }) => {
  const database = process.env.E2E_DATABASE_URL!;
  const sql = (query: string) => execFileSync('psql', [database, '-v', 'ON_ERROR_STOP=1', '-qtAc', query], { encoding: 'utf8' }).trim();
  const password = 'browser-vendor-onboarding-123';
  const adminHandle = uniqueHandle('onboard_admin');
  const vendorHandle = uniqueHandle('onboard_vendor');
  const title = `Onboarded ${uniqueHandle('item')}`;
  const admin = await signedIn(browser, baseURL, adminHandle, password, true, 320);
  const panel = admin.getByRole('region', { name: 'Vendor applications' });
  try {
    sql(`UPDATE users SET role='admin' WHERE handle='${adminHandle}'`);
    await admin.goto('/admin');
    await panel.getByLabel('Bond in XMR').fill('0.25');
    await panel.getByRole('radio', { name: /Manual/ }).check();
    await panel.getByLabel('Bond payment instructions').fill('Send the bond on stagenet to the address in the canary.\nThen message the staff contact.');
    const policy = panel.locator('form', { has: admin.getByRole('button', { name: 'Save vendor policy' }) });
    await policy.getByLabel('Current password').fill(password);
    await policy.getByRole('button', { name: 'Save vendor policy' }).click();
    await expectNotice(admin, 'Vendor policy saved. It applies to new applications; open applications keep their bond and approval mode.');
    await expect(admin).toHaveURL(/\/admin#vendors$/);
    await noOverflow(admin);

    // Sign up as a vendor account, with JavaScript off.
    const context = await browser.newContext({ baseURL, javaScriptEnabled: false, viewport: { width: 320, height: 900 } });
    const vendor = await context.newPage();
    await vendor.goto('/register');
    await expect(vendor.locator('#account-type-help')).toContainText('a bond of 0.25 XMR, a non-refundable fee');
    await vendor.getByLabel('Handle', { exact: true }).fill(vendorHandle);
    await vendor.getByLabel('Password', { exact: true }).fill(password);
    await vendor.getByLabel('Vendor account').check();
    await vendor.getByRole('button', { name: 'Create account' }).click();
    await expect(vendor).toHaveURL(/\/account#vendor-application$/);
    const section = vendor.getByRole('region', { name: 'Sell on this market' });
    await expect(section).toContainText('Waiting for bond');
    await expect(section).toContainText('0.25 XMR, a non-refundable fee');
    await expect(section.locator('.vendor-instructions')).toContainText('Send the bond on stagenet to the address in the canary.');
    await expect(vendor.locator('.page-head .badge')).toHaveText('buyer');
    await noOverflow(vendor);
    // Not yet a vendor: the vendor desk is refused.
    expect((await vendor.goto('/vendor-dashboard'))?.status()).toBe(403);

    // The administrator records the bond as received, then approves.
    const decide = async (choice: RegExp, note: string) => {
      await admin.goto('/admin');
      const form = panel.locator('form', { has: admin.getByRole('group', { name: `Decision for ${vendorHandle}` }) });
      await form.getByRole('radio', { name: choice }).check();
      await form.getByLabel('Note to the applicant (optional)').fill(note);
      await form.getByLabel('Current password').fill(password);
      expect(await submitStatus(admin, () => form.getByRole('button', { name: `Record decision for ${vendorHandle}` }).click())).toBe(303);
      await expectNotice(admin, 'Vendor application updated. The applicant was notified.');
    };
    await admin.goto('/admin');
    await expect(panel).toContainText(`${vendorHandle}: Waiting for bond`);
    await decide(/Record bond received/, '');
    await expect(panel).toContainText(`${vendorHandle}: Waiting for administrator approval`);
    await vendor.goto('/account');
    await expect(section).toContainText('Waiting for administrator approval');
    await expect(section).toContainText('recorded by an administrator');
    await decide(/Approve/, 'Welcome to the market');
    await expect(panel).not.toContainText(vendorHandle);

    await vendor.goto('/account');
    await expect(section).toContainText('Approved');
    await expect(section).toContainText('Welcome to the market');
    await expect(vendor.locator('.page-head .badge')).toHaveText('vendor');
    await vendor.goto('/notifications');
    await expect(vendor.locator('main')).toContainText('Your vendor application was approved.');

    // The approved vendor publishes a listing.
    await vendor.goto('/vendor-dashboard');
    await vendor.getByText('+ Publish a new listing', { exact: true }).click();
    await vendor.getByLabel('Listing title').fill(title);
    await vendor.getByLabel('Description', { exact: true }).fill('Vendor onboarding journey listing.');
    await vendor.getByLabel('Fulfillment type').selectOption('digital');
    await vendor.getByLabel('Price (XMR)').fill('0.5');
    await vendor.getByRole('button', { name: 'Publish listing' }).click();
    await vendor.waitForURL(/\/listing-edit\?id=/);
    const product = new URL(vendor.url()).searchParams.get('id')!;

    // The administrator marks the vendor verified: the badge shows on the product page.
    await admin.goto('/admin');
    const verify = panel.locator('form', { has: admin.getByRole('button', { name: 'Save verified mark' }) });
    await verify.getByLabel('Vendor handle').fill(vendorHandle);
    await verify.getByLabel('Mark').selectOption('verify');
    await verify.getByLabel('Current password').fill(password);
    await verify.getByRole('button', { name: 'Save verified mark' }).click();
    await expectNotice(admin, 'Verified vendor mark saved. The vendor was notified.');
    await vendor.goto(`/product?id=${product}`);
    await expect(vendor.locator('.verified-vendor')).toHaveText('Verified vendor');
    await noOverflow(vendor);
    await context.close();
  } finally {
    sql(`UPDATE settings SET value='0' WHERE key='vendor_bond_xmr'; UPDATE settings SET value='manual' WHERE key='vendor_approval';
      UPDATE settings SET value='' WHERE key='vendor_bond_instructions'`);
  }
});
