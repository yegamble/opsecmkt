import { createHmac } from 'node:crypto';
import { expect, test, type Page } from '@playwright/test';
import { FIXTURE_LISTING, uniqueHandle } from './db-fixtures';
import { enrollTOTP, submitAndLoad } from './db-helpers';

// RFC 6238 TOTP (SHA-1, 30 s, 6 digits) computed in the test runner, not in the browser.
function base32Decode(input: string): Buffer {
  const alphabet = 'ABCDEFGHIJKLMNOPQRSTUVWXYZ234567';
  let bits = 0, value = 0;
  const out: number[] = [];
  for (const ch of input.toUpperCase().replace(/[\s=]/g, '')) {
    value = (value << 5) | alphabet.indexOf(ch);
    bits += 5;
    if (bits >= 8) {
      out.push((value >>> (bits - 8)) & 0xff);
      bits -= 8;
    }
  }
  return Buffer.from(out);
}

function totp(secret: string, step: number): string {
  const counter = Buffer.alloc(8);
  counter.writeBigUInt64BE(BigInt(step));
  const mac = createHmac('sha1', base32Decode(secret)).update(counter).digest();
  const offset = mac[mac.length - 1] & 0x0f;
  return ((mac.readUInt32BE(offset) & 0x7fffffff) % 1_000_000).toString().padStart(6, '0');
}

const currentStep = () => Math.floor(Date.now() / 30_000);
const totpStatus = (page: Page) => page.locator('.key-values div', { hasText: 'TOTP authentication' }).locator('dd');

async function signOutAndIn(page: Page, handle: string, password: string) {
  await page.goto('/account');
  await submitAndLoad(page, page.getByRole('button', { name: 'Sign out', exact: true }));
  await page.goto('/login');
  await page.getByLabel('Handle', { exact: true }).fill(handle);
  await page.getByLabel('Password', { exact: true }).fill(password);
  await page.getByRole('button', { name: 'Sign in' }).click();
  await expect(page).toHaveURL(/\/challenge$/);
  await expect(page.getByRole('heading', { level: 1 })).toHaveText('Verify your sign-in.');
}

test('enroll TOTP, sign in through the challenge and spend a recovery code once', async ({ page }) => {
  const handle = uniqueHandle('totp');
  const password = 'browser-totp-password-123';
  await page.goto('/register');
  await page.getByLabel('Handle', { exact: true }).fill(handle);
  await page.getByLabel('Password', { exact: true }).fill(password);
  await page.getByRole('button', { name: 'Create account' }).click();
  await expect(page.locator('.account-name')).toHaveText(handle);

  await page.goto('/account');
  await expect(totpStatus(page)).toHaveText('Not enrolled');
  await page.getByRole('link', { name: 'Manage TOTP' }).click();
  await page.getByRole('button', { name: 'Start enrollment' }).click();
  await expect(page.getByText('No QR code is shown.')).toBeVisible();
  const secret = (await page.locator('.totp-secret').innerText()).replace(/\s+/g, '');
  expect(secret).toMatch(/^[A-Z2-7]{32}$/);
  await expect(page.locator('.totp-uri')).toContainText(`otpauth://totp/`);

  const step = currentStep();
  const wrong = ['000000', '111111', '222222'].find(c => ![-1, 0, 1, 2].some(d => totp(secret, step + d) === c))!;
  await page.getByLabel('6-digit code from your app').fill(wrong);
  await page.getByLabel('Current password').fill(password);
  await page.getByRole('button', { name: 'Verify and turn on' }).click();
  await expect(page.locator('body')).toContainText('Code incorrect');
  await page.goto('/totp');
  await expect(page.locator('.page-head .badge')).toHaveText('Not enrolled');
  await page.getByLabel('6-digit code from your app').fill(totp(secret, step));
  // Adding the first sign-in factor needs the current password too (A-152).
  await page.getByLabel('Current password').fill(password);
  await page.getByRole('button', { name: 'Verify and turn on' }).click();
  await expect(page.locator('.page-head .badge')).toHaveText('Enabled');
  const codes = await page.locator('.recovery-codes li').allInnerTexts();
  expect(codes).toHaveLength(10);
  await page.waitForLoadState();
  await page.reload();
  await expect(page.locator('.recovery-codes')).toHaveCount(0);
  await page.goto('/account');
  await expect(totpStatus(page)).toHaveText('Enabled');

  // The activation step is spent (replay guard); the next step's code completes sign-in.
  await signOutAndIn(page, handle, password);
  await page.getByLabel('6-digit code', { exact: true }).fill(totp(secret, step + 1));
  await page.getByRole('button', { name: 'Verify code' }).click();
  await expect(page.locator('.account-name')).toHaveText(handle);

  await signOutAndIn(page, handle, password);
  await page.getByText('Use a recovery code instead').click();
  await page.getByLabel('Recovery code').fill(codes[0]);
  await page.getByRole('button', { name: 'Use recovery code' }).click();
  await expect(page.locator('.account-name')).toHaveText(handle);
  // The owner is told, and the sign-in names its method (A-152).
  await page.goto('/notifications');
  await expect(page.locator('main')).toContainText('A recovery code was used to sign in to your account (9 remaining).');
  await page.goto('/account');
  await expect(page.locator('main')).toContainText('Signed in with password and recovery code');

  await signOutAndIn(page, handle, password);
  await page.getByText('Use a recovery code instead').click();
  await page.getByLabel('Recovery code').fill(codes[0]);
  await page.getByRole('button', { name: 'Use recovery code' }).click();
  await expect(page.locator('body')).toContainText('Recovery code incorrect or already used');
});

