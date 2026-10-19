import { expect, test, type Page } from '@playwright/test';
import { waitForStyles } from './styles';

// A-544 vendor onboarding, read-only preview (layout evidence only; the preview refuses every POST). The sign-up
// form offers a buyer or vendor account without scripts, /account shows a sample application waiting for its bond,
// /admin shows the vendors panel, and the sample vendor carries the verified badge.

async function noScriptsNoOverflow(page: Page) {
  await expect(page.locator('script')).toHaveCount(0);
  await waitForStyles(page);
  expect(await page.evaluate(() => document.documentElement.scrollWidth)).toBeLessThanOrEqual(page.viewportSize()!.width);
}

test('register offers a buyer or a vendor account as plain radios', async ({ page }) => {
  await page.goto('/register');
  const type = page.getByRole('group', { name: 'Account type' });
  await expect(type.getByLabel('Buyer account')).toBeChecked();
  await expect(type.getByLabel('Vendor account')).not.toBeChecked();
  await expect(page.locator('#account-type-help')).toContainText('A vendor account is a buyer account with a vendor application');
  await expect(page.locator('#account-type-help')).toContainText('a bond of 0.5 XMR, a non-refundable fee');
  await noScriptsNoOverflow(page);
});

test('account shows a vendor application waiting for its bond', async ({ page }) => {
  await page.goto('/account');
  const section = page.getByRole('region', { name: 'Sell on this market' });
  await expect(section).toContainText('Waiting for bond');
  await expect(section).toContainText('0.5 XMR, a non-refundable fee');
  await expect(section.getByRole('heading', { name: 'Bond payment instructions' })).toBeVisible();
  await expect(section.getByRole('button', { name: 'Withdraw application' })).toBeVisible();
  await noScriptsNoOverflow(page);
});

test('admin vendors panel shows the policy, the queue and the verified mark', async ({ page }) => {
  await page.goto('/admin');
  const panel = page.getByRole('region', { name: 'Vendor applications' });
  await expect(panel.getByLabel('Bond in XMR')).toHaveValue('0.5');
  await expect(panel.getByRole('radio', { name: /Manual/ })).toBeChecked();
  await expect(panel.getByLabel('Bond payment instructions')).toHaveValue('Preview only.');
  await expect(panel.getByRole('group', { name: 'Decision for new_seller' }).getByRole('radio', { name: /Record bond received/ })).toBeVisible();
  await expect(panel.getByRole('group', { name: 'Decision for paid_seller' }).getByRole('radio', { name: /Approve/ })).toBeVisible();
  await expect(panel.getByRole('button', { name: 'Save verified mark' })).toBeVisible();
  await expect(panel).toContainText('ghost_circuit');
  await noScriptsNoOverflow(page);
});

test('the sample verified vendor shows the badge on its page, product and card', async ({ page }) => {
  for (const path of ['/vendor?id=ghost', '/product?id=encrypted-drive', '/']) {
    await page.goto(path);
    await expect(page.locator('.verified-vendor').first()).toHaveText('Verified vendor');
    await noScriptsNoOverflow(page);
  }
});
