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

// A-552: the separated topology, where the upload gateway reports its scanner state to the app. ?scanner=gateway is
// a sample healthy report and ?scanner=gateway-unknown a report older than the staleness limit; ?scanner=gateway on
// the upload pages is the paused state without a fresh report. Layout and copy only: TestGatewayScannerReport
// proves the report, its mutual-TLS identity and the staleness rule against PostgreSQL.
for (const width of [320, 1280]) {
  test(`gateway scanner report panel and paused upload forms at ${width} px`, async ({ page }) => {
    await page.setViewportSize({ width, height: 900 });
    await page.goto('/admin/system?scanner=gateway');
    let panel = page.getByRole('region', { name: 'Image scanner' });
    await expect(panel.locator('strong').first()).toHaveText('Ready');
    await expect(panel).toContainText('reports the scanner’s state to this app over its private connection');
    await expect(panel).toContainText('Signatures: ClamAV 1.4.3, daily database 27999 built 2026-01-01 07:57 UTC, 16 days old (limit 30).');
    await expect(panel).toContainText('Last checked by the gateway: 2026-01-17 09:00 UTC. Last successful scan: 2026-01-17 08:41 UTC. Report received: 2026-01-17 09:00 UTC.');
    await expect(panel).toContainText('1 refused as infected · 0 refused because the scanner failed.');
    await expect(panel.getByRole('table')).toHaveCount(0);
    expect(await page.evaluate(() => document.documentElement.scrollWidth)).toBeLessThanOrEqual(width);

    await page.goto('/admin/system?scanner=gateway-unknown');
    panel = page.getByRole('region', { name: 'Image scanner' });
    await expect(panel.locator('strong').first()).toHaveText('Scanner status unknown — gateway not reporting');
    await expect(panel.getByRole('status')).toContainText('Last report received: 2026-01-17 08:40 UTC.');
    await expect(panel).not.toContainText('Last 24 hours');
    expect(await page.evaluate(() => document.documentElement.scrollWidth)).toBeLessThanOrEqual(width);

    for (const [path, name, label, button] of [
      ['/listing-edit?id=encrypted-drive&scanner=gateway', 'Photos', 'Photo', 'Upload photo'],
      ['/account?scanner=gateway', 'Avatar', 'Avatar', 'Upload avatar'],
    ]) {
      await page.goto(path);
      const uploads = page.getByRole('region', { name });
      const notice = uploads.getByText(/^Uploads paused: the virus scanner is unavailable\./);
      await expect(notice).toBeVisible();
      await expect(uploads.locator('fieldset')).toHaveAttribute('disabled', '');
      const file = uploads.getByLabel(label, { exact: true });
      await expect(file).toBeDisabled();
      await expect(uploads.getByRole('button', { name: button })).toBeDisabled();
      // The notice is read before the file input it disables.
      expect((await notice.boundingBox())!.y).toBeLessThan((await file.boundingBox())!.y);
      expect(await page.evaluate(() => document.documentElement.scrollWidth)).toBeLessThanOrEqual(width);
    }
    await expect(page.locator('script')).toHaveCount(0);
  });
}

// A-551: the private administrator connection with isolated uploads. ?scanner=private is the confirmation that
// authorizes one upload through the separate gateway (no file input, no multipart form); ?scanner=private-none is
// the same connection without a configured gateway origin, which says why instead of offering a form. Layout and
// copy only: TestStaffAvatarViaGatewayCapability and TestStaffListingPhotoViaGatewayCapability prove the capability
// against PostgreSQL and the mutual-TLS gateway.
for (const width of [320, 1280]) {
  test(`private-connection staff upload panels at ${width} px`, async ({ page }) => {
    await page.setViewportSize({ width, height: 900 });
    for (const [path, name, button] of [
      ['/listing-edit?id=encrypted-drive&scanner=private', 'Photos', 'Authorize photo upload'],
      ['/account?scanner=private', 'Avatar', 'Authorize avatar upload'],
    ]) {
      await page.goto(path);
      const uploads = page.getByRole('region', { name });
      await expect(uploads.locator('form[action="/admin/uploads/authorize"]')).toHaveCount(1);
      await expect(uploads.locator('form[enctype="multipart/form-data"]')).toHaveCount(0);
      await expect(uploads.locator('input[type="file"]')).toHaveCount(0);
      await expect(uploads.getByLabel('Current password')).toBeVisible();
      await expect(uploads).toContainText('This private connection does not take files.');
      const submit = uploads.getByRole('button', { name: button });
      await expect(submit).toBeEnabled();
      expect((await submit.boundingBox())!.height).toBeGreaterThanOrEqual(24);
      expect(await page.evaluate(() => document.documentElement.scrollWidth)).toBeLessThanOrEqual(width);

      await page.goto(path.replace('scanner=private', 'scanner=private-none'));
      const none = page.getByRole('region', { name });
      await expect(none).toContainText('Uploads from the private administrator connection are unavailable');
      await expect(none.locator('form.upload-form')).toHaveCount(0);
      await expect(none.locator('input[type="file"]')).toHaveCount(0);
      expect(await page.evaluate(() => document.documentElement.scrollWidth)).toBeLessThanOrEqual(width);
    }
    await expect(page.locator('script')).toHaveCount(0);
  });
}
