import { createPublicKey, verify } from 'node:crypto';
import { readFileSync } from 'node:fs';
import { expect, test } from '@playwright/test';
import { ADMIN, uniqueHandle } from './db-fixtures';
import { pgpFixture, RECIPIENT_FINGERPRINT, setRole, signedIn, submitStatus } from './db-helpers';

// Administrator forms submitted against a real database with JavaScript disabled. Each test signs the shared
// administrator in once (the per-handle sign-in limit is 10 per 10 minutes across all db specs).

test('promoting a buyer to vendor opens the vendor desk; the listing form shows validation errors', async ({ browser, baseURL }) => {
  const vendorHandle = uniqueHandle('promoted');
  const title = `Validation ${uniqueHandle('item')}`;
  const vendor = await signedIn(browser, baseURL, vendorHandle, 'browser-promoted-password-123', true);
  const vendorNav = vendor.getByRole('navigation', { name: 'Main navigation' });
  expect((await vendor.goto('/vendor-dashboard'))?.status()).toBe(403);
  await expect(vendorNav.getByRole('link', { name: 'Vendor desk' })).toHaveCount(0);

  const admin = await signedIn(browser, baseURL, ADMIN.handle, ADMIN.password, false);
  await admin.goto('/admin');
  await expect(admin.locator('option', { hasText: `${vendorHandle} ·` })).toHaveText(`${vendorHandle} · buyer`);
  await setRole(admin, vendorHandle, 'vendor');
  await expect(admin.getByRole('status')).toHaveText('Changes saved.');
  await admin.reload();
  await expect(admin.locator('option', { hasText: `${vendorHandle} ·` })).toHaveText(`${vendorHandle} · vendor`);
  await expect(admin.locator('table').last()).toContainText('Changed user role');
  // An administrator cannot be assigned from this form; the server refuses it with a reason.
  const form = admin.locator('form', { has: admin.getByRole('button', { name: 'Update role' }) });
  await form.getByLabel('Account').selectOption(await form.locator('option', { hasText: `${vendorHandle} ·` }).getAttribute('value'));
  await form.getByLabel('Role').selectOption('admin');
  expect(await submitStatus(admin, () => form.getByRole('button', { name: 'Update role' }).click())).toBe(400);
  await expect(admin.locator('body')).toHaveText('Choose buyer, vendor, or moderator');
  await admin.goto('/admin');
  await expect(admin.locator('option', { hasText: `${vendorHandle} ·` })).toHaveText(`${vendorHandle} · vendor`);
  await admin.context().close();

  // The promotion takes effect on the vendor's existing session.
  await vendor.goto('/account');
  await expect(vendor.locator('.page-head .badge')).toHaveText('vendor');
  await vendorNav.getByRole('link', { name: 'Vendor desk' }).click();
  await expect(vendor).toHaveURL(/\/vendor-dashboard$/);

  const publish = async (fields: { title: string; btc: string; kind?: string; delivery?: string }) => {
    await vendor.goto('/vendor-dashboard');
    await vendor.getByText('+ Publish a new listing', { exact: true }).click();
    await vendor.getByLabel('Listing title').fill(fields.title);
    await vendor.getByLabel('Description', { exact: true }).fill('Listing validation regression.');
    await vendor.getByLabel('Fulfillment type').selectOption(fields.kind ?? 'physical');
    await vendor.getByLabel('Bitcoin reference price').fill(fields.btc);
    await vendor.getByLabel('Monero reference price').fill('0.5');
    if (fields.delivery) await vendor.getByLabel('Automatic delivery content').fill(fields.delivery);
    return submitStatus(vendor, () => vendor.getByRole('button', { name: 'Publish listing ↗' }).click());
  };
  const invalid = 'Invalid listing. Check price precision, stock, category, and required fields.';
  // Nine decimal places for BTC (satoshi precision is eight).
  expect(await publish({ title, btc: '0.123456789' })).toBe(400);
  await expect(vendor.locator('body')).toHaveText(invalid);
  // Whitespace passes the browser's minlength but not the server's trimmed title check.
  expect(await publish({ title: '     ', btc: '0.001' })).toBe(400);
  await expect(vendor.locator('body')).toHaveText(invalid);
  expect(await publish({ title, btc: '0.001', delivery: 'secret-download-link' })).toBe(400);
  await expect(vendor.locator('body')).toHaveText('Automatic delivery content is only available for digital listings.');
  await vendor.goto('/vendor-dashboard');
  await expect(vendor.getByText('You have not published any listings yet.')).toBeVisible();

  expect(await publish({ title, btc: '0.001' })).toBe(303);
  await expect(vendor.locator('.listings-grid')).toContainText(title);

  // Editing with a stale stock value is refused rather than overwriting a change made meanwhile.
  await vendor.getByRole('link', { name: `Edit ${title}` }).click();
  const editURL = vendor.url();
  const second = await vendor.context().newPage();
  await second.goto(editURL);
  await second.getByLabel('Stock').fill('5');
  await second.getByRole('button', { name: 'Save changes' }).click();
  await expect(second.getByRole('status')).toHaveText('Changes saved.');
  await vendor.getByLabel('Stock').fill('2');
  expect(await submitStatus(vendor, () => vendor.getByRole('button', { name: 'Save changes' }).click())).toBe(409);
  await expect(vendor.locator('body')).toHaveText('Stock changed to 5 while you were editing (orders reserve stock). Reload and try again.');
  await vendor.goto(editURL);
  await expect(vendor.getByLabel('Stock')).toHaveValue('5');
  await vendor.getByLabel('Monero reference price').fill('0.1234567890123');
  expect(await submitStatus(vendor, () => vendor.getByRole('button', { name: 'Save changes' }).click())).toBe(400);
  await expect(vendor.locator('body')).toHaveText(invalid);
  await vendor.goto(editURL);
  await expect(vendor.getByLabel('Monero reference price')).toHaveValue(/^0\.5/);
  await vendor.context().close();
});

