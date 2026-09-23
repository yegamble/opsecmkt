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
