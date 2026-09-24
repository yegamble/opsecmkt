import { createHmac } from 'node:crypto';
import { expect, test, type Page } from '@playwright/test';
import { uniqueHandle } from './db-fixtures';

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
  await page.getByRole('button', { name: 'Sign out', exact: true }).click();
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
  await page.getByRole('button', { name: 'Verify and turn on' }).click();
  await expect(page.locator('body')).toContainText('Code incorrect');
  await page.goto('/totp');
  await expect(page.locator('.page-head .badge')).toHaveText('Not enrolled');
  await page.getByLabel('6-digit code from your app').fill(totp(secret, step));
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

  await signOutAndIn(page, handle, password);
  await page.getByText('Use a recovery code instead').click();
  await page.getByLabel('Recovery code').fill(codes[0]);
  await page.getByRole('button', { name: 'Use recovery code' }).click();
  await expect(page.locator('body')).toContainText('Recovery code incorrect or already used');
});
