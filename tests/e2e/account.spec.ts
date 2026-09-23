import { expect, test } from '@playwright/test';

// One stateful journey, one fresh CI database. Never point E2E_DATABASE_URL at production.
// Contexts stay separate so an administrator's session cannot mask buyer authorization bugs.
test('setup, publish, register, draft, profile persistence and session boundaries', async ({ browser, baseURL }) => {
  const admin = await browser.newContext({ baseURL, javaScriptEnabled: false });
  const buyer = await browser.newContext({ baseURL, javaScriptEnabled: false });
  const operatorPage = await admin.newPage();
  const buyerPage = await buyer.newPage();
  try {
    await operatorPage.goto('/setup');
    await operatorPage.getByLabel('Setup token').fill('e2e-local-only-setup-token-at-least-32-characters');
    await operatorPage.getByLabel('Handle', { exact: true }).fill('browser_admin');
    await operatorPage.getByLabel('Password', { exact: true }).fill('browser-admin-password-123');
    await operatorPage.getByRole('button', { name: 'Initialize marketplace' }).click();
    await expect(operatorPage.locator('.account-name')).toHaveText('browser_admin');
    await operatorPage.goto('/vendor-dashboard');
    await operatorPage.getByText('+ Publish a new listing', { exact: true }).click();
    await operatorPage.getByLabel('Listing title').fill('Browser regression test hardware');
    await operatorPage.getByLabel('Description', { exact: true }).fill('Deterministic browser regression fixture. Unfunded drafts only.');
    await operatorPage.getByLabel('Bitcoin reference price').fill('0.00100000');
    await operatorPage.getByLabel('Monero reference price').fill('0.500000000000');
    await operatorPage.getByRole('button', { name: 'Publish listing' }).click();
    await expect(operatorPage.locator('.listings-grid')).toContainText('Browser regression test hardware');
    await buyerPage.goto('/register');
    await buyerPage.getByLabel('Handle', { exact: true }).fill('browser_buyer');
    await buyerPage.getByLabel('Password', { exact: true }).fill('browser-buyer-password-123');
    await buyerPage.getByRole('button', { name: 'Create account' }).click();
    await expect(buyerPage.locator('.account-name')).toHaveText('browser_buyer');
    await buyerPage.getByRole('link', { name: 'Browser regression test hardware', exact: true }).click();
    await buyerPage.getByRole('link', { name: 'Review order draft' }).click();
    await buyerPage.getByLabel('Reference currency').selectOption('XMR');
    await buyerPage.getByRole('button', { name: 'Create unfunded draft' }).click();
    await expect(buyerPage).toHaveURL(/\/order\?id=/);
    await expect(buyerPage.getByRole('heading', { name: 'Payment is unavailable' })).toBeVisible();
    await expect(buyerPage.locator('.total')).toContainText('XMR');
    expect((await buyerPage.goto('/admin'))?.status()).toBe(403);
    await buyerPage.goto('/account');
    await buyerPage.getByLabel('XMPP address').fill('browser_buyer@example.test');
    await buyerPage.getByRole('button', { name: 'Save profile' }).click();
    await buyerPage.reload();
    await expect(buyerPage.getByLabel('XMPP address')).toHaveValue('browser_buyer@example.test');
    await buyerPage.getByRole('button', { name: 'Sign out', exact: true }).click();
    await buyerPage.goto('/account');
    await expect(buyerPage).toHaveURL(/\/login$/);
    await buyerPage.getByLabel('Handle', { exact: true }).fill('browser_buyer');
    await buyerPage.getByLabel('Password', { exact: true }).fill('wrong-password-123');
    await buyerPage.getByRole('button', { name: 'Sign in' }).click();
    await expect(buyerPage.locator('body')).toContainText('Invalid handle or password');
    await buyerPage.goto('/login');
    await buyerPage.getByLabel('Handle', { exact: true }).fill('browser_buyer');
    await buyerPage.getByLabel('Password', { exact: true }).fill('browser-buyer-password-123');
    await buyerPage.getByRole('button', { name: 'Sign in' }).click();
    await buyerPage.goto('/account');
    await expect(buyerPage.getByLabel('XMPP address')).toHaveValue('browser_buyer@example.test');
  } finally {
    await admin.close();
    await buyer.close();
  }
});
