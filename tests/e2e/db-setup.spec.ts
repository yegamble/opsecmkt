import { execFileSync } from 'node:child_process';
import { expect, test } from '@playwright/test';
import { ADMIN, FIXTURE_LISTING, SETUP_TOKEN } from './db-fixtures';
import { saveEncryptionKey, seedPGPAccess, seedStaffFactor } from './db-helpers';

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
  // A-706: the new administrator has no second factor yet. The overview's first step says it is required, and a
  // staff change is refused with links to enrol, changing nothing.
  await expect(page.locator('main')).toContainText('Required before any staff action:');
  await page.goto('/admin/security');
  await page.locator('.admin-auth').getByLabel('Current password').fill(ADMIN.password);
  await page.locator('.admin-auth').getByRole('radio', { name: 'Off' }).check();
  await page.locator('.admin-auth').getByRole('button', { name: 'Save human check' }).click();
  await expect(page.getByRole('alert')).toContainText('Staff actions need a second factor on your account.');
  await expect(page.getByRole('link', { name: 'Set up an authenticator' })).toHaveAttribute('href', '/totp');
  // The shared administrator's factor is a fixture stand-in for PGP sign-in (seedStaffFactor); db-admin.spec.ts
  // enrols a real authenticator for a moderator.
  seedStaffFactor(ADMIN.handle);
  // The setup administrator also sells the shared listing; every vendor needs a
  // usable encryption key before accepting orders.
  await saveEncryptionKey(page, ADMIN.password);
  await seedPGPAccess(page);
  // The general browser suite exercises explicitly unfunded drafts with intake paused.
  // Fresh default escrow-only readiness is checked separately; no pooled checkout exists.
  execFileSync('psql', [process.env.E2E_DATABASE_URL!, '-v', 'ON_ERROR_STOP=1', '-qtAc',
    "UPDATE settings SET value='false' WHERE key IN ('payment_intake_BTC','payment_intake_XMR')"]);

  await page.goto('/vendor-dashboard');
  await page.getByText('+ Publish a new listing', { exact: true }).click();
  await page.getByLabel('Listing title').fill(FIXTURE_LISTING);
  await page.getByLabel('Description', { exact: true }).fill('Deterministic browser regression fixture. Unfunded drafts only.');
  await page.getByLabel('Price (XMR)').fill('0.500000000000');
  await page.getByRole('button', { name: 'Publish listing' }).click();
  await expect(page.getByLabel('Listing title')).toHaveValue(FIXTURE_LISTING); // A-188: publishing lands on its edit page
  await page.goto('/vendor-dashboard');
  await expect(page.locator('.listings-grid')).toContainText(FIXTURE_LISTING);

  // A-725: a fresh install has no image on sign-in, and a written question on registration; a wrong answer creates no
  // account and asks a new question, with no reload needed without JavaScript.
  const visitor = await browser.newContext({ baseURL, javaScriptEnabled: false });
  const anon = await visitor.newPage();
  await anon.goto('/login');
  await expect(anon.locator('img.captcha-image')).toHaveCount(0);
  await expect(anon.locator('.captcha')).toHaveCount(0);
  await anon.goto('/register');
  await expect(anon.locator('img.captcha-image')).toHaveCount(0);
  const asked = await anon.locator('.captcha label').innerText();
  await anon.getByLabel('Handle', { exact: true }).fill('captcha_rejected');
  await anon.getByLabel('Password', { exact: true }).fill('browser-captcha-password-123');
  await anon.getByRole('textbox', { name: asked.trim() }).fill('not an answer');
  const firstID = await anon.locator('input[name="captcha_id"]').getAttribute('value');
  await anon.getByRole('button', { name: 'Create account' }).click();
  await expect(anon.getByRole('alert')).toContainText('That answer is not right. Answer the new question below.');
  await expect(anon.getByLabel('Handle', { exact: true })).toHaveValue('captcha_rejected');
  expect(await anon.locator('input[name="captcha_id"]').getAttribute('value')).not.toBe(firstID);
  await expect(anon.locator('img.captcha-image')).toHaveCount(0);

  // The operator can force the image; a wrong answer creates no account.
  await page.goto('/admin/security');
  await page.locator('.admin-auth').getByRole('radio', { name: 'Always the image' }).check();
  await page.locator('.admin-auth').getByLabel('Current password').fill(ADMIN.password);
  await page.locator('.admin-auth').getByRole('button', { name: 'Save human check' }).click();
  await expect(page.locator('.admin-auth .key-values')).toContainText('Always the image');
  // A-517: the image is forced and there is no staff contact, so the administrator is warned.
  await expect(page.locator('.admin-auth .captcha-contact-warning')).toContainText('the CAPTCHA is on, but no staff contact is set.');
  await anon.goto('/register');
  const image = anon.locator('img.captcha-image');
  const src = await image.getAttribute('src');
  const png = await visitor.request.get(src!);
  expect(png.status()).toBe(200);
  expect(png.headers()['content-type']).toBe('image/png');
  expect(png.headers()['cache-control']).toBe('no-store');
  await anon.getByLabel('Handle', { exact: true }).fill('captcha_rejected');
  await anon.getByLabel('Password', { exact: true }).fill('browser-captcha-password-123');
  await expect(anon.locator('.captcha')).toContainText('There is no audio or text alternative.');
  await anon.getByLabel('Characters in the image').fill('WRONG1');
  await anon.getByRole('button', { name: 'Create account' }).click();
  await expect(anon.locator('body')).toContainText('CAPTCHA answer incorrect');
  // QA-3: the refusal is the form again with a fresh image, so no reload is needed without JavaScript.
  await expect(anon.getByRole('alert')).toContainText('Enter the characters of the new image below.');
  await expect(anon.locator('img.captcha-image')).toHaveCount(1);
  expect(await anon.locator('img.captcha-image').getAttribute('src')).not.toBe(src);
  // A-517: no staff contact is set yet, so the form says there is no way in without the image.
  await expect(anon.locator('.captcha .captcha-fallback')).toContainText('This market lists no contact that works without an account');
  await visitor.close();
  // Turn it off so the other db-*.spec.ts files can register and sign in without a human check.
  await page.goto('/admin/security');
  await page.locator('.admin-auth').getByRole('radio', { name: 'Off' }).check();
  await page.locator('.admin-auth').getByLabel('Current password').fill(ADMIN.password);
  await page.locator('.admin-auth').getByRole('button', { name: 'Save human check' }).click();
  await expect(page.locator('.admin-auth .key-values')).toContainText('Off');
  await expect(page.locator('.admin-auth .captcha-contact-warning')).toHaveCount(0);
  await page.goto('/register');
  await expect(page.locator('img.captcha-image')).toHaveCount(0);
});
