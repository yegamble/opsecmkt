import { execFileSync } from 'node:child_process';
import { expect, test } from '@playwright/test';
import { ADMIN, uniqueHandle } from './db-fixtures';
import { signedIn } from './db-helpers';

// No wallet or node RPC runs here. The normal database suite explicitly pauses intake
// for unfunded draft coverage; migration tests separately verify fresh escrow defaults.
test('fresh payment surfaces offer escrow only and removed pooled routes cannot write', async ({ browser, baseURL }) => {
  const password = 'browser-payments-password-123';
  const buyer = await signedIn(browser, baseURL, uniqueHandle('paybuyer'), password, true);
  const database = process.env.E2E_DATABASE_URL!;
  const sql = (query: string) => execFileSync('psql', [database, '-v', 'ON_ERROR_STOP=1', '-qtAc', query], { encoding: 'utf8' }).trim();
  try {
    await buyer.goto('/account');
    // A-508: no reader of the saved pooled payout addresses remains; the account page no longer lists them.
    await expect(buyer.getByRole('heading', { name: 'Historical payout addresses' })).toHaveCount(0);
    await expect(buyer.locator('#unsent-payouts')).toHaveCount(0);
    await expect(buyer.locator('form[action="/account/payout"]')).toHaveCount(0);
    const csrf = await buyer.locator('input[name="csrf"]').first().inputValue();
    const before = sql('SELECT (SELECT count(*) FROM payment_addresses)::text||\':\'||(SELECT count(*) FROM payouts)::text');
    for (const route of ['/orders/pay', '/account/payout', '/admin/payout', '/admin/payment-address-collisions']) {
      const response = await buyer.request.post(route, { form: { csrf, currency: 'BTC', address: 'tb1qretired', op: 'sent', password }, maxRedirects: 0 });
      expect(response.status(), route).toBe(404);
    }
    expect(sql('SELECT (SELECT count(*) FROM payment_addresses)::text||\':\'||(SELECT count(*) FROM payouts)::text')).toBe(before);
    const admin = await browser.newPage({ baseURL, javaScriptEnabled: false });
    await admin.goto('/login');
    await admin.getByLabel('Handle', { exact: true }).fill(ADMIN.handle);
    await admin.getByLabel('Password', { exact: true }).fill(ADMIN.password);
    await admin.getByRole('button', { name: 'Sign in' }).click();
    await admin.goto('/admin/payments');
    await expect(admin.getByRole('heading', { name: 'Escrow status and payment intake' })).toBeVisible();
    for (const currency of ['XMR']) {
      const form = admin.locator('form', { has: admin.getByRole('button', { name: `Save ${currency} intake` }) });
      await expect(form.getByRole('radio')).toHaveCount(2);
      await expect(form.getByRole('radio', { name: /^Escrow$/ })).toHaveAttribute('value', 'escrow');
      await expect(form.getByRole('radio', { name: /^Paused/ })).toHaveAttribute('value', 'false');
    }
    await expect(admin.locator('form[action="/admin/payout"]')).toHaveCount(0);
    // A-492: no payout table, count or link on the admin pages (A-542: the payments section and the overview).
    await expect(admin.locator('#payouts')).toHaveCount(0);
    await expect(admin.locator('a[href*="#payouts"]')).toHaveCount(0);
    await admin.goto('/admin');
    await expect(admin.locator('#payouts, a[href*="#payouts"]')).toHaveCount(0);
    await expect(admin.getByRole('navigation', { name: 'Main navigation' })).not.toContainText('payouts');
    await admin.context().close();
  } finally { await buyer.context().close(); }
});
