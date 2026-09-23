import { expect, test } from '@playwright/test';
import { ADMIN, FIXTURE_LISTING, uniqueHandle } from './db-fixtures';

// The browser test server runs without any payment provider (no RPC URLs). Every payment surface must be
// visibly unavailable, no address may appear, and saving a payout address must be refused.
test('payments are visibly disabled without a provider', async ({ page }) => {
  const handle = uniqueHandle('paybuyer');
  await page.goto('/register');
  await page.getByLabel('Handle', { exact: true }).fill(handle);
  await page.getByLabel('Password', { exact: true }).fill('browser-payments-password-123');
  await page.getByRole('button', { name: 'Create account' }).click();
  await expect(page.locator('.account-name')).toHaveText(handle);
  await expect(page.locator('footer')).toContainText('Payments disabled');
  await expect(page.locator('main')).toContainText('Order drafts only.');

  await page.getByRole('link', { name: FIXTURE_LISTING, exact: true }).click();
  await page.getByRole('link', { name: 'Review order draft' }).click();
  await expect(page.locator('main')).toContainText('UNFUNDED DRAFT ONLY.');
  await page.getByLabel('Reference currency').selectOption('BTC');
  await page.getByRole('button', { name: 'Create unfunded draft' }).click();
  await expect(page).toHaveURL(/\/order\?id=/);
  await expect(page.getByRole('heading', { name: 'Payment is unavailable' })).toBeVisible();
  await expect(page.locator('main')).toContainText('BTC payments are not configured on this market');
  await expect(page.locator('.payment-address')).toHaveCount(0);
  await expect(page.locator('body')).not.toContainText('TESTNET');

  await page.goto('/account');
  await expect(page.locator('main')).toContainText('Unavailable — payments disabled');
  await expect(page.getByLabel('Bitcoin payout address')).toHaveCount(0);
  const csrf = await page.locator('form[action="/logout"] input[name="csrf"]').getAttribute('value');
  // Payout changes need the current password; with no provider the request is refused before that check.
  const refused = await page.request.post('/account/payout', { form: { csrf: csrf!, currency: 'BTC', address: 'tb1qexampleaddress000000000000000000000', password: 'browser-payments-password-123' } });
  expect(refused.status()).toBe(409);
  expect(await refused.text()).toContain('BTC payments are not configured');
  await page.getByRole('button', { name: 'Sign out', exact: true }).click();

  await page.goto('/login');
  await page.getByLabel('Handle', { exact: true }).fill(ADMIN.handle);
  await page.getByLabel('Password', { exact: true }).fill(ADMIN.password);
  await page.getByRole('button', { name: 'Sign in' }).click();
  await page.goto('/admin');
  const providers = page.locator('.admin-payments');
  await expect(providers).toContainText('Not configured (BITCOIN_RPC_URL is blank)');
  await expect(providers).toContainText('Not configured (MONERO_WALLET_RPC_URL is blank)');
  await expect(providers).toContainText('No payouts recorded.');
});
