import { execFileSync } from 'node:child_process';
import { expect, test, type Page } from '@playwright/test';
import { uniqueHandle } from './db-fixtures';
import { saveEncryptionKey, signedIn, submitAndLoad, submitStatus } from './db-helpers';
import { waitForStyles } from './styles';

async function fits(page: Page, width: number) {
  await page.setViewportSize({ width, height: 900 });
  await waitForStyles(page);
  await expect(page.locator('script')).toHaveCount(0);
  expect(await page.evaluate(() => document.documentElement.scrollWidth)).toBeLessThanOrEqual(width);
}

test('moderator selling approval retains moderation and can be revoked independently', async ({ browser, baseURL }) => {
  const sql = (query: string) => execFileSync('psql', [process.env.E2E_DATABASE_URL!, '-v', 'ON_ERROR_STOP=1', '-qtAc', query], { encoding: 'utf8' }).trim();
  const password = 'browser-role-capabilities-123';
  const adminHandle = uniqueHandle('cap_admin');
  const modHandle = uniqueHandle('cap_mod');
  const admin = await signedIn(browser, baseURL, adminHandle, password, true, 320);
  const mod = await signedIn(browser, baseURL, modHandle, password, true, 320);
  const policy = sql(`SELECT value FROM settings WHERE key='vendor_approval'`);
  const bond = sql(`SELECT value FROM settings WHERE key='vendor_bond_xmr'`);
  try {
    sql(`UPDATE users SET role='admin' WHERE handle='${adminHandle}'; UPDATE users SET role='moderator' WHERE handle='${modHandle}';
      UPDATE settings SET value='manual' WHERE key='vendor_approval'; UPDATE settings SET value='0' WHERE key='vendor_bond_xmr'`);
    expect((await mod.goto('/vendor-dashboard'))?.status()).toBe(403);
    // Selling approval requires a usable encryption key, including for moderators.
    await saveEncryptionKey(mod, password);
    await expect(mod.getByRole('navigation', { name: 'Main navigation' }).getByRole('link', { name: 'Moderation' })).toBeVisible();
    await mod.getByRole('button', { name: 'Apply to sell', exact: true }).click();
    await expect(mod.getByRole('region', { name: 'Sell on this market' })).toContainText('Waiting for administrator approval');
    await admin.goto('/admin/vendors');
    const decision = admin.locator('form', { has: admin.getByRole('group', { name: `Decision for ${modHandle}` }) });
    await decision.getByRole('radio', { name: /Approve/ }).check();
    await decision.getByLabel('Current password').fill(password);
    await submitAndLoad(admin, decision.getByRole('button', { name: `Record decision for ${modHandle}` }));
    expect(sql(`SELECT role || ':' || vendor_approved FROM users WHERE handle='${modHandle}'`)).toBe('moderator:true');
    await mod.goto('/vendor-dashboard');
    for (const width of [320, 390, 768, 1280]) await fits(mod, width);
    const nav = mod.getByRole('navigation', { name: 'Main navigation' });
    await expect(nav.getByRole('link', { name: 'Vendor desk' })).toBeVisible();
    await expect(nav.getByRole('link', { name: /^Moderation/ })).toBeVisible();
    expect((await mod.goto('/admin'))?.status()).toBe(403);

    await admin.goto(`/admin/users?handle=${modHandle}`);
    const revoke = admin.locator('form[action="/admin/vendor-revoke"]');
    await expect(revoke.getByRole('button', { name: 'Remove selling approval' })).toBeVisible();
    await revoke.getByLabel('Current password').fill(password);
    expect(await submitStatus(admin, () => revoke.getByRole('button', { name: 'Remove selling approval' }).click())).toBe(303);
    expect(sql(`SELECT role || ':' || vendor_approved FROM users WHERE handle='${modHandle}'`)).toBe('moderator:false');
    expect((await mod.goto('/vendor-dashboard'))?.status()).toBe(403);
    expect((await mod.goto('/moderator'))?.status()).toBe(200);
    for (const width of [320, 390, 768, 1280]) await fits(mod, width);
  } finally {
    sql(`UPDATE settings SET value='${policy}' WHERE key='vendor_approval'; UPDATE settings SET value='${bond}' WHERE key='vendor_bond_xmr'`);
    await admin.context().close();
    await mod.context().close();
  }
});
