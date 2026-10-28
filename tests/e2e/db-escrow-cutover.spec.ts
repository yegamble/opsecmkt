import { execFileSync } from 'node:child_process';
import { randomBytes } from 'node:crypto';
import { expect, test } from '@playwright/test';
import { uniqueHandle } from './db-fixtures';
import { expectNotice, saveEncryptionKey, signedIn, submitStatus, withinViewport, seedPGPAccess } from './db-helpers';

// The application has no provider in this suite. Paused intake allows unfunded
// Monero drafts; enabling escrow fails closed until the operator configures it.
test('Monero-only intake, forged Bitcoin refusals and unavailable escrow work without scripts', async ({ browser, baseURL }) => {
  const sql = (query: string) => execFileSync('psql', [process.env.E2E_DATABASE_URL!, '-v', 'ON_ERROR_STOP=1', '-qtAc', query], { encoding: 'utf8' }).trim();
  const adminHandle = uniqueHandle('intake_admin');
  const vendorHandle = uniqueHandle('intake_vendor');
  const password = 'browser-monero-intake-password-123';
  const product = randomBytes(16).toString('hex');
  const title = `Monero ${uniqueHandle('item')}`;
  const admin = await signedIn(browser, baseURL, adminHandle, password, true, 320);
  const vendor = await signedIn(browser, baseURL, vendorHandle, password, true, 320);
  const buyer = await signedIn(browser, baseURL, uniqueHandle('intake_buyer'), password, true, 320);
  try {
    await saveEncryptionKey(vendor, password);
    await saveEncryptionKey(buyer, password);
    await seedPGPAccess(vendor);
    await seedPGPAccess(buyer);
    sql(`UPDATE settings SET value='false' WHERE key='payment_intake_XMR';
      UPDATE users SET role='admin',pgp_2fa=true WHERE handle='${adminHandle}';
      UPDATE users SET role='vendor' WHERE handle='${vendorHandle}';
      INSERT INTO products(id,vendor_id,title,description,category,region,kind,xmr,stock)
      SELECT '${product}',id,'${title}','Monero-only fixture','Digital','Worldwide','digital',500000000000,3 FROM users WHERE handle='${vendorHandle}';`);
    await admin.goto('/admin/payments');
    await expect(admin.getByRole('button', { name: 'Save BTC intake' })).toHaveCount(0);
    let intake = admin.locator('form[action="/admin/payment-intake"]');
    await expect(intake).toHaveCount(1);
    await expect(intake.getByRole('radio')).toHaveCount(2);
    await withinViewport(intake, 320);
    // Alter the real form only from the test driver to exercise server validation.
    await intake.locator('input[name="currency"]').evaluate((input: HTMLInputElement) => { input.value = 'BTC'; });
    await intake.getByLabel('Current password', { exact: true }).fill(password);
    expect(await submitStatus(admin, () => intake.getByRole('button', { name: 'Save XMR intake' }).click())).toBe(400);
    expect(sql(`SELECT value FROM settings WHERE key='payment_intake_BTC'`)).toBe('false');

    await buyer.goto(`/checkout?id=${product}`);
    await expect(buyer.getByLabel('Pay with').locator('option')).toHaveCount(1);
    await expect(buyer.getByLabel('Pay with')).toHaveValue('XMR');
    await buyer.getByLabel('Pay with').evaluate((select: HTMLSelectElement) => { select.add(new Option('BTC', 'BTC')); select.value = 'BTC'; });
    expect(await submitStatus(buyer, () => buyer.getByRole('button', { name: 'Create unfunded draft' }).click())).toBe(400);
    expect(sql(`SELECT count(*) FROM orders WHERE product_id='${product}'`)).toBe('0');
    await buyer.goto(`/checkout?id=${product}`);
    await buyer.getByRole('button', { name: 'Create unfunded draft' }).click();
    await expect(buyer).toHaveURL(/\/order\?id=/);
    await expect(buyer.getByRole('heading', { name: 'Unfunded draft' })).toBeVisible();
    expect(sql(`SELECT currency FROM orders WHERE product_id='${product}'`)).toBe('XMR');
    expect(sql(`SELECT count(*) FROM escrows e JOIN orders o ON o.id=e.order_id WHERE o.product_id='${product}'`)).toBe('0');

    await admin.goto('/admin/payments');
    intake = admin.locator('form[action="/admin/payment-intake"]');
    await intake.getByRole('radio', { name: /^Escrow$/ }).check();
    await intake.getByLabel('Current password', { exact: true }).fill('incorrect-password');
    expect(await submitStatus(admin, () => intake.getByRole('button', { name: 'Save XMR intake' }).click())).toBe(401);
    await admin.goto('/admin/payments');
    intake = admin.locator('form[action="/admin/payment-intake"]');
    await intake.getByRole('radio', { name: /^Escrow$/ }).check();
    await intake.getByLabel('Current password', { exact: true }).fill(password);
    await intake.getByRole('button', { name: 'Save XMR intake' }).click();
    await expectNotice(admin, 'New payments set to escrow only.');
    await buyer.goto(`/checkout?id=${product}`);
    await expect(buyer.locator('form[action="/orders"]')).toHaveCount(0);
    await expect(buyer.getByText('Not available for purchase in XMR.')).toBeVisible();
  } finally {
    sql(`UPDATE settings SET value='false' WHERE key='payment_intake_XMR'`);
    await admin.context().close(); await vendor.context().close(); await buyer.context().close();
  }
});
