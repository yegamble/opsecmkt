import { expect, test, type Page } from '@playwright/test';

async function noScriptsNoOverflow(page: Page) {
  await expect(page.locator('script')).toHaveCount(0);
  expect(await page.evaluate(() => document.documentElement.scrollWidth)).toBeLessThanOrEqual(page.viewportSize()!.width);
}

test('TOTP enrollment shows a manual-entry secret, no QR code and no scripts', async ({ page }) => {
  expect((await page.goto('/totp'))?.status()).toBe(200);
  await expect(page.getByRole('heading', { level: 1 })).toHaveText('Two-factor authentication.');
  await expect(page.locator('.page-head .badge')).toHaveText('Not enrolled');
  await expect(page.locator('.totp-secret')).toBeVisible();
  await expect(page.locator('.totp-uri')).toContainText('otpauth://totp/');
  await expect(page.getByText('No QR code is shown.')).toBeVisible();
  await expect(page.getByLabel('6-digit code from your app')).toBeVisible();
  await noScriptsNoOverflow(page);
});

for (const path of ['/login', '/register']) {
  test(`${path} shows a same-origin PNG CAPTCHA without audio claims`, async ({ page, baseURL }) => {
    await page.goto(path);
    const image = page.locator('img.captcha-image');
    await expect(image).toBeVisible();
    const src = await image.getAttribute('src');
    expect(new URL(src!, baseURL).origin).toBe(new URL(baseURL!).origin);
    const png = await page.request.get(src!);
    expect(png.status()).toBe(200);
    expect(png.headers()['content-type']).toBe('image/png');
    expect(png.headers()['cache-control']).toBe('no-store');
    expect(await image.evaluate((img: HTMLImageElement) => img.naturalWidth)).toBe(200);
    await expect(page.getByLabel('Characters in the image')).toBeVisible();
    await expect(page.locator('.captcha')).toContainText('There is no audio alternative.');
    await noScriptsNoOverflow(page);
  });
}

test('challenge offers an authenticator code and a recovery code', async ({ page }) => {
  await page.goto('/challenge');
  await expect(page.getByLabel('6-digit code', { exact: true })).toBeVisible();
  await expect(page.getByLabel('Recovery code')).toBeHidden();
  await page.getByText('Use a recovery code instead').click();
  await expect(page.getByLabel('Recovery code')).toBeVisible();
  await noScriptsNoOverflow(page);
});

test('account and admin state TOTP and CAPTCHA status truthfully', async ({ page }) => {
  await page.goto('/account');
  const status = page.locator('.key-values div', { hasText: 'TOTP authentication' });
  await expect(status.locator('dd')).toHaveText('Not enrolled');
  await expect(page.getByRole('link', { name: 'Manage TOTP' })).toBeVisible();
  await page.goto('/admin');
  await expect(page.locator('.admin-auth')).toContainText('Sign-in protection');
  await expect(page.locator('.admin-auth')).toContainText('no audio alternative');
  await noScriptsNoOverflow(page);
});

test('password change and second-factor reset are plain POST forms and the preview refuses them', async ({ page }) => {
  await page.goto('/account');
  const password = page.locator('form[action="/account/password"]');
  await expect(password).toHaveAttribute('method', 'post');
  await expect(password.locator('input[name="csrf"]')).toHaveCount(1);
  for (const label of ['Current password', 'New password', 'Repeat new password']) {
    await expect(password.getByLabel(label, { exact: true })).toBeVisible();
  }
  // TOTP is off in the preview, so no code is asked for.
  await expect(password.getByLabel('Authenticator code or recovery code')).toHaveCount(0);
  await noScriptsNoOverflow(page);
  await password.getByLabel('Current password', { exact: true }).fill('preview-password-123');
  await password.getByLabel('New password', { exact: true }).fill('preview-new-password');
  await password.getByLabel('Repeat new password').fill('preview-new-password');
  await password.getByRole('button', { name: 'Change password' }).click();
  await expect(page.locator('body')).toHaveText('Read-only preview. Start with PostgreSQL to save changes.');

  await page.goto('/admin');
  const reset = page.getByRole('region', { name: "Reset a user's second factors" });
  await expect(reset).toContainText('Administrator accounts cannot be reset here');
  const form = reset.locator('form[action="/admin/reset-factors"]');
  await expect(form).toHaveAttribute('method', 'post');
  await expect(form.getByLabel('Account handle')).toHaveAttribute('pattern', '[a-zA-Z0-9_]{3,32}');
  await expect(reset.locator('.account-list li', { hasText: 'ghost_circuit · TOTP' })).toHaveCount(1);
  await expect(page.locator('table').last().locator('thead')).toContainText('Account');
  await noScriptsNoOverflow(page);
  await form.getByLabel('Account handle').fill('ghost_circuit');
  await form.getByLabel('Current password').fill('preview-password-123');
  await form.getByRole('button', { name: 'Reset second factors' }).click();
  await expect(page.locator('body')).toHaveText('Read-only preview. Start with PostgreSQL to save changes.');

  // Suspend / restore: a handle, a required suspend-or-restore choice and the administrator's password; the
  // suspended accounts are listed so they can be restored.
  await page.goto('/admin');
  const suspension = page.getByRole('region', { name: 'Suspend or restore an account' });
  const suspend = suspension.locator('form[action="/admin/suspend"]');
  await expect(suspend).toHaveAttribute('method', 'post');
  await expect(suspend.locator('input[name="csrf"]')).toHaveCount(1);
  await expect(suspend.getByLabel('Account handle')).toHaveAttribute('pattern', '[a-zA-Z0-9_]{3,32}');
  await expect(suspend.getByRole('group', { name: 'Action' }).getByRole('radio')).toHaveCount(2);
  await expect(suspend.getByRole('radio', { name: /^Suspend:/ })).not.toBeChecked();
  await expect(suspend.getByRole('radio', { name: /^Restore:/ })).not.toBeChecked();
  await expect(suspend.getByLabel('Authenticator code')).toHaveCount(0);
  await expect(suspension.locator('.account-list li')).toHaveText(['held_account · buyer · suspended 2026-01-01 00:00 UTC']);
  await noScriptsNoOverflow(page);
  await suspend.getByLabel('Account handle').fill('held_account');
  await suspend.getByRole('radio', { name: /^Restore:/ }).check();
  await suspend.getByLabel('Current password').fill('preview-password-123');
  await suspend.getByRole('button', { name: 'Suspend or restore' }).click();
  await expect(page.locator('body')).toHaveText('Read-only preview. Start with PostgreSQL to save changes.');
});
