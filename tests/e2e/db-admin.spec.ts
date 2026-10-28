import { execFileSync } from 'node:child_process';
import { createPublicKey, randomBytes, verify } from 'node:crypto';
import { readFileSync } from 'node:fs';
import { expect, test } from '@playwright/test';
import { ADMIN, uniqueHandle } from './db-fixtures';
import { enrollTOTP, expectNotice, pgpFixture, RECIPIENT_FINGERPRINT, seedPGPAccess, seedRecordedMoneroEscrow, setRole, signedIn, submitStatus, withinViewport } from './db-helpers';
import { waitForStyles } from './styles';

// Administrator forms submitted against a real database with JavaScript disabled. Each test signs the shared
// administrator in once (the per-handle sign-in limit, 10 failed passwords per 10 minutes, no longer counts
// correct sign-ins since A-153, but one sign-in per test keeps the specs independent of it).

test('promoting a buyer to vendor opens the vendor desk; the listing form shows validation errors', async ({ browser, baseURL }) => {
  const vendorHandle = uniqueHandle('promoted');
  const title = `Validation ${uniqueHandle('item')}`;
  const vendor = await signedIn(browser, baseURL, vendorHandle, 'browser-promoted-password-123', true);
  const vendorNav = vendor.getByRole('navigation', { name: 'Main navigation' });
  expect((await vendor.goto('/vendor-dashboard'))?.status()).toBe(403);
  await expect(vendorNav.getByRole('link', { name: 'Vendor desk' })).toHaveCount(0);
  // A fresh account that has TOTP on, for the second-factor reset below (it reuses this test's admin sign-in).
  const lockedHandle = uniqueHandle('lostfactor');
  const lockedPassword = 'browser-lost-factor-123';
  const locked = await signedIn(browser, baseURL, lockedHandle, lockedPassword, true);
  await enrollTOTP(locked, lockedPassword);

  const admin = await signedIn(browser, baseURL, ADMIN.handle, ADMIN.password, false);
  // A-545: the user directory (which replaced the role form's "Newest accounts" list) shows the account's role.
  const directoryRole = async () => {
    await admin.goto(`/admin/users?find=${vendorHandle}`);
    return admin.getByRole('region', { name: 'User directory' }).first().locator('tbody tr', { hasText: vendorHandle }).locator('td').nth(1);
  };
  await expect(await directoryRole()).toHaveText('buyer');
  await admin.goto('/admin/users');
  const roleSection = admin.getByRole('region', { name: 'Assign an account role' });
  const roleForm = roleSection.locator('form');
  // The form takes a handle looked up on the server. An unknown one answers 404 with the page again: the
  // reason in an alert, the handle and role kept for correction, nothing changed.
  const unknown = uniqueHandle('nobody');
  await roleForm.getByLabel('Account handle').fill(unknown);
  await roleForm.getByLabel('Role').selectOption('vendor');
  await roleForm.getByLabel('Current password').fill(ADMIN.password);
  expect(await submitStatus(admin, () => roleForm.getByRole('button', { name: 'Update role' }).click())).toBe(404);
  await expect(admin.getByRole('alert')).toHaveText(`No buyer, vendor or moderator has the handle "${unknown}". Check the spelling: handles are case-sensitive, and administrator roles cannot be changed here.`);
  await expect(roleForm.getByLabel('Account handle')).toHaveValue(unknown);
  await expect(roleForm.getByLabel('Role')).toHaveValue('vendor');
  // The administrator's own role is fixed.
  await roleForm.getByLabel('Account handle').fill(ADMIN.handle);
  await roleForm.getByLabel('Current password').fill(ADMIN.password);
  expect(await submitStatus(admin, () => roleForm.getByRole('button', { name: 'Update role' }).click())).toBe(400);
  await expect(admin.getByRole('alert')).toHaveText('Cannot change your own administrator role');
  await setRole(admin, vendorHandle, 'vendor');
  await admin.waitForLoadState();
  await expect(await directoryRole()).toHaveText('vendor');
  await admin.goto('/admin/users');
  // Only roles the server can assign are offered; administrator remains setup-only.
  await expect(roleForm.getByLabel('Role').locator('option[value="admin"]')).toHaveCount(0);
  // The administrator's row names the account and both roles; the account gets its own row, and the
  // Account column shows whose row each is. The audit trail is on System & audit (A-542), its last table.
  await admin.goto('/admin/system');
  const auditRows = admin.locator('table').last().locator('tbody tr');
  await expect(auditRows.filter({ hasText: `Changed role of ${vendorHandle} from buyer to vendor` }).locator('td').first()).toHaveText(ADMIN.handle);
  await expect(auditRows.filter({ hasText: `Role changed from buyer to vendor by administrator ${ADMIN.handle}` }).filter({ hasText: vendorHandle }).locator('td').first()).toHaveText(vendorHandle);
  await admin.goto('/admin/users');

  // Second-factor reset by handle: the helper list shows accounts with a factor, never the administrator; an
  // unknown handle answers 404 with the handle kept; it needs the administrator's own password; it ends the
  // account's sessions and records the reset on both accounts.
  const reset = admin.getByRole('region', { name: "Reset a user's second factors" });
  const factorList = reset.locator('.account-list li');
  await expect(factorList.filter({ hasText: `${lockedHandle} ·` })).toHaveText(`${lockedHandle} · TOTP`);
  await expect(factorList.filter({ hasText: `${vendorHandle} ·` })).toHaveCount(0);
  await expect(factorList.filter({ hasText: `${ADMIN.handle} ·` })).toHaveCount(0);
  const submitReset = async (handle: string, password: string) => {
    await admin.goto('/admin/users');
    await reset.getByLabel('Account handle').fill(handle);
    await reset.getByLabel('Current password').fill(password);
    return submitStatus(admin, () => reset.getByRole('button', { name: 'Reset second factors' }).click());
  };
  expect(await submitReset(unknown, ADMIN.password)).toBe(404);
  await expect(admin.getByRole('alert')).toHaveText(`No account has the handle "${unknown}". Check the spelling: handles are case-sensitive.`);
  await expect(reset.getByLabel('Account handle')).toHaveValue(unknown);
  await expect(reset.getByLabel('Current password')).toHaveValue('');
  expect(await submitReset(lockedHandle, 'not-the-admin-password')).toBe(401);
  await expect(admin.getByRole('alert')).toHaveText('Password incorrect');
  await locked.goto('/account');
  await expect(locked.locator('.key-values div', { hasText: 'TOTP authentication' }).locator('dd')).toHaveText('Enabled');
  expect(await submitReset(lockedHandle, ADMIN.password)).toBe(303);
  await expect(admin).toHaveURL(/\/admin\/users#reset-factors$/);
  await expect(reset.getByRole('status')).toHaveText('Second factors reset.');
  await expect(factorList.filter({ hasText: `${lockedHandle} ·` })).toHaveCount(0);
  await waitForStyles(admin);
  expect(await admin.evaluate(() => document.documentElement.scrollWidth)).toBeLessThanOrEqual(admin.viewportSize()!.width);
  await admin.goto('/admin/system');
  const resetRows = admin.locator('table').last().locator('tbody tr');
  await expect(resetRows.filter({ hasText: `Reset second factors of ${lockedHandle}: TOTP and recovery codes turned off; ended 1 session(s) (confirmed with password)` }).locator('td').first()).toHaveText(ADMIN.handle);
  await expect(resetRows.filter({ hasText: `Second factors reset by administrator ${ADMIN.handle}: TOTP and recovery codes turned off; ended 1 session(s)` }).locator('td').first()).toHaveText(lockedHandle);
  await waitForStyles(admin);
  expect(await admin.evaluate(() => document.documentElement.scrollWidth)).toBeLessThanOrEqual(admin.viewportSize()!.width);
  await admin.context().close();

  // The account was signed out, signs in with the password alone and is told what happened.
  await locked.goto('/account');
  await expect(locked).toHaveURL(/\/login\?next=%2Faccount$/);
  await locked.getByLabel('Handle', { exact: true }).fill(lockedHandle);
  await locked.getByLabel('Password', { exact: true }).fill(lockedPassword);
  await locked.getByRole('button', { name: 'Sign in' }).click();
  await expect(locked.locator('.account-name')).toHaveText(lockedHandle);
  await expect(locked).not.toHaveURL(/\/challenge$/);
  await locked.goto('/notifications');
  await expect(locked.locator('main')).toContainText('An administrator reset your two-factor sign-in (TOTP and recovery codes turned off)');
  await locked.goto('/account');
  await expect(locked.locator('.key-values div', { hasText: 'TOTP authentication' }).locator('dd')).toHaveText('Not enrolled');
  await locked.context().close();

  // The promotion takes effect on the vendor's existing session.
  await vendor.goto('/account');
  await expect(vendor.locator('.page-head .badge')).toHaveText('vendor');
  await vendorNav.getByRole('link', { name: 'Vendor desk' }).click();
  await expect(vendor).toHaveURL(/\/vendor-dashboard$/);

  const publish = async (fields: { title: string; xmr: string; kind?: string; delivery?: string }) => {
    await vendor.goto('/vendor-dashboard');
    await vendor.getByText('+ Publish a new listing', { exact: true }).click();
    await vendor.getByLabel('Listing title').fill(fields.title);
    await vendor.getByLabel('Description', { exact: true }).fill('Listing validation regression.');
    await vendor.getByLabel('Fulfillment type').selectOption(fields.kind ?? 'physical');
    await vendor.getByLabel('Price (XMR)').fill(fields.xmr);
    if (fields.delivery) await vendor.getByLabel('Reusable download content').fill(fields.delivery);
    return submitStatus(vendor, () => vendor.getByRole('button', { name: 'Publish listing ↗' }).click());
  };
  // Thirteen decimal places exceed Monero precision. The refused form comes back with the typed text and the
  // problem next to its field (A-514, A-59).
  expect(await publish({ title, xmr: '0.1234567890123' })).toBe(400);
  await expect(vendor.getByRole('alert')).toHaveText('The listing was not published; nothing was saved. Monero price: use a positive amount with at most 12 decimal places.');
  await expect(vendor.getByLabel('Description', { exact: true })).toHaveValue('Listing validation regression.');
  await expect(vendor.getByLabel('Listing title', { exact: true })).toHaveValue(title);
  await expect(vendor.locator('#lf-price_xmr-error')).toHaveText('Monero price: use a positive amount with at most 12 decimal places.');
  // Whitespace passes the browser's minlength but not the server's trimmed title check.
  expect(await publish({ title: '     ', xmr: '0.5' })).toBe(400);
  await expect(vendor.locator('#lf-title-error')).toHaveText('Listing title: enter at least 3 characters.');
  // The reusable download content is a delivery secret: named, but never filled in again.
  expect(await publish({ title, xmr: '0.5', delivery: 'secret-download-link' })).toBe(400);
  await expect(vendor.locator('#lf-delivery_content-error')).toHaveText('Reusable download content: only a digital listing can have it. Clear it, or choose Digital as the fulfillment type.');
  await expect(vendor.getByLabel('Reusable download content')).toHaveValue('');
  await expect(vendor.locator('main')).not.toContainText('secret-download-link');
  await vendor.goto('/vendor-dashboard');
  await expect(vendor.getByText('You have not published any listings yet.')).toBeVisible();

  expect(await publish({ title, xmr: '0.5' })).toBe(303);
  await expect(vendor.getByLabel('Listing title')).toHaveValue(title); // A-188: publishing lands on its edit page
  await vendor.goto('/vendor-dashboard');
  await expect(vendor.locator('.listings-grid')).toContainText(title);

  // Editing with a stale stock value is refused rather than overwriting a change made meanwhile.
  await vendor.getByRole('link', { name: `Edit ${title}` }).click();
  await vendor.waitForURL(/\/listing-edit\?id=/);
  const editURL = vendor.url();
  const second = await vendor.context().newPage();
  await second.goto(editURL);
  await second.getByLabel('Stock').fill('5');
  await second.getByRole('button', { name: 'Save changes' }).click();
  await expectNotice(second, 'Listing saved.');
  await vendor.getByLabel('Stock').fill('2');
  expect(await submitStatus(vendor, () => vendor.getByRole('button', { name: 'Save changes' }).click())).toBe(409);
  await expect(vendor.getByRole('alert')).toHaveText('Stock changed to 5 while you were editing (orders reserve stock). Reload and try again.');
  await vendor.goto(editURL);
  await expect(vendor.getByLabel('Stock')).toHaveValue('5');
  await vendor.getByLabel('Price (XMR)').fill('0.1234567890123');
  expect(await submitStatus(vendor, () => vendor.getByRole('button', { name: 'Save changes' }).click())).toBe(400);
  await expect(vendor.getByRole('alert')).toHaveText('The changes were not saved. Monero price: use a positive amount with at most 12 decimal places.');
  await expect(vendor.getByLabel('Price (XMR)')).toHaveValue('0.1234567890123');
  await vendor.goto(editURL);
  await expect(vendor.getByLabel('Price (XMR)')).toHaveValue(/^0\.5/);
  await vendor.context().close();
});

test('site settings, account suspension, operator key, canary signature checks and audit export', async ({ browser, baseURL }) => {
  // A fresh account to suspend and restore (it reuses this test's administrator sign-in).
  const suspectHandle = uniqueHandle('suspect');
  const suspectPassword = 'browser-suspect-password-123';
  const suspect = await signedIn(browser, baseURL, suspectHandle, suspectPassword, true);
  const admin = await signedIn(browser, baseURL, ADMIN.handle, ADMIN.password, false);
  await admin.goto('/admin/site');
  const siteName = admin.getByLabel('Market name', { exact: true });
  const original = await siteName.inputValue();
  const renamed = `E2E ${uniqueHandle('market')}`.slice(0, 40);
  const saveSettings = () => admin.getByRole('button', { name: 'Save market name' }).click();

  await siteName.fill(renamed);
  expect(await submitStatus(admin, saveSettings)).toBe(303);
  await expect(admin).toHaveURL(/\/admin\/site$/);
  await expectNotice(admin, 'Marketplace settings saved.');
  await admin.waitForLoadState();
  await admin.reload();
  await expect(siteName).toHaveValue(renamed);
  await admin.goto('/admin/system');
  await expect(admin.locator('table').last()).toContainText(`Saved marketplace site name from ${JSON.stringify(original)} to ${JSON.stringify(renamed)}`);
  await admin.goto('/admin/site');
  await siteName.fill('    ');
  expect(await submitStatus(admin, saveSettings)).toBe(400);
  await expect(admin.getByRole('alert')).toHaveText('Invalid settings');
  await admin.goto('/admin/site');
  await expect(siteName).toHaveValue(renamed);
  await siteName.fill(original);
  expect(await submitStatus(admin, saveSettings)).toBe(303);
  await expect(siteName).toHaveValue(original);

  // Suspension by handle: an unknown handle answers 404 with the page again (handle and choice kept); it needs
  // the administrator's password; it signs the account out, blocks sign-in with the reason (only after a
  // correct password), lists the account for restoring and records the change on both accounts.
  const suspension = admin.getByRole('region', { name: 'Suspend or restore an account' });
  const submitSuspension = async (handle: string, action: 'Suspend' | 'Restore', password: string) => {
    await admin.goto('/admin/users');
    await suspension.getByLabel('Account handle').fill(handle);
    await suspension.getByRole('radio', { name: new RegExp(`^${action}:`) }).check();
    await suspension.getByLabel('Current password').fill(password);
    return submitStatus(admin, () => suspension.getByRole('button', { name: 'Suspend or restore' }).click());
  };
  const suspended = suspension.locator('.account-list li', { hasText: `${suspectHandle} ·` });
  await admin.goto('/admin/users');
  await expect(suspended).toHaveCount(0);
  const nobody = uniqueHandle('nobody');
  expect(await submitSuspension(nobody, 'Suspend', ADMIN.password)).toBe(404);
  await expect(admin.getByRole('alert')).toHaveText(`No account has the handle "${nobody}". Check the spelling: handles are case-sensitive.`);
  await expect(admin).toHaveTitle(/^Error: Admin: Users · /); // A-117, A-542
  await expect(suspension.getByLabel('Account handle')).toHaveValue(nobody);
  await expect(suspension.getByRole('radio', { name: /^Suspend:/ })).toBeChecked();
  expect(await submitSuspension(suspectHandle, 'Suspend', 'not-the-admin-password')).toBe(401);
  await expect(admin.getByRole('alert')).toHaveText('Password incorrect');
  await suspect.goto('/account');
  await expect(suspect.locator('.account-name')).toHaveText(suspectHandle);
  // A-117: the one-shot notice names the change inside the section the page returns to, in view at 320 px (and
  // at 1280 px for the restore below) once the stylesheet applies (A-228), and prefixes the title.
  const suspensionNotice = suspension.getByRole('status');
  await admin.setViewportSize({ width: 320, height: 900 });
  expect(await submitSuspension(suspectHandle, 'Suspend', ADMIN.password)).toBe(303);
  await expect(admin).toHaveURL(/\/admin\/users#suspend$/);
  await admin.waitForLoadState('load');
  await expect(suspensionNotice).toHaveText('Account suspended.');
  await expect(suspensionNotice).toBeInViewport({ ratio: 1 });
  await expect(admin).toHaveTitle(/^Account suspended\. – Admin: Users · /);
  expect(await admin.evaluate(() => document.documentElement.scrollWidth)).toBeLessThanOrEqual(320);
  await admin.setViewportSize({ width: 390, height: 900 });
  await expect(suspended).toHaveText(new RegExp(`^${suspectHandle} · buyer · suspended \\d{4}-\\d{2}-\\d{2} \\d{2}:\\d{2} UTC · factors: none \\(password only\\)$`));
  expect(await admin.evaluate(() => document.documentElement.scrollWidth)).toBeLessThanOrEqual(admin.viewportSize()!.width);
  await admin.goto('/admin/system');
  const suspendRows = admin.locator('table').last().locator('tbody tr');
  await expect(suspendRows.filter({ hasText: `Suspended account ${suspectHandle}; ended 1 session(s) and any pending sign-ins (confirmed with password)` }).locator('td').first()).toHaveText(ADMIN.handle);
  await expect(suspendRows.filter({ hasText: `Account suspended by administrator ${ADMIN.handle}; ended 1 session(s)` }).locator('td').first()).toHaveText(suspectHandle);
  expect(await admin.evaluate(() => document.documentElement.scrollWidth)).toBeLessThanOrEqual(admin.viewportSize()!.width);

  // The suspended account's open page is signed out; a wrong password is answered as for any account, the
  // right one with the reason.
  await suspect.goto('/account');
  await expect(suspect).toHaveURL(/\/login\?next=%2Faccount$/);
  const suspectSignIn = async (password: string) => {
    await suspect.goto('/login');
    await suspect.getByLabel('Handle', { exact: true }).fill(suspectHandle);
    await suspect.getByLabel('Password', { exact: true }).fill(password);
    return submitStatus(suspect, () => suspect.getByRole('button', { name: 'Sign in' }).click());
  };
  expect(await suspectSignIn('wrong-suspect-password-1')).toBe(401);
  await expect(suspect.getByRole('alert')).toHaveText('Invalid handle or password');
  expect(await suspectSignIn(suspectPassword)).toBe(403);
  // A-119: no staff contact is set in this suite at this point (db-staff-contact.spec.ts clears the one it sets). A-702:
  // a signed-out refusal says what to have ready and what staff never ask for.
  await expect(suspect.getByRole('alert')).toHaveText('This account is suspended. Ask the operator of this market. Have ready your account\'s handle, your PGP key fingerprint and, if your key expired or was lost, your renewed or replacement public key. Staff never ask for your password, recovery codes or private key.');

  // Restoring allows sign-in again; the account was told about both changes.
  await admin.setViewportSize({ width: 1280, height: 900 });
  expect(await submitSuspension(suspectHandle, 'Restore', ADMIN.password)).toBe(303);
  await admin.waitForLoadState('load');
  await expect(suspensionNotice).toHaveText('Account restored.');
  await expect(suspensionNotice).toBeInViewport({ ratio: 1 });
  await expect(admin).toHaveTitle(/^Account restored\. – Admin: Users · /);
  await admin.setViewportSize({ width: 390, height: 900 });
  await expect(suspended).toHaveCount(0);
  await admin.goto('/admin/system');
  await expect(suspendRows.filter({ hasText: `Restored account ${suspectHandle} (confirmed with password)` }).locator('td').first()).toHaveText(ADMIN.handle);
  expect(await suspectSignIn(suspectPassword)).toBe(303);
  await expect(suspect.locator('.account-name')).toHaveText(suspectHandle);
  await suspect.goto('/notifications');
  await expect(suspect.locator('main')).toContainText('An administrator suspended your account and signed you out everywhere.');
  await expect(suspect.locator('main')).toContainText('An administrator restored your account. You can sign in again.');
  await suspect.context().close();

  // Operator key: a malformed key is refused; the fixture key is stored and its fingerprint shown.
  const keyPanel = admin.getByRole('region', { name: 'Operator PGP key' });
  const canaryPanel = admin.getByRole('region', { name: 'Warrant canary' });
  await admin.goto('/admin/transparency');
  await expect(canaryPanel).toContainText('No signed canary published');
  await keyPanel.getByLabel('Current password').fill(ADMIN.password);
  await keyPanel.getByLabel('Armored public key').fill('-----BEGIN PGP PUBLIC KEY BLOCK-----\n\nbm90IGEga2V5\n-----END PGP PUBLIC KEY BLOCK-----');
  expect(await submitStatus(admin, () => keyPanel.getByRole('button', { name: 'Save operator key' }).click())).toBe(400);
  await expect(admin.locator('body')).toContainText('Operator key rejected:');
  await admin.goto('/admin/transparency');
  await keyPanel.getByLabel('Armored public key').fill(pgpFixture('recipient.pub.asc'));
  await keyPanel.getByLabel('Current password').fill(ADMIN.password);
  expect(await submitStatus(admin, () => keyPanel.getByRole('button', { name: 'Save operator key' }).click())).toBe(303);
  await expect(admin).toHaveURL(/\/admin\/transparency#transparency$/);
  await expectNotice(admin, 'Operator PGP key saved.');
  await admin.waitForLoadState();
  await admin.reload();
  await expect(keyPanel.locator('.badge')).toHaveText('Configured');
  await expect(keyPanel.locator('.fingerprint')).toHaveText(RECIPIENT_FINGERPRINT);

  // A statement signed by a different key is rejected and nothing is published.
  await canaryPanel.getByLabel('Current password').fill(ADMIN.password);
  await canaryPanel.getByLabel('Clearsigned statement').fill(pgpFixture('canary.other-key.asc'));
  expect(await submitStatus(admin, () => canaryPanel.getByRole('button', { name: 'Publish canary' }).click())).toBe(400);
  await expect(admin.getByRole('alert')).toHaveText('Canary rejected: signature does not verify with the configured key');
  await admin.goto('/canary');
  await expect(admin.locator('section.canary .badge')).toHaveText('No signed canary published');
  // The same statement signed by the operator key is published and verified on the public page.
  await admin.goto('/admin/transparency');
  await canaryPanel.getByLabel('Current password').fill(ADMIN.password);
  await canaryPanel.getByLabel('Clearsigned statement').fill(pgpFixture('canary.recipient-key.asc'));
  expect(await submitStatus(admin, () => canaryPanel.getByRole('button', { name: 'Publish canary' }).click())).toBe(303);
  await expect(admin).toHaveURL(/\/canary$/);
  const canary = admin.locator('section.canary');
  await expect(canary.locator('.badge')).toHaveText('Signature verified');
  await expect(canary.locator('.canary-text')).toContainText('E2E FIXTURE - not a real warrant canary.');
  await expect(canary.locator('dd.fingerprint')).toHaveText(RECIPIENT_FINGERPRINT);

  // Signed audit export. The e2e server inherits the runner's environment and playwright.config.ts sets no
  // AUDIT_SIGNING_KEY, so the default run asserts the unavailable state; with a key exported, a real download.
  await admin.goto('/admin/transparency');
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
    expect(lines.map(l => l.action)).toContain(`Set operator PGP key ${RECIPIENT_FINGERPRINT.replace(/ /g, '')} for canary verification (confirmed with password)`);
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

// A-185: the image scanner panel reads the app's cached clamd state. playwright.config.ts starts a loopback fake
// clamd (tests/e2e/fixtures/fake-clamd.py) whose daily signatures were built two days before the suite began.
test('admin sees scanner status and signature date', async ({ browser, baseURL }) => {
  const admin = await signedIn(browser, baseURL, ADMIN.handle, ADMIN.password, false);
  await admin.goto('/admin/system#image-scanner');
  const panel = admin.getByRole('region', { name: 'Image scanner' });
  await expect(panel.locator('strong').first()).toHaveText('Ready');
  await expect(panel).toContainText(/Signatures: ClamAV 1\.4\.3, daily database 27999 built \d{4}-\d{2}-\d{2} \d{2}:\d{2} UTC, 2 days old \(limit 30\)\./);
  const built = new Date(Date.now() - 2 * 86400_000).toISOString().slice(0, 10);
  const builtEarlier = new Date(Date.now() - 3 * 86400_000).toISOString().slice(0, 10); // the suite started before midnight UTC
  expect([built, builtEarlier]).toContain((await panel.textContent())!.match(/built (\d{4}-\d{2}-\d{2})/)![1]);
  await expect(panel).toContainText(/Last checked: \d{4}-\d{2}-\d{2} \d{2}:\d{2} UTC\./);
  await expect(panel).toContainText('stored · ');
  await expect(panel).toContainText('never linked to accounts');
  await expect(panel).toContainText(/Database size: \d+ [kMG]B\./);
  // A-186: stored photos and avatars against IMAGE_STORAGE_LIMIT_MB (default 2048).
  await expect(panel).toContainText(/Stored photos and avatars: \d+, using \d+\.\d MB of the 2048 MB limit \(IMAGE_STORAGE_LIMIT_MB\)\./);
  // Fresh signatures: no refresh warning and no paused notice.
  await expect(panel.getByRole('status')).toHaveCount(0);
  await expect(panel).not.toContainText('refresh-scanner');
  await expect(admin.locator('script')).toHaveCount(0);
  for (const width of [390, 320]) {
    await admin.setViewportSize({ width, height: 900 });
    expect(await admin.evaluate(() => document.documentElement.scrollWidth), `/admin/system at ${width}px`).toBeLessThanOrEqual(width);
  }
  await admin.context().close();
});

// A-546: a ban is permanent closure. The administrator bans a vendor from /admin/users with a reason and their
// password; the vendor's open page is signed out and its correct password is refused neutrally (never the reason);
// a reload lists it as banned with the reason. Unbanning, with a reason, lets it sign in again.
test('an administrator bans a vendor from the Users section and the vendor cannot sign in', async ({ browser, baseURL }) => {
  const vendorHandle = uniqueHandle('banned');
  const vendorPassword = 'browser-banned-password-123';
  const reason = 'Took payment for orders and shipped nothing.';
  const vendor = await signedIn(browser, baseURL, vendorHandle, vendorPassword, true);
  const admin = await signedIn(browser, baseURL, ADMIN.handle, ADMIN.password, false);
  await setRole(admin, vendorHandle, 'vendor');

  const panel = admin.getByRole('region', { name: 'Ban or unban an account' });
  const submitBan = async (action: 'Ban' | 'Unban', why: string) => {
    await admin.goto('/admin/users');
    await panel.getByLabel('Account handle').fill(vendorHandle);
    await panel.getByRole('radio', { name: new RegExp(`^${action}:`) }).check();
    await panel.getByLabel('Reason').fill(why);
    await panel.getByLabel('Current password').fill(ADMIN.password);
    return submitStatus(admin, () => panel.getByRole('button', { name: 'Ban or unban' }).click());
  };
  await admin.goto('/admin/users');
  await expect(panel).toContainText('A ban is permanent closure');
  const listed = panel.locator('.account-list li', { hasText: `${vendorHandle} ·` });
  await expect(listed).toHaveCount(0);

  expect(await submitBan('Ban', reason)).toBe(303);
  await expect(admin).toHaveURL(/\/admin\/users#ban$/);
  await expectNotice(admin, 'Account banned.');
  await admin.reload();
  await expect(listed).toHaveText(new RegExp(`^${vendorHandle} · vendor · banned \\d{4}-\\d{2}-\\d{2} \\d{2}:\\d{2} UTC by ${ADMIN.handle} · reason: ${reason.replace(/\./g, '\\.')}$`));
  await expect(admin.getByRole('region', { name: 'Suspend or restore an account' }).locator('.account-list li', { hasText: `${vendorHandle} ·` })).toHaveCount(0);

  await vendor.goto('/account');
  await expect(vendor).toHaveURL(/\/login\?next=%2Faccount$/);
  const vendorSignIn = async (password: string) => {
    await vendor.goto('/login');
    await vendor.getByLabel('Handle', { exact: true }).fill(vendorHandle);
    await vendor.getByLabel('Password', { exact: true }).fill(password);
    return submitStatus(vendor, () => vendor.getByRole('button', { name: 'Sign in' }).click());
  };
  expect(await vendorSignIn('wrong-banned-password-1')).toBe(401);
  await expect(vendor.getByRole('alert')).toHaveText('Invalid handle or password');
  expect(await vendorSignIn(vendorPassword)).toBe(403);
  await expect(vendor.getByRole('alert')).toHaveText(/^This account is closed and cannot sign in\. /);
  await expect(vendor.locator('main')).not.toContainText(reason);

  expect(await submitBan('Unban', 'Appeal upheld after review.')).toBe(303);
  await expectNotice(admin, 'Ban lifted.');
  await admin.reload();
  await expect(listed).toHaveCount(0);
  expect(await vendorSignIn(vendorPassword)).toBe(303);
  await expect(vendor.locator('.account-name')).toHaveText(vendorHandle);
  await vendor.context().close();
  await admin.context().close();
});

// A-652b: on a banned vendor's account page the administrator refunds its unpaid Monero escrow order with the separate
// form, with JavaScript disabled. No wallet runs: the escrow record is a format fixture (seedRecordedMoneroEscrow) and
// the order received nothing, so the refund just cancels it (settlements are covered by the Go simulated-RPC tests).
// A reload offers nothing more to refund and the buyer's order page shows it cancelled.
test('an administrator refunds a banned vendor\'s unpaid escrow order from its account page', async ({ browser, baseURL }) => {
  const sql = (query: string) => execFileSync('psql', [process.env.E2E_DATABASE_URL!, '-v', 'ON_ERROR_STOP=1', '-qtAc', query], { encoding: 'utf8' }).trim();
  const hex = (bytes: number) => randomBytes(bytes).toString('hex');
  const [vendorHandle, buyerHandle] = [uniqueHandle('refund_vendor'), uniqueHandle('refund_buyer')];
  const [productID, order] = [hex(16), hex(32)];
  const buyer = await signedIn(browser, baseURL, buyerHandle, 'browser-ban-refund-buyer-123', true);
  await seedPGPAccess(buyer);
  sql(`INSERT INTO users(id,handle,password_hash,role) VALUES ('${hex(16)}','${vendorHandle}','x','vendor');
    INSERT INTO products(id,vendor_id,title,description,category,region,kind,btc,xmr,stock)
      VALUES ('${productID}',(SELECT id FROM users WHERE handle='${vendorHandle}'),'Ban refund fixture','Seeded by db-admin.spec.ts','Other','Worldwide','physical',100000,500000000000,0);
    INSERT INTO orders(id,buyer_id,product_id,currency,amount,state)
      VALUES ('${order}',(SELECT id FROM users WHERE handle='${buyerHandle}'),'${productID}','XMR',500000000000,'awaiting_payment');`);
  seedRecordedMoneroEscrow(sql, order);
  const admin = await signedIn(browser, baseURL, ADMIN.handle, ADMIN.password, false);
  await admin.goto(`/admin/users?handle=${vendorHandle}`);
  await expect(admin.getByRole('region', { name: 'Orders of this banned account' })).toHaveCount(0);
  const ban = admin.getByRole('region', { name: 'Ban this account' });
  await ban.getByLabel('Reason').fill('Took payment for orders and shipped nothing.');
  await ban.getByLabel('Current password').fill(ADMIN.password);
  expect(await submitStatus(admin, () => ban.getByRole('button', { name: `Ban ${vendorHandle}` }).click())).toBe(303);

  const section = admin.getByRole('region', { name: 'Orders of this banned account' });
  await expect(section.getByRole('table', { name: 'What the refund takes now' }).locator('tr', { hasText: 'Awaiting payment, nothing received' })).toContainText('1');
  await expect(section.getByRole('table', { name: 'Open orders and escrows holding coins' })).toContainText(order.slice(0, 8));
  await section.locator('form').getByLabel('Current password').fill(ADMIN.password);
  expect(await submitStatus(admin, () => section.getByRole('button', { name: 'Cancel and refund 1 order(s)' }).click())).toBe(303);
  await expect(admin).toHaveURL(new RegExp(`/admin/users\\?handle=${vendorHandle}#ban-orders$`));
  await expect(section.getByRole('status')).toContainText('Orders cancelled.');
  await admin.reload();
  await expect(section).toContainText('No unpaid or unshipped order is left to refund.');
  await expect(section.locator('form')).toHaveCount(0);
  expect(sql(`SELECT state FROM orders WHERE id='${order}'`)).toBe('cancelled');
  await buyer.goto(`/order?id=${order}`);
  await expect(buyer.locator('main')).toContainText('Cancelled');
  await buyer.context().close();
  await admin.context().close();
});

// A-545: an administrator finds an account in the user directory (any part of the handle, any case, with a filter),
// opens its page, suspends it from there and, after a reload, sees it suspended; restoring returns there too. The
// page links to /messages for this account.
test('user directory: search, open an account, suspend and restore it from its page', async ({ browser, baseURL }) => {
  const handle = uniqueHandle('dirsusp');
  const account = await signedIn(browser, baseURL, handle, 'browser-directory-password-123', true);
  const admin = await signedIn(browser, baseURL, ADMIN.handle, ADMIN.password, false);
  await admin.goto('/admin/users');
  const search = admin.getByRole('search', { name: 'Search and filter accounts' });
  await search.getByLabel('Handle contains').fill(handle.toUpperCase());
  await search.locator('select[name="role"]').selectOption('buyer');
  await search.locator('select[name="status"]').selectOption('active');
  await search.getByRole('button', { name: 'Search' }).click();
  await expect(admin).toHaveURL(`/admin/users?find=${handle.toUpperCase()}&role=buyer&status=active&vendor=`);
  const directory = admin.getByRole('region', { name: 'User directory' }).first();
  await expect(directory.locator('tbody tr')).toHaveCount(1);
  await expect(directory.locator('tbody tr')).toContainText('Active');
  await expect(search.getByLabel('Handle contains')).toHaveValue(handle.toUpperCase());
  await directory.getByRole('link', { name: handle }).click();
  await expect(admin).toHaveURL(`/admin/users?handle=${handle}`);
  await expect(admin.getByRole('heading', { level: 2, name: handle })).toBeVisible();
  await expect(admin.getByRole('link', { name: /Message user/ })).toHaveAttribute('href', `/messages?to=${handle}`);
  const status = admin.locator('.key-values div').filter({ has: admin.locator('dt', { hasText: /^Status$/ }) }).locator('dd');
  await expect(status).toHaveText('Active');

  const suspend = admin.getByRole('region', { name: 'Suspend this account' });
  await suspend.getByLabel('Current password').fill(ADMIN.password);
  await suspend.getByRole('button', { name: `Suspend ${handle}` }).click();
  await expectNotice(admin, 'Account suspended.');
  await expect(admin).toHaveURL(`/admin/users?handle=${handle}#suspend`);
  await expect(admin.getByRole('region', { name: 'Restore this account' }).getByRole('status')).toHaveText('Account suspended.');
  await admin.reload();
  await expect(status).toHaveText('Suspended');
  await expect(admin.locator('.key-values div').filter({ has: admin.locator('dt', { hasText: /^Suspended$/ }) }).locator('dd')).toContainText(`by ${ADMIN.handle}`);
  await account.goto('/account');
  await expect(account).toHaveURL(/\/login\?next=%2Faccount$/);
  await admin.goto(`/admin/users?status=suspended&find=${handle}`);
  await expect(directory.locator('tbody tr')).toContainText('Suspended');
  await directory.getByRole('link', { name: handle }).click();

  const restore = admin.getByRole('region', { name: 'Restore this account' });
  await restore.getByLabel('Current password').fill(ADMIN.password);
  await restore.getByRole('button', { name: `Restore ${handle}` }).click();
  await expectNotice(admin, 'Account restored.');
  await expect(admin).toHaveURL(`/admin/users?handle=${handle}#suspend`);
  await expect(status).toHaveText('Active');
  await waitForStyles(admin);
  expect(await admin.evaluate(() => document.documentElement.scrollWidth)).toBeLessThanOrEqual(admin.viewportSize()!.width);
  // The message link opens the staff message form addressed to the account.
  await admin.getByRole('link', { name: /Message user/ }).click();
  await expect(admin).toHaveURL(`/messages?to=${handle}`);
  await expect(admin.getByLabel('Recipient handle')).toHaveValue(handle);
  await admin.context().close();
  await account.context().close();
});

// A-706: a moderator without a second factor is refused a staff action with copy and links that work without
// JavaScript, follows the link to enrol an authenticator through the real form, and the same form then succeeds. The
// staff roster shows the moderator without a factor, then with TOTP.
test('a staff action needs a second factor; enrolling TOTP from the refusal unblocks it', async ({ browser, baseURL }) => {
  const handle = uniqueHandle('nofactor_mod');
  const password = 'browser-no-factor-mod-123';
  const moderator = await signedIn(browser, baseURL, handle, password, true);
  const admin = await signedIn(browser, baseURL, ADMIN.handle, ADMIN.password, false);
  await setRole(admin, handle, 'moderator', ADMIN.password, false);
  const rosterRow = async () => {
    await admin.goto('/admin/users');
    return admin.locator('section[aria-labelledby="staff-roster"] tbody tr', { hasText: handle });
  };
  await expect(await rosterRow()).toContainText('None: staff actions refused');
  await expect(admin.locator('#staff-without-factor')).toContainText('have no second factor and cannot take staff actions');

  await moderator.goto('/account');
  // New staff start hidden from the public staff list; showing this moderator is the refused staff change.
  const choice = moderator.locator('form[action="/account/staff-list"]');
  await expect(choice.getByLabel('Hide me from the public staff list')).toBeChecked();
  await choice.getByLabel('Hide me from the public staff list').uncheck();
  expect(await submitStatus(moderator, () => choice.getByRole('button', { name: 'Save choice' }).click())).toBe(403);
  await expect(moderator.getByRole('alert')).toHaveText('Staff actions need a second factor on your account. Turn on an authenticator (TOTP) or PGP sign-in, then submit the form again. Nothing was changed.');
  await expect(moderator.getByRole('link', { name: 'Turn on PGP sign-in' })).toHaveAttribute('href', '/pgp');
  await moderator.getByRole('link', { name: 'Set up an authenticator' }).click();
  await expect(moderator).toHaveURL(/\/totp$/);
  await enrollTOTP(moderator, password);

  await moderator.goto('/account');
  await expect(choice.getByLabel('Hide me from the public staff list')).toBeChecked(); // the refusal changed nothing
  await choice.getByLabel('Hide me from the public staff list').uncheck();
  await choice.getByRole('button', { name: 'Save choice' }).click();
  await expectNotice(moderator, 'Staff list choice saved.');
  await expect(choice.getByLabel('Hide me from the public staff list')).not.toBeChecked();
  await expect(await rosterRow()).toContainText('TOTP');
  await admin.context().close();
  await moderator.context().close();
});
