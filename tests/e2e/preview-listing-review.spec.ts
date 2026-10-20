import { expect, test, type Page } from '@playwright/test';
import { waitForStyles } from './styles';

// MOD72-6 listing hold mode, read-only preview (layout evidence only; the preview refuses every POST and seeds no
// database). The moderation desk shows a sample listing awaiting review with its approve form and a reject form behind
// a disclosure, and Admin → Vendors shows the listing review panel. Each preview project runs this at its own width,
// 320 px included.

async function noScriptsNoOverflow(page: Page) {
  await expect(page.locator('script')).toHaveCount(0);
  await waitForStyles(page);
  expect(await page.evaluate(() => document.documentElement.scrollWidth)).toBeLessThanOrEqual(page.viewportSize()!.width);
}

test('moderation desk lists a listing awaiting review with labelled approve and reject forms', async ({ page }) => {
  await page.goto('/moderator');
  const section = page.getByRole('region', { name: 'Listings awaiting review' });
  const entry = section.locator('.listing-review', { hasText: 'Sample listing awaiting review' });
  await expect(entry.getByRole('link', { name: 'Sample listing awaiting review' })).toHaveAttribute('href', '/product?id=preview-held');
  await expect(entry).toContainText('new_seller');
  const approve = entry.locator('form', { has: page.getByRole('button', { name: 'Approve Sample listing awaiting review' }) });
  await expect(approve.getByLabel('Current password')).toBeVisible();
  await expect(approve.getByRole('button', { name: 'Approve Sample listing awaiting review' })).toHaveText('Approve');
  await entry.locator('summary', { hasText: 'Reject Sample listing awaiting review' }).click();
  await expect(entry.getByLabel('Reason for the vendor')).toHaveAttribute('minlength', '10');
  await expect(entry.getByRole('button', { name: 'Reject Sample listing awaiting review' })).toBeVisible();
  await noScriptsNoOverflow(page);
});

test('admin vendors section shows the listing review mode and the vendors who publish without review', async ({ page }) => {
  await page.goto('/admin/vendors');
  const mode = page.getByRole('group', { name: 'New and edited listings' });
  await expect(mode.getByRole('radio', { name: 'Hold for review by a moderator or administrator' })).toBeChecked();
  await expect(mode.getByRole('radio', { name: 'Publish at once' })).not.toBeChecked();
  await expect(page.locator('#listing-review-help')).toContainText('Switching back to publish at once approves nothing');
  await expect(page.getByRole('button', { name: 'Save listing review' })).toBeVisible();
  await expect(page.getByRole('button', { name: 'Save vendor review setting' })).toBeVisible();
  await expect(page.locator('main')).toContainText('1 listing waits for review');
  await noScriptsNoOverflow(page);
});