// A-126: an anonymous buyer's "Review order draft" leads to sign-in and back to that checkout, through registration
// and through the TOTP challenge, with JavaScript disabled.
test('anonymous checkout returns to the checkout after registering and after the TOTP challenge', async ({ browser, baseURL }) => {
  const handle = uniqueHandle('next');
  const passphrase = `${handle}-long-enough`;
  const anonymous = async () => {
    const context = await browser.newContext({ baseURL, javaScriptEnabled: false, viewport: { width: 390, height: 900 } });
    return context.newPage();
  };

  let page = await anonymous();
  await page.goto(`/?q=${encodeURIComponent(FIXTURE_LISTING)}`);
  await page.getByRole('link', { name: FIXTURE_LISTING, exact: true }).click();
  const productID = new URL(page.url()).searchParams.get('id')!;
  const checkout = `/checkout?id=${productID}`;
  await page.getByRole('link', { name: 'Review order draft' }).click();
  await expect(page).toHaveURL(`/login?next=${encodeURIComponent(checkout)}`);
  await expect(page.locator('main')).toContainText('Sign in to see Checkout.');

  await page.getByRole('link', { name: 'Create an account' }).click();
  await expect(page).toHaveURL(/\/register\?next=/);
  expect(new URL(page.url()).searchParams.get('next')).toBe(checkout); // html/template writes lower-case %-escapes
  await page.getByLabel('Handle', { exact: true }).fill(handle);
  await page.getByLabel('Password', { exact: true }).fill(passphrase);
  await page.getByRole('button', { name: 'Create account' }).click();
  await expect(page).toHaveURL(checkout);
  await expect(page.getByRole('button', { name: 'Create unfunded draft' })).toBeVisible();

  const secret = await enrollTOTP(page, passphrase);
  await page.context().close();

  page = await anonymous();
  await page.goto(`/product?id=${productID}`);
  await page.getByRole('link', { name: 'Review order draft' }).click();
  await expect(page).toHaveURL(`/login?next=${encodeURIComponent(checkout)}`);
  await page.getByLabel('Handle', { exact: true }).fill(handle);
  await page.getByLabel('Password', { exact: true }).fill(passphrase);
  await page.getByRole('button', { name: 'Sign in' }).click();
  await expect(page).toHaveURL(new RegExp(`/challenge\\?next=${encodeURIComponent(checkout)}$`));
  // The enrollment's step is spent (replay guard); the next step's code completes sign-in.
  await page.getByLabel('6-digit code', { exact: true }).fill(totp(secret, currentStep() + 1));
  await page.getByRole('button', { name: 'Verify code' }).click();
  await expect(page).toHaveURL(checkout);
  await expect(page.locator('.account-name')).toHaveText(handle);
  await expect(page.getByRole('button', { name: 'Create unfunded draft' })).toBeVisible();
  await page.context().close();
});