test('site settings, operator key, canary signature checks and audit export', async ({ browser, baseURL }) => {
  const admin = await signedIn(browser, baseURL, ADMIN.handle, ADMIN.password, false);
  await admin.goto('/admin');
  const siteName = admin.getByLabel('Site name');
  const original = await siteName.inputValue();
  const renamed = `E2E ${uniqueHandle('market')}`.slice(0, 40);
  const saveSettings = () => admin.getByRole('button', { name: 'Save settings' }).click();

  await siteName.fill(renamed);
  expect(await submitStatus(admin, saveSettings)).toBe(303);
  await expect(admin).toHaveURL(/\/admin\?saved=1$/);
  await admin.reload();
  await expect(siteName).toHaveValue(renamed);
  await expect(admin.locator('table').last()).toContainText('Saved marketplace settings');
  await siteName.fill('    ');
  expect(await submitStatus(admin, saveSettings)).toBe(400);
  await expect(admin.locator('body')).toHaveText('Invalid settings');
  await admin.goto('/admin');
  await expect(siteName).toHaveValue(renamed);
  await siteName.fill(original);
  expect(await submitStatus(admin, saveSettings)).toBe(303);
  await expect(siteName).toHaveValue(original);

  // Operator key: a malformed key is refused; the fixture key is stored and its fingerprint shown.
  const keyPanel = admin.getByRole('region', { name: 'Operator PGP key' });
  const canaryPanel = admin.getByRole('region', { name: 'Warrant canary' });
  await expect(canaryPanel).toContainText('No signed canary published');
  await keyPanel.getByLabel('Armored public key').fill('-----BEGIN PGP PUBLIC KEY BLOCK-----\n\nbm90IGEga2V5\n-----END PGP PUBLIC KEY BLOCK-----');
  expect(await submitStatus(admin, () => keyPanel.getByRole('button', { name: 'Save operator key' }).click())).toBe(400);
  await expect(admin.locator('body')).toContainText('Operator key rejected:');
  await admin.goto('/admin');
  await keyPanel.getByLabel('Armored public key').fill(pgpFixture('recipient.pub.asc'));
  expect(await submitStatus(admin, () => keyPanel.getByRole('button', { name: 'Save operator key' }).click())).toBe(303);
  await expect(admin).toHaveURL(/\/admin\?saved=1#transparency$/);
  await admin.reload();
  await expect(keyPanel.locator('.badge')).toHaveText('Configured');
  await expect(keyPanel.locator('.fingerprint')).toHaveText(RECIPIENT_FINGERPRINT);

  // A statement signed by a different key is rejected and nothing is published.
  await canaryPanel.getByLabel('Clearsigned statement').fill(pgpFixture('canary.other-key.asc'));
  expect(await submitStatus(admin, () => canaryPanel.getByRole('button', { name: 'Publish canary' }).click())).toBe(400);
  await expect(admin.locator('body')).toHaveText('Canary rejected: signature does not verify with the configured key');
  await admin.goto('/canary');
  await expect(admin.locator('section.canary .badge')).toHaveText('No signed canary published');
  // The same statement signed by the operator key is published and verified on the public page.
  await admin.goto('/admin');
  await canaryPanel.getByLabel('Clearsigned statement').fill(pgpFixture('canary.recipient-key.asc'));
  expect(await submitStatus(admin, () => canaryPanel.getByRole('button', { name: 'Publish canary' }).click())).toBe(303);
  await expect(admin).toHaveURL(/\/canary$/);
  const canary = admin.locator('section.canary');
  await expect(canary.locator('.badge')).toHaveText('Signature verified');
  await expect(canary.locator('.canary-text')).toContainText('E2E FIXTURE - not a real warrant canary.');
  await expect(canary.locator('dd.fingerprint')).toHaveText(RECIPIENT_FINGERPRINT);

  // Signed audit export. The e2e server inherits the runner's environment and playwright.config.ts sets no
  // AUDIT_SIGNING_KEY, so the default run asserts the unavailable state; with a key exported, a real download.
  await admin.goto('/admin');
  const audit = admin.getByRole('region', { name: 'Signed audit export' });
  const key = process.env.AUDIT_SIGNING_KEY?.trim() ?? '';
  if (!key) {
    await expect(audit.locator('.badge')).toHaveText('Unavailable');
    await expect(audit).toContainText('AUDIT_SIGNING_KEY is not configured; signed audit export is unavailable.');
    await expect(audit.getByRole('button')).toHaveCount(0);
    const refused = await admin.goto('/admin/audit-export?upto=1');
    expect(refused?.status()).toBe(409);
    await expect(admin.locator('body')).toHaveText('AUDIT_SIGNING_KEY is not configured; signed audit export is unavailable. Set a 64-hex-character AUDIT_SIGNING_KEY and restart the application.');
  } else {
    expect(key).toMatch(/^[0-9a-fA-F]{64}$/);
    await expect(audit.locator('.badge')).toHaveText('Available');
    const publicKey = (await audit.locator('.fingerprint').innerText()).trim();
    const upto = await audit.getByLabel('Include events up to id').inputValue();
    const [download] = await Promise.all([
      admin.waitForEvent('download'),
      audit.getByRole('button', { name: 'Download export (.jsonl)' }).click(),
    ]);
    expect(download.suggestedFilename()).toBe(`audit-events-upto-${upto}.jsonl`);
    const exported = readFileSync(await download.path());
    const lines = exported.toString('utf8').trim().split('\n').map(l => JSON.parse(l));
    expect(lines.at(-1).id).toBe(Number(upto));
    expect(lines.map(l => l.action)).toContain(`Set operator PGP key ${RECIPIENT_FINGERPRINT.replace(/ /g, '')} for canary verification`);
    const response = await admin.request.get(`/admin/audit-export?upto=${upto}`);
    expect(response.headers()['content-type']).toBe('application/x-ndjson');
    expect(response.headers()['content-disposition']).toBe(`attachment; filename="audit-events-upto-${upto}.jsonl"`);
    expect(Buffer.compare(await response.body(), exported)).toBe(0);
    const sig = await admin.request.get(`/admin/audit-export?upto=${upto}&sig=1`);
    expect(sig.headers()['content-type']).toBe('text/plain; charset=utf-8');
    const fields = Object.fromEntries((await sig.text()).trim().split('\n').map(l => l.split(': ')));
    expect(fields.public_key).toBe(publicKey);
    const spki = createPublicKey({ key: Buffer.concat([Buffer.from('302a300506032b6570032100', 'hex'), Buffer.from(publicKey, 'hex')]), format: 'der', type: 'spki' });
    expect(verify(null, exported, spki, Buffer.from(fields.signature, 'hex'))).toBe(true);
  }
  await admin.context().close();
});
