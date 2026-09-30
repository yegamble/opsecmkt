import { expect, test, type Page } from '@playwright/test';
import { waitForStyles } from './styles';

// P6 Transparency in the read-only preview. The preview canary is a labelled sample signed by a throwaway key
// generated at preview start and checked by the real verifier; signed audit export is visibly unavailable.

async function noScriptsNoOverflow(page: Page) {
  await expect(page.locator('script')).toHaveCount(0);
  await waitForStyles(page);
  expect(await page.evaluate(() => document.documentElement.scrollWidth)).toBeLessThanOrEqual(page.viewportSize()!.width);
}

test('canary page names no staff contact the preview does not have (A-119)', async ({ page }) => {
  await page.goto('/canary');
  const contact = page.getByRole('region', { name: 'Staff contact' });
  await expect(contact.locator('.staff-contact')).toHaveText('Ask the operator of this market.');
  await expect(contact.locator('a')).toHaveCount(0);
  await noScriptsNoOverflow(page);
});

test('canary page shows one verified, clearly labelled sample statement', async ({ page }) => {
  const response = await page.goto('/canary');
  expect(response?.status()).toBe(200);
  const canary = page.locator('section.canary');
  await expect(canary).toHaveCount(1);
  await expect(canary.getByRole('heading', { name: 'Warrant canary' })).toBeVisible();
  await expect(canary.locator('.badge')).toHaveText('Signature verified');
  await expect(canary).not.toContainText('No signed canary published');
  await expect(canary).not.toContainText('INVALID');
  await expect(canary.locator('.canary-text')).toContainText('PREVIEW SAMPLE - not a real warrant canary.');
  await expect(canary.locator('dd.fingerprint')).toHaveText(/^([0-9A-F]{4} ){9}[0-9A-F]{4}$/);
  await expect(canary.getByText('Signature created')).toBeVisible();
  await expect(canary.getByText('Posted to this site')).toBeVisible();

  const signedMessage = canary.locator('details', { hasText: 'Signed message as posted' });
  await expect(signedMessage.locator('pre')).toBeHidden();
  await signedMessage.locator('summary').click();
  await expect(signedMessage.locator('pre')).toContainText('-----BEGIN PGP SIGNED MESSAGE-----');
  await expect(signedMessage.locator('pre')).toContainText('-----END PGP SIGNATURE-----');
  const key = canary.locator('details', { hasText: 'Operator public key' });
  await key.locator('summary').click();
  await expect(key.locator('pre')).toContainText('-----BEGIN PGP PUBLIC KEY BLOCK-----');

  const audit = page.locator('section.audit-key');
  await expect(audit.locator('.badge')).toHaveText('Signed exports unavailable');
  await expect(audit).toContainText('AUDIT_SIGNING_KEY');
  await noScriptsNoOverflow(page);
});

// A-189, owner decision 1(a): the public staff list is off by default, so /canary lists no staff and shows no
// avatar; the admin page offers the password-confirmed switch.
test('canary shows no staff list by default; the admin switch is off', async ({ page }) => {
  await page.goto('/canary');
  await expect(page.locator('#staff')).toHaveCount(0);
  await expect(page.getByRole('heading', { name: 'Market staff' })).toHaveCount(0);
  await expect(page.locator('main img')).toHaveCount(0);
  await page.goto('/admin');
  const panel = page.locator('#staff-list');
  await expect(panel).toContainText('Show a public staff list: Off');
  const form = panel.locator('form[method="post"][action="/admin/staff-list"]');
  await expect(form.locator('input[name="staff_list_public"]')).toHaveValue('true');
  await expect(form.getByLabel('Current password')).toHaveAttribute('type', 'password');
  await expect(form.getByRole('button', { name: 'Turn the public staff list on' })).toBeVisible();
  await noScriptsNoOverflow(page);
});

test('admin transparency panels are server-rendered forms and export is visibly unavailable', async ({ page }) => {
  await page.goto('/admin');
  await expect(page.getByRole('heading', { name: 'Transparency', level: 2 })).toBeVisible();
  const keyPanel = page.locator('section', { has: page.getByRole('heading', { name: 'Operator PGP key' }) });
  await expect(keyPanel.locator('.fingerprint')).toHaveText(/^([0-9A-F]{4} ){9}[0-9A-F]{4}$/);
  await expect(keyPanel.getByLabel('Armored public key')).toHaveValue(/BEGIN PGP PUBLIC KEY BLOCK/);
  const canaryPanel = page.locator('section', { has: page.getByRole('heading', { name: 'Warrant canary' }) });
  await expect(canaryPanel.getByText('Signature verified')).toBeVisible();
  await expect(canaryPanel.getByLabel('Clearsigned statement')).toBeVisible();
  const exportPanel = page.locator('section', { has: page.getByRole('heading', { name: 'Signed audit export' }) });
  await expect(exportPanel.locator('.badge')).toHaveText('Unavailable');
  await expect(exportPanel.getByRole('button')).toHaveCount(0);
  await noScriptsNoOverflow(page);

  await canaryPanel.getByLabel('Clearsigned statement').fill('-----BEGIN PGP SIGNED MESSAGE-----');
  await canaryPanel.getByRole('button', { name: 'Publish canary' }).click();
  await expect(page.locator('body')).toContainText('Read-only preview');
});

test('audit export download is refused in the preview', async ({ request }) => {
  const response = await request.get('/admin/audit-export?upto=1');
  expect(response.status()).toBe(403);
  expect(await response.text()).toContain('Read-only preview');
});
