import { expect, test, type Page } from '@playwright/test';
import { FIXTURE_LISTING, uniqueHandle } from './db-fixtures';

// The db-account journey (register, unfunded draft, profile persistence, sign-out) at desktop width. The
// database project's default viewport is 390 px; a second database project would run every db spec twice
// against the shared administrator and its sign-in limit, so only this journey is repeated at 1280 px.
test.use({ viewport: { width: 1280, height: 900 } });

async function fits(page: Page) {
  expect(await page.evaluate(() => document.documentElement.scrollWidth)).toBeLessThanOrEqual(1280);
}

test('desktop: register, draft, profile persistence and sign-out', async ({ page }) => {
  const handle = uniqueHandle('desktop');
  const password = 'browser-desktop-password-123';
  await page.goto('/register');
  await page.getByLabel('Handle', { exact: true }).fill(handle);
  await page.getByLabel('Password', { exact: true }).fill(password);
  await page.getByRole('button', { name: 'Create account' }).click();
  await expect(page.locator('.account-name')).toHaveText(handle);

  // At desktop width the catalog shows the filter sidebar instead of the collapsible filter panel.
  await expect(page.locator('.desktop-filters')).toBeVisible();
  await expect(page.locator('.filter-details')).toBeHidden();
  await fits(page);

  await page.getByRole('link', { name: FIXTURE_LISTING, exact: true }).click();
  await fits(page);
  await page.getByRole('link', { name: 'Review order draft' }).click();
  await page.getByLabel('Reference currency').selectOption('XMR');
  await page.getByRole('button', { name: 'Create unfunded draft' }).click();
  await expect(page).toHaveURL(/\/order\?id=/);
  await expect(page.getByRole('heading', { name: 'Payment is unavailable' })).toBeVisible();
  await expect(page.locator('.total')).toContainText('XMR');
  await expect(page.locator('.page-head .badge')).toHaveText('Draft — unfunded');
  await fits(page);

  await page.goto('/orders');
  await expect(page.locator('tr', { hasText: FIXTURE_LISTING }).first()).toContainText('Buying');
  await fits(page);

  await page.goto('/account');
  await page.getByLabel('XMPP address').fill(`${handle}@example.test`);
  await page.getByRole('button', { name: 'Save profile' }).click();
  await page.reload();
  await expect(page.getByLabel('XMPP address')).toHaveValue(`${handle}@example.test`);
  await fits(page);
  await page.getByRole('button', { name: 'Sign out', exact: true }).click();
  await page.goto('/account');
  await expect(page).toHaveURL(/\/login$/);
  await page.getByLabel('Handle', { exact: true }).fill(handle);
  await page.getByLabel('Password', { exact: true }).fill(password);
  await page.getByRole('button', { name: 'Sign in' }).click();
  await page.goto('/account');
  await expect(page.getByLabel('XMPP address')).toHaveValue(`${handle}@example.test`);
});
