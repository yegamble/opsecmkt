import { expect, test } from '@playwright/test';

// A-185 image scanner panel in the read-only preview: a labelled sample of the Ready state past half the
// signature age limit (the preview has no scanner). Proves layout and semantics only; db-admin.spec.ts reads a
// real app's cached state from a fake clamd.

test('admin image scanner panel: state, signature age warning and counts by signature', async ({ page }) => {
  await page.goto('/admin/system');
  const panel = page.getByRole('region', { name: 'Image scanner' });
  await expect(panel.getByRole('heading', { name: 'Image scanner', level: 2 })).toBeVisible();
  await expect(panel.locator('strong').first()).toHaveText('Ready');
  await expect(panel).toContainText('Signatures: ClamAV 1.4.3, daily database 27999 built 2026-01-01 07:57 UTC, 16 days old (limit 30).');
  await expect(panel.getByRole('status')).toContainText('./scripts/install.sh refresh-scanner');
  await expect(panel).toContainText('it does not review picture content');
  await expect(panel).toContainText('3 stored · 1 refused as infected · 0 refused because the scanner failed.');
  // A-186: stored photos and avatars against IMAGE_STORAGE_LIMIT_MB.
  await expect(panel).toContainText('Stored photos and avatars: 3, using 0.4 MB of the 2048 MB limit (IMAGE_STORAGE_LIMIT_MB).');
  const table = panel.getByRole('table');
  await expect(table.getByRole('columnheader')).toHaveText(['Signature', 'Uploads refused']);
  await expect(table.getByRole('row').nth(1).getByRole('cell')).toHaveText(['Sample.Signature.UNOFFICIAL', '1']);
  await expect(panel).not.toContainText('Handle');
  await expect(page.locator('script')).toHaveCount(0);
  expect(await page.evaluate(() => document.documentElement.scrollWidth)).toBeLessThanOrEqual(page.viewportSize()!.width);
});
