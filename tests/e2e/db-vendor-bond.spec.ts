import { expect, test } from '@playwright/test';
import { ADMIN, uniqueHandle } from './db-fixtures';
import { setRole, signedIn, submitStatus, withinViewport } from './db-helpers';

test('admin bond policy persists and blocks new selling with JavaScript disabled', async ({ browser, baseURL }) => {
  const handle = uniqueHandle('bondvendor');
  const vendor = await signedIn(browser, baseURL, handle, 'browser-bond-vendor-password', true);
  const admin = await signedIn(browser, baseURL, ADMIN.handle, ADMIN.password, false);
  await setRole(admin, handle, 'vendor');
  const save = async (amount: string, currency: string) => {
    await admin.goto('/admin');
    const policy = admin.getByRole('region', { name: 'Vendor bond requirement' });
    await policy.getByLabel('Required bond amount').fill(amount);
    await policy.getByLabel('Bond reference currency').selectOption(currency);
    await policy.getByLabel('Current password').fill(ADMIN.password);
    return submitStatus(admin, () => policy.getByRole('button', { name: 'Save vendor bond requirement' }).click());
  };
  try {
    expect(await save('250', 'GBP')).toBe(303);
    await expect(admin).toHaveURL(/\/admin#vendor-bond$/);
    await admin.reload();
    const policy = admin.getByRole('region', { name: 'Vendor bond requirement' });
    await expect(policy.getByLabel('Required bond amount')).toHaveValue('250');
    await expect(policy.getByLabel('Bond reference currency')).toHaveValue('GBP');
    await expect(policy).toContainText('No accepted rate snapshot yet');
    await vendor.goto('/vendor-dashboard');
    const bond = vendor.getByRole('region', { name: 'Required vendor bond' });
    await expect(bond).toContainText('250 GBP');
    await expect(bond).toContainText('New listings and orders are paused');
    await expect(vendor.locator('form[action="/listings"]')).toHaveCount(0);
    await expect(bond).toContainText('Automatic bond deposits are unavailable');
    await withinViewport(policy, admin.viewportSize()!.width);
    expect(await vendor.locator('html').evaluate(e => e.scrollWidth)).toBeLessThanOrEqual(vendor.viewportSize()!.width);
  } finally {
    expect(await save('0', 'USD')).toBe(303);
    await vendor.goto('/vendor-dashboard');
    await expect(vendor.locator('form[action="/listings"]')).toHaveCount(1);
    await vendor.context().close();
    await admin.context().close();
  }
});
