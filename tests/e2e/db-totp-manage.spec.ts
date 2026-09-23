import { expect, test, type Page } from '@playwright/test';
import { uniqueHandle } from './db-fixtures';
import { signedIn, submitStatus, totp } from './db-helpers';

// TOTP management after enrollment: replacing recovery codes invalidates the old set, and turning TOTP off
// needs the password plus a current code. Codes are computed in the test runner (RFC 6238, SHA-1, 30 s).
//
// The server accepts steps now-1..now+1 and only steps above the last one used (replay guard). The test
// therefore spends three increasing steps from one window: activation uses step-1, the replacement uses
// step, and turning off uses step+1, which stays acceptable until the window after next (>= 30 s later).

async function signOutAndChallenge(page: Page, handle: string, password: string) {
  await page.goto('/account');
  await page.getByRole('button', { name: 'Sign out', exact: true }).click();
  await page.goto('/login');
  await page.getByLabel('Handle', { exact: true }).fill(handle);
  await page.getByLabel('Password', { exact: true }).fill(password);
  await page.getByRole('button', { name: 'Sign in' }).click();
  await expect(page).toHaveURL(/\/challenge$/);
}

test('replacing recovery codes invalidates the old ones; TOTP turns off with password and code', async ({ browser, baseURL }) => {
  const handle = uniqueHandle('totpmgmt');
  const password = 'browser-totp-manage-123';
  const page = await signedIn(browser, baseURL, handle, password, true);
  await page.goto('/totp');
  await page.getByRole('button', { name: 'Start enrollment ↗' }).click();
  const secret = (await page.locator('.totp-secret').innerText()).replace(/\s+/g, '');

  // Stay clear of a step boundary so step-1 is still inside the server's window when it checks it.
  const intoWindow = Date.now() % 30_000;
  if (intoWindow > 20_000) await page.waitForTimeout(30_500 - intoWindow);
  const step = Math.floor(Date.now() / 30_000);
  await page.getByLabel('6-digit code from your app').fill(totp(secret, step - 1));
  await page.getByRole('button', { name: 'Verify and turn on ↗' }).click();
  await expect(page.locator('.page-head .badge')).toHaveText('Enabled');
  const oldCodes = await page.locator('.recovery-codes li').allInnerTexts();
  expect(oldCodes).toHaveLength(10);
  await page.reload();
  await expect(page.locator('.key-values div', { hasText: 'Unused recovery codes' }).locator('dd')).toHaveText('10 of 10');

  // Replacing the codes needs a fresh authenticator code: a wrong one and the spent activation code are refused.
  const replace = page.getByLabel('Current authenticator code');
  const wrong = ['000000', '111111', '222222'].find(c => ![-1, 0, 1, 2].some(d => totp(secret, step + d) === c))!;
  for (const code of [wrong, totp(secret, step - 1)]) {
    await page.goto('/totp');
    await replace.fill(code);
    expect(await submitStatus(page, () => page.getByRole('button', { name: 'Replace recovery codes' }).click())).toBe(401);
    await expect(page.locator('body')).toHaveText('Verification code incorrect, expired or already used');
  }
  await page.goto('/totp');
  await expect(page.locator('.recovery-codes')).toHaveCount(0);
  await replace.fill(totp(secret, step));
  await page.getByRole('button', { name: 'Replace recovery codes' }).click();
  await expect(page).toHaveURL(/\/totp$/);
  const newCodes = await page.locator('.recovery-codes li').allInnerTexts();
  expect(newCodes).toHaveLength(10);
  expect(newCodes.filter(c => oldCodes.includes(c))).toEqual([]);

  // An old recovery code no longer completes sign-in; a new one does, once.
  await signOutAndChallenge(page, handle, password);
  await page.getByText('Use a recovery code instead').click();
  await page.getByLabel('Recovery code').fill(oldCodes[0]);
  expect(await submitStatus(page, () => page.getByRole('button', { name: 'Use recovery code' }).click())).toBe(401);
  await expect(page.locator('body')).toContainText('Recovery code incorrect or already used');
  await page.goto('/challenge');
  await page.getByText('Use a recovery code instead').click();
  await page.getByLabel('Recovery code').fill(newCodes[0]);
  await page.getByRole('button', { name: 'Use recovery code' }).click();
  await expect(page.locator('.account-name')).toHaveText(handle);
  await page.goto('/totp');
  await expect(page.locator('.key-values div', { hasText: 'Unused recovery codes' }).locator('dd')).toHaveText('9 of 10');

  // Turning TOTP off: wrong password, then an invalidated recovery code, are both refused and change nothing.
  const off = async (pw: string, code: string) => {
    await page.goto('/totp');
    await page.getByLabel('Password', { exact: true }).fill(pw);
    await page.getByLabel('Authenticator code or recovery code').fill(code);
    return submitStatus(page, () => page.getByRole('button', { name: 'Turn off TOTP' }).click());
  };
  expect(await off('not-the-password-123', totp(secret, step + 1))).toBe(401);
  await expect(page.locator('body')).toHaveText('Password incorrect');
  expect(await off(password, oldCodes[1])).toBe(401);
  await expect(page.locator('body')).toHaveText('Recovery code incorrect or already used');
  await page.goto('/totp');
  await expect(page.locator('.page-head .badge')).toHaveText('Enabled');

  expect(await off(password, totp(secret, step + 1))).toBe(303);
  await expect(page).toHaveURL(/\/totp$/);
  await expect(page.locator('.page-head .badge')).toHaveText('Not enrolled');
  await expect(page.getByRole('button', { name: 'Start enrollment ↗' })).toBeVisible();
  await page.goto('/account');
  await expect(page.locator('.key-values div', { hasText: 'TOTP authentication' }).locator('dd')).toHaveText('Not enrolled');
  const activity = page.getByRole('region', { name: 'Recent activity' });
  await expect(activity).toContainText('Replaced recovery codes; previous codes invalidated');
  await expect(activity).toContainText('Turned off TOTP two-factor authentication (confirmed with password and authenticator code)');

  // Sign-in is password-only again.
  await page.getByRole('button', { name: 'Sign out', exact: true }).click();
  await page.goto('/login');
  await page.getByLabel('Handle', { exact: true }).fill(handle);
  await page.getByLabel('Password', { exact: true }).fill(password);
  await page.getByRole('button', { name: 'Sign in' }).click();
  await expect(page.locator('.account-name')).toHaveText(handle);
  await expect(page).not.toHaveURL(/\/challenge$/);
  await page.context().close();
});
