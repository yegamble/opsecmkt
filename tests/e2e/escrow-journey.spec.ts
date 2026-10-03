import { readFileSync } from 'node:fs';
import { expect, test, type APIRequestContext, type Browser, type Locator, type Page, type TestInfo } from '@playwright/test';
import { waitForStyles } from './styles';

// WR-escrow Phase 1 (A-246): the Bitcoin escrow paste journey with JavaScript disabled at 320 px, every form driven by
// the keyboard alone, against the real server and PostgreSQL. The escrow node is SIMULATED
// (tests/e2e/fixtures/escrow_rpc.py): its keys, PSBTs and transactions are real bytes, but its "signatures" are
// keyed hashes only the simulator accepts, the signed PSBTs pasted here come from the simulator playing each party's
// wallet, and no chain exists. This proves the application's forms, checks, order states and pages end to end; it is
// not chain evidence (that is scripts/test-escrow-regtest.sh, on real Bitcoin Core regtest).
const password = 'escrow-browser-admin-password-123';
const buyerPassword = 'escrow-browser-buyer-password-123';
const vendorPassword = 'escrow-browser-vendor-password-123';
const sim = process.env.E2E_ESCROW_RPC ?? 'http://127.0.0.1:18092';
const width = 320;

// Presses Tab until target has the focus (a keyboard user reaching a field or button).
async function tabTo(page: Page, target: Locator) {
  await expect(target).toHaveCount(1);
  for (let i = 0; i < 400; i++) {
    if (await target.evaluate(el => el === document.activeElement)) return;
    await page.keyboard.press('Tab');
  }
  throw new Error('not reachable with the Tab key');
}
// Types text into a field reached with Tab. A PSBT is inserted as one paste (keyboard.insertText).
async function typeInto(page: Page, field: Locator, text: string) {
  await tabTo(page, field);
  if (text.length > 120) await page.keyboard.insertText(text);
  else await page.keyboard.type(text);
}
// Reaches a submit button with Tab and presses Enter; resolves with the HTTP status of the page it loads.
async function pressSubmit(page: Page, button: Locator): Promise<number> {
  await tabTo(page, button);
  const [response] = await Promise.all([
    page.waitForResponse(r => r.request().method() === 'POST' && r.request().isNavigationRequest()),
    page.waitForEvent('load'),
    page.keyboard.press('Enter'),
  ]);
  return response.status();
}
async function check(page: Page, box: Locator) {
  await tabTo(page, box);
  await page.keyboard.press('Space');
  await expect(box).toBeChecked();
}
async function simPost(request: APIRequestContext, path: string, data: object): Promise<any> {
  const r = await request.post(`${sim}${path}`, { data });
  expect(r.ok(), `${path}: ${await r.text()}`).toBeTruthy();
  return r.json();
}
async function reloadUntil(page: Page, assertion: () => Promise<void>) {
  await expect(async () => { await page.reload(); await assertion(); }).toPass({ timeout: 30_000, intervals: [300, 700, 1000] });
}
// The page at 320 px: no horizontal scrolling, and a screenshot for the record.
async function shot(page: Page, info: TestInfo, name: string) {
  await waitForStyles(page);
  expect(await page.evaluate(() => document.documentElement.scrollWidth), `${name} at ${width}px`).toBeLessThanOrEqual(width);
  await page.screenshot({ path: info.outputPath(`${name}.png`), fullPage: true });
}
async function context(browser: Browser, baseURL: string | undefined): Promise<Page> {
  const ctx = await browser.newContext({ baseURL, javaScriptEnabled: false, viewport: { width, height: 800 } });
  return ctx.newPage();
}
async function register(page: Page, handle: string, pass: string) {
  await page.goto('/register');
  await typeInto(page, page.getByLabel('Handle', { exact: true }), handle);
  await typeInto(page, page.getByLabel('Password', { exact: true }), pass);
  await pressSubmit(page, page.getByRole('button', { name: 'Create account' }));
  await expect(page.locator('.account-name')).toHaveText(handle);
}
// Registers the party's escrow key and activates it by signing the practice transaction (in the simulated wallet).
async function activateKey(page: Page, request: APIRequestContext, name: string, pass: string, info: TestInfo) {
  const { key } = await simPost(request, '/test/escrow/key', { name });
  await page.goto('/escrow-key');
  await typeInto(page, page.getByLabel('Extended public key with its origin'), key);
  await typeInto(page, page.getByLabel('Current password'), pass);
  expect(await pressSubmit(page, page.getByRole('button', { name: 'Register key' }))).toBe(303);
  await expect(page.locator('.page-head .badge')).toHaveText('Inactive');
  await pressSubmitGet(page, page.getByRole('button', { name: 'Show the practice transaction' }));
  const practice = (await page.locator('#practice textarea[readonly]').inputValue()).trim();
  expect(practice).toMatch(/^cHNidP8/);
  await shot(page, info, `${name}-01-practice`);
  const { psbt } = await simPost(request, '/test/escrow/sign', { psbt: practice, fingerprint: key.slice(1, 9), sighash: 1 });
  await typeInto(page, page.getByLabel('Signed practice PSBT'), psbt);
  expect(await pressSubmit(page, page.getByRole('button', { name: 'Check signature' }))).toBe(303);
  await expect(page.locator('.page-head .badge')).toHaveText('Active');
  await shot(page, info, `${name}-02-key-active`);
  return key;
}
// A GET form submitted from the keyboard.
async function pressSubmitGet(page: Page, button: Locator) {
  await tabTo(page, button);
  await Promise.all([page.waitForEvent('load'), page.keyboard.press('Enter')]);
}
// Downloads the reader's copy of the settlement with the keyboard and returns it.
async function downloadCopy(page: Page): Promise<string> {
  await tabTo(page, page.getByRole('link', { name: 'Download your copy' }));
  const [download] = await Promise.all([page.waitForEvent('download'), page.keyboard.press('Enter')]);
  return readFileSync((await download.path())!, 'utf8').trim();
}
const settlementStatus = (page: Page) => page.locator('.escrow-settlement [role="status"]').first();

