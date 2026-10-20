import { expect, test } from '@playwright/test';
import { PREVIEW_DISPUTED_ID } from './preview-routes';
import { waitForStyles } from './styles';

test('dispute image evidence form works without scripts and stays within the viewport', async ({ page }) => {
  await page.goto(`/order?id=${PREVIEW_DISPUTED_ID}`);
  await waitForStyles(page);
  const panel = page.locator('#dispute-evidence');
  await expect(panel.getByRole('heading', { name: 'Dispute image evidence', exact: true })).toBeVisible();
  await expect(panel.getByText('No image evidence has been added.', { exact: true })).toBeVisible();
  const form = panel.locator('form');
  await expect(form).toHaveAttribute('method', 'post');
  await expect(form).toHaveAttribute('action', '/orders/evidence');
  await expect(form).toHaveAttribute('enctype', 'multipart/form-data');
  const input = panel.getByLabel('Evidence image', { exact: true });
  await expect(input).toHaveAttribute('accept', 'image/jpeg,image/png');
  await expect(input).toHaveAttribute('aria-describedby', 'evidence-help evidence-privacy');
  await expect(form.locator('input').first()).toHaveAttribute('name', 'csrf');
  await expect(panel).toContainText('PDF, Office documents and SVG are refused.');
  await expect(panel).toContainText('Every image is scanned by ClamAV');
  await expect(panel).toContainText('Stored unencrypted');
  await expect(page.locator('script')).toHaveCount(0);
  expect(await page.evaluate(() => document.documentElement.scrollWidth)).toBeLessThanOrEqual(page.viewportSize()!.width);
  // Long descriptions and 200% text must not grow the document sideways.
  await page.evaluate(() => document.documentElement.style.setProperty('font-size', '200%', 'important'));
  expect(await page.evaluate(() => document.documentElement.scrollWidth)).toBeLessThanOrEqual(page.viewportSize()!.width);
});
