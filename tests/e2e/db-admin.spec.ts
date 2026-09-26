import { execFileSync } from 'node:child_process';
import { createPublicKey, randomBytes, verify } from 'node:crypto';
import { readFileSync } from 'node:fs';
import { expect, test } from '@playwright/test';
import { ADMIN, uniqueHandle } from './db-fixtures';
import { enrollTOTP, pgpFixture, RECIPIENT_FINGERPRINT, setRole, signedIn, submitStatus } from './db-helpers';

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
  await enrollTOTP(locked);

  const admin = await signedIn(browser, baseURL, ADMIN.handle, ADMIN.password, false);
  await admin.goto('/admin');
  const roleSection = admin.getByRole('region', { name: 'Assign an account role' });
  const roleForm = roleSection.locator('form');
  const newest = roleSection.locator('.account-list li', { hasText: `${vendorHandle} ·` });
  await expect(newest).toHaveText(`${vendorHandle} · buyer`);
  // The form takes a handle looked up on the server. An unknown one answers 404 with the page again: the
  // reason in an alert, the handle and role kept for correction, nothing changed.
  const unknown = uniqueHandle('nobody');
  await roleForm.getByLabel('Account handle').fill(unknown);
  await roleForm.getByLabel('Role').selectOption('vendor');
  expect(await submitStatus(admin, () => roleForm.getByRole('button', { name: 'Update role' }).click())).toBe(404);
  await expect(admin.getByRole('alert')).toHaveText(`No buyer, vendor or moderator has the handle "${unknown}". Check the spelling: handles are case-sensitive, and administrator roles cannot be changed here.`);
  await expect(roleForm.getByLabel('Account handle')).toHaveValue(unknown);
  await expect(roleForm.getByLabel('Role')).toHaveValue('vendor');
  // The administrator's own role is fixed.
  await roleForm.getByLabel('Account handle').fill(ADMIN.handle);
  expect(await submitStatus(admin, () => roleForm.getByRole('button', { name: 'Update role' }).click())).toBe(400);
  await expect(admin.locator('body')).toHaveText('Cannot change your own administrator role');
  await setRole(admin, vendorHandle, 'vendor');
  await expect(admin.getByRole('status')).toHaveText('Changes saved.');
  await admin.waitForLoadState();
  await admin.reload();
  await expect(newest).toHaveText(`${vendorHandle} · vendor`);
  // The administrator's row names the account and both roles; the account gets its own row, and the
  // Account column shows whose row each is.
  const auditRows = admin.locator('table').last().locator('tbody tr');
  await expect(auditRows.filter({ hasText: `Changed role of ${vendorHandle} from buyer to vendor` }).locator('td').first()).toHaveText(ADMIN.handle);
  await expect(auditRows.filter({ hasText: 'Role changed from buyer to vendor by an administrator' }).filter({ hasText: vendorHandle }).locator('td').first()).toHaveText(vendorHandle);
  // Only roles the server can assign are offered; administrator remains setup-only.
  await expect(roleForm.getByLabel('Role').locator('option[value="admin"]')).toHaveCount(0);

  // Second-factor reset by handle: the helper list shows accounts with a factor, never the administrator; an
  // unknown handle answers 404 with the handle kept; it needs the administrator's own password; it ends the
  // account's sessions and records the reset on both accounts.
  const reset = admin.getByRole('region', { name: "Reset a user's second factors" });
  const factorList = reset.locator('.account-list li');
  await expect(factorList.filter({ hasText: `${lockedHandle} ·` })).toHaveText(`${lockedHandle} · TOTP`);
  await expect(factorList.filter({ hasText: `${vendorHandle} ·` })).toHaveCount(0);
  await expect(factorList.filter({ hasText: `${ADMIN.handle} ·` })).toHaveCount(0);
  const submitReset = async (handle: string, password: string) => {
    await admin.goto('/admin');
    await reset.getByLabel('Account handle').fill(handle);
    await reset.getByLabel('Current password').fill(password);
    return submitStatus(admin, () => reset.getByRole('button', { name: 'Reset second factors' }).click());
  };
  expect(await submitReset(unknown, ADMIN.password)).toBe(404);
  await expect(admin.getByRole('alert')).toHaveText(`No account has the handle "${unknown}". Check the spelling: handles are case-sensitive.`);
  await expect(reset.getByLabel('Account handle')).toHaveValue(unknown);
  await expect(reset.getByLabel('Current password')).toHaveValue('');
  expect(await submitReset(lockedHandle, 'not-the-admin-password')).toBe(401);
  await expect(admin.locator('body')).toHaveText('Password incorrect');
  await locked.goto('/account');
  await expect(locked.locator('.key-values div', { hasText: 'TOTP authentication' }).locator('dd')).toHaveText('Enabled');
  expect(await submitReset(lockedHandle, ADMIN.password)).toBe(303);
  await expect(admin).toHaveURL(/\/admin\?saved=1#reset-factors$/);
  await expect(factorList.filter({ hasText: `${lockedHandle} ·` })).toHaveCount(0);
  const resetRows = admin.locator('table').last().locator('tbody tr');
  await expect(resetRows.filter({ hasText: `Reset second factors of ${lockedHandle}: TOTP and recovery codes turned off; ended 1 session(s) (confirmed with password)` }).locator('td').first()).toHaveText(ADMIN.handle);
  await expect(resetRows.filter({ hasText: `Second factors reset by administrator ${ADMIN.handle}: TOTP and recovery codes turned off; ended 1 session(s)` }).locator('td').first()).toHaveText(lockedHandle);
  expect(await admin.evaluate(() => document.documentElement.scrollWidth)).toBeLessThanOrEqual(admin.viewportSize()!.width);
  await admin.context().close();

  // The account was signed out, signs in with the password alone and is told what happened.
  await locked.goto('/account');
  await expect(locked).toHaveURL(/\/login$/);
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
  await vendor.waitForURL(/\/listing-edit\?id=/);
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

test('site settings, account suspension, operator key, canary signature checks and audit export', async ({ browser, baseURL }) => {
  // A fresh account to suspend and restore (it reuses this test's administrator sign-in).
  const suspectHandle = uniqueHandle('suspect');
  const suspectPassword = 'browser-suspect-password-123';
  const suspect = await signedIn(browser, baseURL, suspectHandle, suspectPassword, true);
  const admin = await signedIn(browser, baseURL, ADMIN.handle, ADMIN.password, false);
  await admin.goto('/admin');
  const siteName = admin.getByLabel('Site name');
  const original = await siteName.inputValue();
  const renamed = `E2E ${uniqueHandle('market')}`.slice(0, 40);
  const saveSettings = () => admin.getByRole('button', { name: 'Save settings' }).click();

  await siteName.fill(renamed);
  expect(await submitStatus(admin, saveSettings)).toBe(303);
  await expect(admin).toHaveURL(/\/admin\?saved=1$/);
  await admin.waitForLoadState();
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

  // Suspension by handle: an unknown handle answers 404 with the page again (handle and choice kept); it needs
  // the administrator's password; it signs the account out, blocks sign-in with the reason (only after a
  // correct password), lists the account for restoring and records the change on both accounts.
  const suspension = admin.getByRole('region', { name: 'Suspend or restore an account' });
  const submitSuspension = async (handle: string, action: 'Suspend' | 'Restore', password: string) => {
    await admin.goto('/admin');
    await suspension.getByLabel('Account handle').fill(handle);
    await suspension.getByRole('radio', { name: new RegExp(`^${action}:`) }).check();
    await suspension.getByLabel('Current password').fill(password);
    return submitStatus(admin, () => suspension.getByRole('button', { name: 'Suspend or restore' }).click());
  };
  const suspended = suspension.locator('.account-list li', { hasText: `${suspectHandle} ·` });
  await admin.goto('/admin');
  await expect(suspended).toHaveCount(0);
  const nobody = uniqueHandle('nobody');
  expect(await submitSuspension(nobody, 'Suspend', ADMIN.password)).toBe(404);
  await expect(admin.getByRole('alert')).toHaveText(`No account has the handle "${nobody}". Check the spelling: handles are case-sensitive.`);
  await expect(suspension.getByLabel('Account handle')).toHaveValue(nobody);
  await expect(suspension.getByRole('radio', { name: /^Suspend:/ })).toBeChecked();
  expect(await submitSuspension(suspectHandle, 'Suspend', 'not-the-admin-password')).toBe(401);
  await expect(admin.locator('body')).toHaveText('Password incorrect');
  await suspect.goto('/account');
  await expect(suspect.locator('.account-name')).toHaveText(suspectHandle);
  expect(await submitSuspension(suspectHandle, 'Suspend', ADMIN.password)).toBe(303);
  await expect(admin).toHaveURL(/\/admin\?saved=1#suspend$/);
  await expect(suspended).toHaveText(new RegExp(`^${suspectHandle} · buyer · suspended \\d{4}-\\d{2}-\\d{2} \\d{2}:\\d{2} UTC$`));
  const suspendRows = admin.locator('table').last().locator('tbody tr');
  await expect(suspendRows.filter({ hasText: `Suspended account ${suspectHandle}; ended 1 session(s) and any pending sign-ins (confirmed with password)` }).locator('td').first()).toHaveText(ADMIN.handle);
  await expect(suspendRows.filter({ hasText: `Account suspended by administrator ${ADMIN.handle}; ended 1 session(s)` }).locator('td').first()).toHaveText(suspectHandle);
  expect(await admin.evaluate(() => document.documentElement.scrollWidth)).toBeLessThanOrEqual(admin.viewportSize()!.width);

  // The suspended account's open page is signed out; a wrong password is answered as for any account, the
  // right one with the reason.
  await suspect.goto('/account');
  await expect(suspect).toHaveURL(/\/login$/);
  const suspectSignIn = async (password: string) => {
    await suspect.goto('/login');
    await suspect.getByLabel('Handle', { exact: true }).fill(suspectHandle);
    await suspect.getByLabel('Password', { exact: true }).fill(password);
    return submitStatus(suspect, () => suspect.getByRole('button', { name: 'Sign in' }).click());
  };
  expect(await suspectSignIn('wrong-suspect-password-1')).toBe(401);
  await expect(suspect.locator('body')).toHaveText('Invalid handle or password');
  expect(await suspectSignIn(suspectPassword)).toBe(403);
  await expect(suspect.locator('body')).toHaveText('This account is suspended. Contact the market staff.');

  // Restoring allows sign-in again; the account was told about both changes.
  expect(await submitSuspension(suspectHandle, 'Restore', ADMIN.password)).toBe(303);
  await expect(suspended).toHaveCount(0);
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
  await expect(canaryPanel).toContainText('No signed canary published');
  await keyPanel.getByLabel('Armored public key').fill('-----BEGIN PGP PUBLIC KEY BLOCK-----\n\nbm90IGEga2V5\n-----END PGP PUBLIC KEY BLOCK-----');
  expect(await submitStatus(admin, () => keyPanel.getByRole('button', { name: 'Save operator key' }).click())).toBe(400);
  await expect(admin.locator('body')).toContainText('Operator key rejected:');
  await admin.goto('/admin');
  await keyPanel.getByLabel('Armored public key').fill(pgpFixture('recipient.pub.asc'));
  expect(await submitStatus(admin, () => keyPanel.getByRole('button', { name: 'Save operator key' }).click())).toBe(303);
  await expect(admin).toHaveURL(/\/admin\?saved=1#transparency$/);
  await admin.waitForLoadState();
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

// A-116: the payout resolve forms are the last human check before a possible double pay. Their help text and
// confirmation labels must wrap inside the payouts table (not run off under table{white-space:nowrap}), and each
// summary must say which payout it resolves. No wallet runs here, so the payouts are seeded in SQL: a refund held
// by a restore from backup and an account suspension (both confirmations), an ambiguous failed release and a definite
// failed release whose recipient has since saved another address (A-121: the move to the current address). A fresh
// account is made administrator for this test only, so the shared administrator's sign-in budget is untouched.
test('payout resolve forms wrap inside the payouts table and name what they resolve', async ({ browser, baseURL }) => {
  const database = process.env.E2E_DATABASE_URL!;
  const sql = (query: string) => execFileSync('psql', [database, '-v', 'ON_ERROR_STOP=1', '-qtAc', query], { encoding: 'utf8' }).trim();
  const hex = (bytes: number) => randomBytes(bytes).toString('hex');
  const adminHandle = uniqueHandle('payout_admin');
  const buyer = uniqueHandle('refund_recipient_long_handle');
  const vendor = uniqueHandle('release_recipient_long_handle');
  const [buyerID, vendorID, productID, refundOrder, releaseOrder, movedOrder] = [hex(16), hex(16), hex(16), hex(32), hex(32), hex(32)];
  const admin = await signedIn(browser, baseURL, adminHandle, 'browser-payout-admin-password-123', true, 1280);
  try {
    sql(`UPDATE users SET role='admin' WHERE handle='${adminHandle}';
      INSERT INTO users(id,handle,password_hash,role) VALUES ('${buyerID}','${buyer}','!','buyer'),('${vendorID}','${vendor}','!','vendor');
      UPDATE users SET payout_xmr='5${'b'.repeat(94)}',payout_xmr_changed=now() WHERE id='${vendorID}';
      INSERT INTO products(id,vendor_id,title,description,category,region,kind,btc,xmr,stock,archived)
        VALUES ('${productID}','${vendorID}','Payout layout fixture','Seeded by db-admin.spec.ts','Other','Worldwide','physical',100000,500000000000,0,true);
      INSERT INTO orders(id,buyer_id,product_id,currency,amount,state)
        VALUES ('${refundOrder}','${buyerID}','${productID}','BTC',100000,'cancelled'),('${releaseOrder}','${buyerID}','${productID}','XMR',500000000000,'completed'),
          ('${movedOrder}','${buyerID}','${productID}','XMR',500000000000,'completed');
      INSERT INTO payouts(order_id,kind,user_id,currency,amount,address,state,error,send_ambiguous) VALUES
        ('${refundOrder}','refund','${buyerID}','BTC',100000,'tb1q${'q'.repeat(58)}','held',
         'Restored from backup: verify in the wallet before releasing; this payout may already have been sent. Suspended account: payout held when the recipient''s account was suspended; an administrator must check the payout address before releasing it.',false),
        ('${releaseOrder}','release','${vendorID}','XMR',500000000000,'5${'a'.repeat(94)}','failed',
         'wallet call failed without a definite answer: Post "http://monero-wallet-rpc:18083/json_rpc": context deadline exceeded (Client.Timeout exceeded while awaiting headers)',true),
        ('${movedOrder}','release','${vendorID}','XMR',500000000000,'5${'a'.repeat(94)}','failed',
         'The wallet rejected the send; nothing was broadcast. monero-wallet-rpc error -17: not enough money',false);`);
    const ids = sql(`SELECT id FROM payouts WHERE order_id IN ('${refundOrder}','${releaseOrder}','${movedOrder}') ORDER BY order_id='${movedOrder}', order_id='${releaseOrder}'`).split('\n');
    const summaries = [
      `Resolve payout ${ids[0]}: refund 0.001 BTC to ${buyer}, order ${refundOrder.slice(0, 8)}`,
      `Resolve payout ${ids[1]}: release 0.5 XMR to ${vendor}, order ${releaseOrder.slice(0, 8)}`,
      `Resolve payout ${ids[2]}: release 0.5 XMR to ${vendor}, order ${movedOrder.slice(0, 8)}`,
    ];
    await admin.goto('/admin#payouts');
    for (const [i, summary] of summaries.entries()) {
      await expect(admin.locator('details.payout-actions > summary').filter({ hasText: new RegExp(`^Resolve payout ${ids[i]}\\b`) })).toHaveText(summary);
      const details = admin.locator('details.payout-actions', { has: admin.getByText(summary, { exact: true }) });
      await details.locator('summary').click();
      const row = admin.locator('tr', { has: details });
      // The resolve cell and the long-value cells wrap instead of widening the table.
      expect(await details.evaluate(e => getComputedStyle(e).whiteSpace), summary).toBe('normal');
      for (const cell of await row.locator('td.mono').all()) {
        expect(await cell.evaluate(e => getComputedStyle(e).whiteSpace), summary).toBe('normal');
      }
      // Whichever control has focus (the browser scrolls it into view), every help text and label of its form lies
      // inside the table's scroller, so the administrator reads the whole confirmation they are ticking.
      for (const form of await details.locator('form').all()) {
        for (const control of await form.locator('input:not([type="hidden"])').all()) {
          await control.focus();
          const clipped = await form.evaluate(f => {
            const wrap = f.closest('.table-wrap')!.getBoundingClientRect();
            return [...f.querySelectorAll('.help, label')].filter(e => {
              const box = e.getBoundingClientRect();
              return box.left < wrap.left - 1 || box.right > wrap.right + 1;
            }).map(e => e.textContent!.trim().slice(0, 60));
          });
          expect(clipped, `${summary}: clipped with ${await control.getAttribute('name')} focused`).toEqual([]);
        }
      }
    }
    await expect(admin.locator('details.payout-actions input[name="not_broadcast"]')).toHaveCount(2);
    await expect(admin.locator('details.payout-actions input[name="address_checked"]')).toHaveCount(2);
    await expect(admin.locator('details.payout-actions input[name="op"][value="repoint"]')).toHaveCount(1);
    for (const width of [1280, 390, 320]) {
      await admin.setViewportSize({ width, height: 900 });
      expect(await admin.evaluate(() => document.documentElement.scrollWidth), `/admin at ${width}px`).toBeLessThanOrEqual(width);
    }
  } finally {
    // db-payments.spec.ts expects no payouts, and no other spec should meet an extra administrator.
    sql(`DELETE FROM payouts WHERE order_id IN ('${refundOrder}','${releaseOrder}','${movedOrder}');
      DELETE FROM orders WHERE id IN ('${refundOrder}','${releaseOrder}','${movedOrder}');
      DELETE FROM products WHERE id='${productID}';
      DELETE FROM users WHERE id IN ('${buyerID}','${vendorID}');
      DELETE FROM sessions WHERE user_id=(SELECT id FROM users WHERE handle='${adminHandle}');
      UPDATE users SET role='buyer' WHERE handle='${adminHandle}';`);
    await admin.context().close();
  }
});
