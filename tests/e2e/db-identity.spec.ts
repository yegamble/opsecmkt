import { expect, test, type Page } from '@playwright/test';
import { uniqueHandle } from './db-fixtures';
import { enrollTOTP, signedIn, submitStatus } from './db-helpers';

// Self-service password change against a real database with JavaScript disabled, on a fresh account
// (no administrator sign-ins). The administrator second-factor reset is exercised in db-admin.spec.ts.

async function changePassword(page: Page, current: string, next: string, opts: { repeat?: string; code?: string } = {}) {
  await page.goto('/account');
  const form = page.locator('form', { has: page.getByRole('button', { name: 'Change password' }) });
  await form.getByLabel('Current password').fill(current);
  if (opts.code !== undefined) await form.getByLabel('Authenticator code or recovery code').fill(opts.code);
  await form.getByLabel('New password', { exact: true }).fill(next);
  await form.getByLabel('Repeat new password').fill(opts.repeat ?? next);
  return submitStatus(page, () => form.getByRole('button', { name: 'Change password' }).click());
}

async function signIn(page: Page, handle: string, password: string) {
  await page.goto('/login');
  await page.getByLabel('Handle', { exact: true }).fill(handle);
  await page.getByLabel('Password', { exact: true }).fill(password);
  return submitStatus(page, () => page.getByRole('button', { name: 'Sign in' }).click());
}

test('changing the password needs the current one, ends other sessions and keeps this one', async ({ browser, baseURL }) => {
  const handle = uniqueHandle('pwchange');
  const first = 'browser-first-password-1';
  const second = 'browser-second-password-2';
  const third = 'browser-third-password-3';
  const page = await signedIn(browser, baseURL, handle, first, true);
  const other = await signedIn(browser, baseURL, handle, first, false);

  // The form fits a phone screen without scripts.
  await page.goto('/account');
  await expect(page.getByRole('heading', { name: 'Change password' })).toBeVisible();
  await expect(page.locator('script')).toHaveCount(0);
  expect(await page.evaluate(() => document.documentElement.scrollWidth)).toBeLessThanOrEqual(page.viewportSize()!.width);
  await expect(page.getByLabel('Authenticator code or recovery code')).toHaveCount(0);

  expect(await changePassword(page, 'not-my-password-123', second)).toBe(401);
  await expect(page.locator('body')).toHaveText('Password incorrect');
  expect(await changePassword(page, first, second, { repeat: `${second}-typo` })).toBe(400);
  await expect(page.locator('body')).toHaveText('The new passwords do not match.');

  expect(await changePassword(page, first, second)).toBe(303);
  await expect(page).toHaveURL(/\/account\?saved=1$/);
  await expect(page.getByRole('status')).toHaveText('Changes saved.');
  await expect(page.locator('.account-name')).toHaveText(handle);
  const activity = page.getByRole('region', { name: 'Recent activity' });
  await expect(activity).toContainText('Changed password; ended 1 other session(s) and any pending sign-ins (confirmed with password)');

  // The other browser was signed out; the old password no longer works, the new one does.
  await other.goto('/account');
  await expect(other).toHaveURL(/\/login$/);
  expect(await signIn(other, handle, first)).toBe(401);
  await expect(other.locator('body')).toHaveText('Invalid handle or password');
  expect(await signIn(other, handle, second)).toBe(303);
  await expect(other.locator('.account-name')).toHaveText(handle);
  await other.context().close();

  // With TOTP enrolled the change also needs a code; a one-time recovery code is accepted once.
  await enrollTOTP(page);
  const recovery = (await page.locator('.recovery-codes li').allInnerTexts())[0];
  await page.goto('/account');
  await expect(page.getByLabel('Authenticator code or recovery code')).toBeVisible();
  expect(await changePassword(page, second, third, { code: recovery })).toBe(303);
  await expect(page).toHaveURL(/\/account\?saved=1$/);
  await expect(activity).toContainText('Changed password; ended 1 other session(s) and any pending sign-ins (confirmed with password and recovery code)');
  expect(await changePassword(page, third, first, { code: recovery })).toBe(401);
  await expect(page.locator('body')).toHaveText('Recovery code incorrect or already used');
  await page.goto('/totp');
  await expect(page.locator('.key-values div', { hasText: 'Unused recovery codes' }).locator('dd')).toHaveText('9 of 10');
  await page.context().close();
});
