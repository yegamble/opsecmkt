import { expect, test, type Page } from '@playwright/test';

// P2 PGP identity: truthful key, verification and message-encryption states in the read-only preview.

async function fitsWithoutScripts(page: Page, path: string) {
  const response = await page.goto(path);
  expect(response?.status()).toBe(200);
  await expect(page.locator('script')).toHaveCount(0);
  expect(await page.evaluate(() => document.documentElement.scrollWidth)).toBeLessThanOrEqual(page.viewportSize()!.width);
}

test('PGP page shows the fingerprint, an unverified key and a sign challenge', async ({ page }) => {
  await fitsWithoutScripts(page, '/pgp');
  await expect(page.getByRole('heading', { level: 1 })).toHaveText('PGP verification.');
  await expect(page.locator('.page-head .badge')).toHaveText('Not verified');
  await expect(page.locator('.pgp-fingerprint')).toHaveText('0000 0000 0000 0000 0000 0000 0000 0000 0000 0000');
  await expect(page.getByRole('heading', { name: 'Sign exactly this text' })).toBeVisible();
  await expect(page.getByLabel('Clearsigned message or armored detached signature')).toBeVisible();
  await expect(page.getByRole('radio', { name: 'Sign a text with your private key' })).toBeChecked();
  // PGP sign-in cannot be switched on before ownership is proven.
  await expect(page.getByText('Unavailable until ownership of this key is verified.')).toBeVisible();
  await expect(page.getByRole('button', { name: 'Turn on PGP sign-in verification' })).toHaveCount(0);
});

test('account page reports PGP state without claiming verification', async ({ page }) => {
  await fitsWithoutScripts(page, '/account');
  const row = page.locator('.key-values div').filter({ hasText: 'PGP sign-in verification' });
  await expect(row.locator('dd')).toHaveText('No key saved');
  await expect(page.getByText(/^Verified/)).toHaveCount(0);
});

test('messages show encryption status from packet inspection', async ({ page }) => {
  await fitsWithoutScripts(page, '/messages');
  const badges = page.locator('.pgp-message-status .badge');
  await expect(badges).toHaveText([
    'Encrypted (to recipient’s key)',
    'Encrypted (recipient unknown)',
    'Encrypted (NOT to recipient’s key)',
  ]);
  await expect(page.locator('.pgp-status-note')).toContainText('cannot decrypt messages');
});

test('sign-in challenge offers a PGP code without JavaScript', async ({ page }) => {
  await fitsWithoutScripts(page, '/challenge');
  await expect(page.getByRole('heading', { name: 'PGP key' })).toBeVisible();
  await expect(page.getByRole('button', { name: 'Create encrypted code' })).toBeVisible();
});
