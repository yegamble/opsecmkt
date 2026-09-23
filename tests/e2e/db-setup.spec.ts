import { expect, test } from '@playwright/test';
import { ADMIN, FIXTURE_LISTING, SETUP_TOKEN } from './db-fixtures';

// Runs once against the fresh database before every other db-*.spec.ts. Never point E2E_DATABASE_URL at production.
test('initialize the market and publish the fixture listing', async ({ page, browser, baseURL }) => {
  await page.goto('/setup');
  await page.getByLabel('Setup token').fill(SETUP_TOKEN);
  await page.getByLabel('Handle', { exact: true }).fill(ADMIN.handle);
  await page.getByLabel('Password', { exact: true }).fill(ADMIN.password);
  await page.getByRole('button', { name: 'Initialize marketplace' }).click();
  await expect(page).toHaveURL(/\/admin\?welcome=1$/);
  await expect(page.getByRole('heading', { name: 'Get your marketplace ready' })).toBeVisible();
  await expect(page.locator('.account-name')).toHaveText(ADMIN.handle);
  await page.goto('/vendor-dashboard');
  await page.getByText('+ Publish a new listing', { exact: true }).click();
  await page.getByLabel('Listing title').fill(FIXTURE_LISTING);
  await page.getByLabel('Description', { exact: true }).fill('Deterministic browser regression fixture. Unfunded drafts only.');
  await page.getByLabel('Bitcoin reference price').fill('0.00100000');
  await page.getByLabel('Monero reference price').fill('0.500000000000');
  await page.getByRole('button', { name: 'Publish listing' }).click();
  await expect(page.locator('.listings-grid')).toContainText(FIXTURE_LISTING);

  // P1: a fresh install requires the image CAPTCHA; a wrong answer creates no account.
  const visitor = await browser.newContext({ baseURL, javaScriptEnabled: false });
  const anon = await visitor.newPage();
  await anon.goto('/register');
  const image = anon.locator('img.captcha-image');
  const src = await image.getAttribute('src');
  const png = await visitor.request.get(src!);
  expect(png.status()).toBe(200);
  expect(png.headers()['content-type']).toBe('image/png');
  await anon.getByLabel('Handle', { exact: true }).fill('captcha_rejected');
  await anon.getByLabel('Password', { exact: true }).fill('browser-captcha-password-123');
  await anon.getByLabel('Characters in the image').fill('WRONG1');
  await anon.getByRole('button', { name: 'Create account' }).click();
  await expect(anon.locator('body')).toContainText('CAPTCHA answer incorrect');
  await visitor.close();
  // Turn it off so the other db-*.spec.ts files can register and sign in without solving images.
  await page.goto('/admin');
  await page.locator('.admin-auth').getByRole('button', { name: 'Turn CAPTCHA off' }).click();
  await expect(page.locator('.admin-auth')).toContainText('Off');
  await page.goto('/register');
  await expect(page.locator('img.captcha-image')).toHaveCount(0);
});