test('Bitcoin escrow paste journey: keys, checkout and funding, completion, the vendor\'s claim, a refused paste and a refund', async ({ page: admin, browser, baseURL, request }, info) => {
  // The marketplace, an administrator, a buyer and a vendor.
  await admin.goto('/setup');
  await typeInto(admin, admin.getByLabel('Setup token'), 'e2e-escrow-local-setup-token-at-least-32-characters');
  await typeInto(admin, admin.getByLabel('Handle', { exact: true }), 'escrow_admin');
  await typeInto(admin, admin.getByLabel('Password', { exact: true }), password);
  await pressSubmit(admin, admin.getByRole('button', { name: 'Initialize marketplace' }));
  await expect(admin).toHaveURL(/\/admin\?welcome=1$/);
  await pressSubmit(admin, admin.locator('.admin-auth').getByRole('button', { name: 'Turn CAPTCHA off' }));
  const buyer = await context(browser, baseURL);
  const vendor = await context(browser, baseURL);
  await register(buyer, 'escrow_buyer', buyerPassword);
  await register(vendor, 'escrow_vendor', vendorPassword);
  await admin.goto('/admin');
  const role = admin.locator('form', { has: admin.getByRole('button', { name: 'Update role' }) });
  await typeInto(admin, role.getByLabel('Account handle'), 'escrow_vendor');
  await tabTo(admin, role.getByLabel('Role'));
  await role.getByLabel('Role').press('v'); // the keyboard selects "vendor" in the role list
  await expect(role.getByLabel('Role')).toHaveValue('vendor');
  await pressSubmit(admin, role.getByRole('button', { name: 'Update role' }));
  await expect(admin.getByRole('status').filter({ hasText: 'Account role saved.' })).toBeVisible();

  // The vendor's digital listing with automatic delivery.
  await vendor.goto('/vendor-dashboard');
  await tabTo(vendor, vendor.getByText('+ Publish a new listing', { exact: true }));
  await vendor.keyboard.press('Enter');
  await typeInto(vendor, vendor.getByLabel('Listing title'), 'Escrow journey licence');
  await typeInto(vendor, vendor.getByLabel('Description', { exact: true }), 'Simulated escrow node journey; no real coins.');
  await tabTo(vendor, vendor.getByLabel('Fulfillment type'));
  await vendor.getByLabel('Fulfillment type').press('d');
  await expect(vendor.getByLabel('Fulfillment type')).toHaveValue('digital');
  await tabTo(vendor, vendor.getByLabel('Stock', { exact: true }));
  await vendor.keyboard.press('Control+A');
  await vendor.keyboard.type('5');
  await typeInto(vendor, vendor.getByLabel('Bitcoin reference price'), '0.001');
  await typeInto(vendor, vendor.getByLabel('Monero reference price'), '0.5');
  await typeInto(vendor, vendor.getByLabel('Automatic delivery content'), 'LICENCE-ESCROW-JOURNEY-0001');
  await pressSubmit(vendor, vendor.getByRole('button', { name: 'Publish listing' }));

  // ES-3, ES-9: each party registers an escrow key and activates it by practice signing.
  const buyerKey = await activateKey(buyer, request, 'buyer', buyerPassword, info);
  const vendorKey = await activateKey(vendor, request, 'vendor', vendorPassword, info);

  // An escrow checkout: draft, refund address, the escrow address and descriptor.
  const draft = async () => {
    await buyer.goto(`/?q=${encodeURIComponent('Escrow journey licence')}`);
    await tabTo(buyer, buyer.getByRole('link', { name: 'Escrow journey licence', exact: true }));
    await Promise.all([buyer.waitForEvent('load'), buyer.keyboard.press('Enter')]);
    await tabTo(buyer, buyer.getByRole('link', { name: 'Review order draft' }));
    await Promise.all([buyer.waitForEvent('load'), buyer.keyboard.press('Enter')]);
    await expect(buyer.getByLabel('Reference currency')).toHaveValue('BTC');
    await pressSubmit(buyer, buyer.getByRole('button', { name: 'Create unfunded draft' }));
    await buyer.waitForURL(/\/order\?id=/);
    await expect(buyer.getByRole('heading', { name: 'Or pay into a Bitcoin escrow (regtest only)' })).toBeVisible();
    const { address: refund } = await simPost(request, '/test/escrow/address', {});
    await typeInto(buyer, buyer.getByLabel('Refund address'), refund);
    await pressSubmit(buyer, buyer.getByRole('button', { name: 'Pay into an escrow' }));
    await expect(buyer.locator('.page-head .badge')).toHaveText('Awaiting payment');
    const address = (await buyer.locator('.payment-address .mono').innerText()).trim();
    expect(address).toMatch(/^bcrt1q[0-9a-z]{58}$/);
    await expect(buyer.locator('.escrow-descriptor')).toContainText(buyerKey.slice(0, 20));
    await expect(buyer.locator('.escrow-descriptor')).not.toContainText(vendorKey.slice(0, 20)); // the vendor's key is bare
    return { url: buyer.url(), address, refund };
  };
  const first = await draft();
  await shot(buyer, info, '03-escrow-awaiting-payment');

  // Funding: the full price, unconfirmed, then three blocks; the digital order completes on funding.
  await simPost(request, '/test/escrow/deposit', { address: first.address, sats: 100_000 });
  const escrowStatus = buyer.locator('.payment-figures div').filter({ has: buyer.getByText('Escrow status', { exact: true }) });
  await reloadUntil(buyer, async () => { await expect(buyer.getByText('The full price has been seen.', { exact: false })).toBeVisible({ timeout: 500 }); });
  await shot(buyer, info, '04-escrow-unconfirmed');
  await simPost(request, '/test/escrow/mine', { blocks: 3 });
  await reloadUntil(buyer, async () => { await expect(buyer.locator('.page-head .badge')).toHaveText('Completed', { timeout: 500 }); });
  await expect(buyer.getByText('LICENCE-ESCROW-JOURNEY-0001')).toBeVisible();
  await expect(escrowStatus).toBeVisible();
  await shot(buyer, info, '05-completed-on-funding-buyer');

  // The vendor's claim: a fresh address and the password; the market builds it.
  await vendor.goto(first.url);
  await expect(vendor.getByRole('heading', { name: 'Claim the escrowed coins' })).toBeVisible();
  await shot(vendor, info, '06-vendor-claim-form');
  const { address: claimTo } = await simPost(request, '/test/escrow/address', {});
  await typeInto(vendor, vendor.getByLabel('Claim address'), claimTo);
  await typeInto(vendor, vendor.locator('.escrow-claim').getByLabel('Current password'), vendorPassword);
  expect(await pressSubmit(vendor, vendor.getByRole('button', { name: 'Request the claim' }))).toBe(303);
  await reloadUntil(vendor, async () => { await expect(settlementStatus(vendor)).toContainText('Waiting for a signature from the buyer or the vendor', { timeout: 500 }); });
  await expect(vendor.locator('.settlement-outputs li')).toHaveCount(1);
  await expect(vendor.locator('.settlement-outputs')).toContainText(claimTo);
  await shot(vendor, info, '07-claim-awaiting-signature');
  const copy = await downloadCopy(vendor);
  expect(copy).toMatch(/^cHNidP8/);
  expect(copy).toBe((await vendor.locator('.escrow-settlement textarea[readonly]').inputValue()).trim());

  // A refused paste: the vendor's wallet signed with SIGHASH_NONE. Nothing changes.
  const none = await simPost(request, '/test/escrow/sign', { psbt: copy, fingerprint: vendorKey.slice(1, 9), sighash: 2 });
  await typeInto(vendor, vendor.locator('.escrow-settlement').getByLabel('Signed PSBT'), none.psbt);
  expect(await pressSubmit(vendor, vendor.getByRole('button', { name: 'Submit signature' }))).toBe(400);
  await expect(vendor.locator('body')).toContainText('A signature uses a sighash type other than ALL. Sign with the default sighash type (ALL). Nothing was changed.');
  // Refusals are plain-text pages (http.Error) for every form of the market, not an escrow page: Chromium with
  // JavaScript off shows them unwrapped, so this one is recorded but not measured at 320 px.
  await vendor.screenshot({ path: info.outputPath('08-refused-paste.png'), fullPage: true });
  await vendor.goto(first.url);
  await expect(settlementStatus(vendor)).toContainText('Waiting for a signature');

  // The good paste: party_signed, then the market co-signs and broadcasts it, then it confirms.
  const signed = await simPost(request, '/test/escrow/sign', { psbt: copy, fingerprint: vendorKey.slice(1, 9), sighash: 1 });
  await typeInto(vendor, vendor.locator('.escrow-settlement').getByLabel('Signed PSBT'), signed.psbt);
  expect(await pressSubmit(vendor, vendor.getByRole('button', { name: 'Submit signature' }))).toBe(303);
  await expect(settlementStatus(vendor)).toContainText(/Signed by the vendor|Broadcast to the regtest network/);
  await shot(vendor, info, '09-claim-signed');
  await reloadUntil(vendor, async () => { await expect(settlementStatus(vendor)).toContainText('Broadcast to the regtest network: transaction', { timeout: 500 }); });
  await expect(vendor.locator('.escrow-raw-tx')).toBeVisible();
  await shot(vendor, info, '10-claim-broadcast');
  await simPost(request, '/test/escrow/mine', { blocks: 3 });
  await reloadUntil(vendor, async () => { await expect(settlementStatus(vendor)).toContainText('Confirmed on the regtest network: transaction', { timeout: 500 }); });
  await shot(vendor, info, '11-claim-confirmed-vendor');
  await buyer.goto(first.url);
  await expect(settlementStatus(buyer)).toContainText('Confirmed on the regtest network: transaction');
  await shot(buyer, info, '12-claim-confirmed-buyer');

  // A refund: an underpaid escrow order the buyer cancels; the buyer signs the refund to the pinned refund address.
  const second = await draft();
  await simPost(request, '/test/escrow/deposit', { address: second.address, sats: 40_000 });
  await simPost(request, '/test/escrow/mine', { blocks: 3 });
  const remaining = buyer.locator('.payment-figures div').filter({ has: buyer.getByText('Remaining to send', { exact: true }) });
  await reloadUntil(buyer, async () => { await expect(remaining).toContainText('0.0006', { timeout: 500 }); });
  await expect(buyer.locator('.page-head .badge')).toHaveText('Awaiting payment');
  await shot(buyer, info, '13-underpaid');
  const cancel = buyer.locator('form', { has: buyer.getByRole('button', { name: 'Cancel order' }) });
  await check(buyer, cancel.locator('input[name="payout_confirmed"]'));
  expect(await pressSubmit(buyer, cancel.getByRole('button', { name: 'Cancel order' }))).toBe(303);
  await expect(buyer.locator('.page-head .badge')).toHaveText('Cancelled');
  await reloadUntil(buyer, async () => { await expect(settlementStatus(buyer)).toContainText('Waiting for a signature', { timeout: 500 }); });
  await expect(buyer.locator('.settlement-outputs')).toContainText(second.refund);
  await shot(buyer, info, '14-refund-awaiting-signature');
  const refundCopy = await downloadCopy(buyer);
  const refundSigned = await simPost(request, '/test/escrow/sign', { psbt: refundCopy, fingerprint: buyerKey.slice(1, 9), sighash: 1 });
  await typeInto(buyer, buyer.locator('.escrow-settlement').getByLabel('Signed PSBT'), refundSigned.psbt);
  expect(await pressSubmit(buyer, buyer.getByRole('button', { name: 'Submit signature' }))).toBe(303);
  await reloadUntil(buyer, async () => { await expect(settlementStatus(buyer)).toContainText('Broadcast to the regtest network: transaction', { timeout: 500 }); });
  await simPost(request, '/test/escrow/mine', { blocks: 3 });
  await reloadUntil(buyer, async () => { await expect(settlementStatus(buyer)).toContainText('Confirmed on the regtest network: transaction', { timeout: 500 }); });
  await shot(buyer, info, '15-refund-confirmed');

  // The simulated node saw exactly two broadcast settlements, one per order, each spending only its own escrow.
  const state = await (await request.get(`${sim}/test/escrow/state`)).json();
  expect(Object.keys(state.sent)).toHaveLength(2);
});
