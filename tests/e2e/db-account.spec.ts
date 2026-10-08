import { expect, test } from '@playwright/test';
import { FIXTURE_LISTING, uniqueHandle } from './db-fixtures';
import { submitAndLoad } from './db-helpers';

// Uses the listing published by db-setup.spec.ts and registers its own buyer.
test('register, draft, removed XMPP field and session boundaries', async ({ page, browser, baseURL }) => {
  const handle = uniqueHandle('buyer');
  const password = 'browser-buyer-password-123';
  await page.goto('/register');
  await page.getByLabel('Handle', { exact: true }).fill(handle);
  await page.getByLabel('Password', { exact: true }).fill(password);
  await page.getByRole('button', { name: 'Create account' }).click();
  await expect(page.locator('.account-name')).toHaveText(handle);
  // The catalog shows 25 listings a page (A-181) and other specs publish more: search for the setup fixture.
  await page.goto(`/?q=${encodeURIComponent(FIXTURE_LISTING)}`);
  await page.getByRole('link', { name: FIXTURE_LISTING, exact: true }).click();
  await page.getByRole('link', { name: 'Review order draft' }).click();
  await page.getByLabel('Reference currency').selectOption('XMR');
  await page.getByRole('button', { name: 'Create unfunded draft' }).click();
  await expect(page).toHaveURL(/\/order\?id=/);
  await expect(page.getByRole('heading', { name: 'Payment is unavailable' })).toBeVisible();
  await expect(page.locator('.total')).toContainText('XMR');
  await expect(page.locator('.page-head .badge')).toHaveText('Draft — unfunded');
  expect((await page.goto('/admin'))?.status()).toBe(403);
  await page.goto('/account');
  await expect(page.getByLabel('XMPP address')).toHaveCount(0);
  await submitAndLoad(page, page.getByRole('button', { name: 'Sign out', exact: true }));
  await page.goto('/account');
  await expect(page).toHaveURL(/\/login\?next=%2Faccount$/);
  await page.getByLabel('Handle', { exact: true }).fill(handle);
  await page.getByLabel('Password', { exact: true }).fill('wrong-password-123');
  await page.getByRole('button', { name: 'Sign in' }).click();
  await expect(page.locator('body')).toContainText('Invalid handle or password');
  await page.goto('/login');
  await page.getByLabel('Handle', { exact: true }).fill(handle);
  await page.getByLabel('Password', { exact: true }).fill(password);
  await submitAndLoad(page, page.getByRole('button', { name: 'Sign in' }));
  await page.goto('/account');
  await expect(page.getByLabel('XMPP address')).toHaveCount(0);
  const activity = page.getByRole('region', { name: 'Recent activity' });
  await expect(activity).toContainText('Signed in');
  await expect(activity).not.toContainText('Updated profile');
  await expect(activity).toContainText('Account created: buyer');

  // "Sign out everywhere" ends this session and one opened in a separate browser context.
  const other = await browser.newContext({ baseURL, javaScriptEnabled: false });
  const otherPage = await other.newPage();
  await otherPage.goto('/login');
  await otherPage.getByLabel('Handle', { exact: true }).fill(handle);
  await otherPage.getByLabel('Password', { exact: true }).fill(password);
  await submitAndLoad(otherPage, otherPage.getByRole('button', { name: 'Sign in' }));
  await otherPage.goto('/account');
  await expect(otherPage).toHaveURL(/\/account$/);
  // A-157: both sessions are listed with their start time; each browser sees its own marked.
  await page.reload();
  const sessions = page.getByRole('region', { name: 'Sessions' });
  await expect(sessions.locator('.session-count')).toHaveText('2 active sessions');
  await expect(sessions.locator('.session-row')).toHaveCount(2);
  await expect(sessions.locator('.session-row', { hasText: 'This session' })).toHaveCount(1);
  for (const row of await sessions.locator('.session-started').allTextContents()) {
    expect(row).toMatch(/^\d{4}-\d{2}-\d{2} \d{2}:\d{2} UTC$/);
  }
  await expect(sessions).toContainText('Only changing your password stops them.');
  await expect(otherPage.getByRole('region', { name: 'Sessions' }).locator('.session-row', { hasText: 'This session' })).toHaveCount(1);
  await page.getByRole('button', { name: 'Sign out everywhere' }).click();
  await expect(page).toHaveURL(/\/login$/);
  await page.goto('/account');
  await expect(page).toHaveURL(/\/login\?next=%2Faccount$/);
  await otherPage.goto('/account');
  await expect(otherPage).toHaveURL(/\/login\?next=%2Faccount$/);
  await other.close();
});
