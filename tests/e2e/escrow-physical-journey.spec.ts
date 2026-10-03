import { execFileSync } from 'node:child_process';
import { expect, test, type APIRequestContext, type Browser, type Page } from '@playwright/test';
import { activateKey, check, context, downloadCopy, password, pressSubmit, register, reloadUntil, settlementStatus, shot, sim, simPost, tabTo, typeInto } from './escrow-helpers';
import { pgpFixture } from './db-helpers';

// WR-escrow Phase 2, slice P2-b (A-247; P2-R1, P2-R2): a physical escrow order released by the buyer's signature, with
// JavaScript disabled at 320 px and every form driven by the keyboard alone, against the real server and PostgreSQL.
// The escrow node is SIMULATED (tests/e2e/fixtures/escrow_rpc.py): its PSBTs and transactions are real bytes, but its
// "signatures" are keyed hashes only the simulator accepts, played for each party's wallet. Not chain evidence (that is
// TestEscrowRegtestPhysical in scripts/test-escrow-regtest.sh). Slice P2-f completes the journey: the buyer's and the vendor's views of a shipped order, the cancel and shipment races, a buyer who never signs, and the administrator's release deadline length (the last test, which restores the default).
const buyerPassword = 'escrow-physical-buyer-password-123';
const vendorPassword = 'escrow-physical-vendor-password-123';
const title = 'Escrow journey shipped goods';

// The market's administrator: sets the market up on a fresh database, or signs in when escrow-journey.spec.ts did.
async function administrator(admin: Page) {
  await admin.goto('/setup');
  if (await admin.getByLabel('Setup token').count() > 0) {
    await typeInto(admin, admin.getByLabel('Setup token'), 'e2e-escrow-local-setup-token-at-least-32-characters');
    await typeInto(admin, admin.getByLabel('Handle', { exact: true }), 'escrow_admin');
    await typeInto(admin, admin.getByLabel('Password', { exact: true }), password);
    await pressSubmit(admin, admin.getByRole('button', { name: 'Initialize marketplace' }));
    await expect(admin).toHaveURL(/\/admin\?welcome=1$/);
    await pressSubmit(admin, admin.locator('.admin-auth').getByRole('button', { name: 'Turn CAPTCHA off' }));
    return;
  }
  await admin.goto('/login');
  await typeInto(admin, admin.getByLabel('Handle', { exact: true }), 'escrow_admin');
  await typeInto(admin, admin.getByLabel('Password', { exact: true }), password);
  await pressSubmit(admin, admin.getByRole('button', { name: 'Sign in' }));
  await expect(admin.locator('.account-name')).toHaveText('escrow_admin');
}

test('Bitcoin escrow physical order: shipped with a claim address, the vendor signs, the buyer signs on the order page, completed', async ({ page: admin, browser, baseURL, request }, info) => {
  await administrator(admin);
  const buyer = await context(browser, baseURL);
  const vendor = await context(browser, baseURL);
  await register(buyer, 'phys_buyer', buyerPassword);
  await register(vendor, 'phys_vendor', vendorPassword);
  await admin.goto('/admin');
  const role = admin.locator('form', { has: admin.getByRole('button', { name: 'Update role' }) });
  await typeInto(admin, role.getByLabel('Account handle'), 'phys_vendor');
  await tabTo(admin, role.getByLabel('Role'));
  await role.getByLabel('Role').press('v');
  await expect(role.getByLabel('Role')).toHaveValue('vendor');
  await pressSubmit(admin, role.getByRole('button', { name: 'Update role' }));
  await expect(admin.getByRole('status').filter({ hasText: 'Account role saved.' })).toBeVisible();

  // A-83: the vendor's PGP key, so the buyer can send an encrypted shipping address; then a physical listing (the
  // dispatch window's default of 3 days).
  await vendor.goto('/account');
  const profile = vendor.locator('form', { has: vendor.getByRole('button', { name: 'Save profile' }) });
  await typeInto(vendor, profile.getByLabel('PGP public key', { exact: true }), pgpFixture('recipient.pub.asc'));
  await typeInto(vendor, profile.getByLabel('Current password', { exact: true }), vendorPassword);
  expect(await pressSubmit(vendor, profile.getByRole('button', { name: 'Save profile' }))).toBe(303);
  await expect(vendor.getByRole('status').filter({ hasText: 'Profile saved.' })).toBeVisible();
  await vendor.goto('/vendor-dashboard');
  await tabTo(vendor, vendor.getByText('+ Publish a new listing', { exact: true }));
  await vendor.keyboard.press('Enter');
  await typeInto(vendor, vendor.getByLabel('Listing title'), title);
  await typeInto(vendor, vendor.getByLabel('Description', { exact: true }), 'Simulated escrow node journey for a shipped item; no real coins.');
  await expect(vendor.getByLabel('Fulfillment type')).toHaveValue('physical');
  await expect(vendor.getByLabel('Dispatch within (days)')).toHaveValue('3');
  await tabTo(vendor, vendor.getByLabel('Stock', { exact: true }));
  await vendor.keyboard.press('Control+A');
  await vendor.keyboard.type('20');
  await typeInto(vendor, vendor.getByLabel('Bitcoin reference price'), '0.001');
  await typeInto(vendor, vendor.getByLabel('Monero reference price'), '0.5');
  await pressSubmit(vendor, vendor.getByRole('button', { name: 'Publish listing' }));

  const buyerKey = await activateKey(buyer, request, 'pbuyer', buyerPassword, info);
  const vendorKey = await activateKey(vendor, request, 'pvendor', vendorPassword, info);

  // Escrow checkout and funding: the order is paid and waits for its shipment.
  await buyer.goto(`/?q=${encodeURIComponent(title)}`);
  await tabTo(buyer, buyer.getByRole('link', { name: title, exact: true }));
  await Promise.all([buyer.waitForEvent('load'), buyer.keyboard.press('Enter')]);
  await tabTo(buyer, buyer.getByRole('link', { name: 'Review order draft' }));
  await Promise.all([buyer.waitForEvent('load'), buyer.keyboard.press('Enter')]);
  await pressSubmit(buyer, buyer.getByRole('button', { name: 'Create unfunded draft' }));
  await buyer.waitForURL(/\/order\?id=/);
  const { address: refund } = await simPost(request, '/test/escrow/address', {});
  await typeInto(buyer, buyer.getByLabel('Refund address'), refund);
  await pressSubmit(buyer, buyer.getByRole('button', { name: 'Pay into an escrow' }));
  await expect(buyer.locator('.page-head .badge')).toHaveText('Awaiting payment');
  const orderURL = buyer.url();
  const address = (await buyer.locator('.payment-address .mono').innerText()).trim();
  await simPost(request, '/test/escrow/deposit', { address, sats: 100_000 });
  await simPost(request, '/test/escrow/mine', { blocks: 3 });
  await reloadUntil(buyer, async () => { await expect(buyer.locator('.page-head .badge')).toHaveText('Paid', { timeout: 500 }); });
  await expect(buyer.getByText('The vendor ships the goods', { exact: false })).toBeVisible();

  // P2-R1: the vendor marks it shipped with a fresh claim address and the password; the release is built.
  await vendor.goto(orderURL);
  const ship = vendor.locator('form', { has: vendor.getByRole('button', { name: 'Mark as shipped' }) });
  await expect(ship).toHaveAttribute('action', '/escrow/ship');
  await shot(vendor, info, 'p01-vendor-ship-form');
  const { address: claimTo } = await simPost(request, '/test/escrow/address', {});
  await typeInto(vendor, ship.getByLabel('Claim address'), claimTo);
  await typeInto(vendor, ship.getByLabel('Current password'), vendorPassword);
  expect(await pressSubmit(vendor, ship.getByRole('button', { name: 'Mark as shipped' }))).toBe(303);
  await expect(vendor.locator('.page-head .badge')).toHaveText('Shipped');
  await reloadUntil(vendor, async () => { await expect(settlementStatus(vendor)).toContainText("Waiting for the vendor's and the buyer's signatures", { timeout: 500 }); });
  await expect(vendor.getByRole('heading', { name: 'Settlement: Release to the vendor' })).toBeVisible();
  await expect(vendor.locator('.settlement-outputs')).toContainText(claimTo);
  await expect(vendor.locator('body')).not.toContainText('Either you or the other party can sign');
  await shot(vendor, info, 'p02-release-awaiting-signatures');

  // Journey check 1: the buyer, who has done nothing yet, reads who acts next and the deadline in absolute UTC.
  await buyer.goto(orderURL);
  await expect(buyer.locator('.page-head .badge')).toHaveText('Shipped');
  await expect(buyer.locator('body')).toContainText('Who acts next: the vendor signs the release now; the buyer signs it once the goods have arrived. The market does not sign it.');
  await expect(buyer.locator('#escrow-deadline')).toContainText(/\d{4}-\d\d-\d\d \d\d:\d\d UTC — About 13 days \d+ h left/);
  await shot(buyer, info, 'p02b-buyer-shipped-deadline');

  // The vendor signs first (the simulated wallet signs the downloaded copy); the release waits for the buyer.
  const vendorCopy = await downloadCopy(vendor);
  const vendorSigned = await simPost(request, '/test/escrow/sign', { psbt: vendorCopy, fingerprint: vendorKey.slice(1, 9), sighash: 1 });
  await typeInto(vendor, vendor.locator('.escrow-settlement').getByLabel('Signed PSBT'), vendorSigned.psbt);
  expect(await pressSubmit(vendor, vendor.getByRole('button', { name: 'Submit signature' }))).toBe(303);
  await expect(settlementStatus(vendor)).toContainText("Waiting for the buyer's signature");
  await expect(vendor.locator('.page-head .badge')).toHaveText('Shipped');
  // Journey check 2: the vendor is told it waits for the buyer.
  await expect(vendor.locator('body')).toContainText('Who acts next: the buyer signs the release once the goods have arrived; the vendor waits.');
  await shot(vendor, info, 'p02c-vendor-waits-for-buyer');

  // P2-R2: the buyer signs the release on the order page; signing confirms receipt.
  await buyer.goto(orderURL);
  await expect(buyer.locator('.page-head .badge')).toHaveText('Shipped');
  await expect(buyer.locator('body')).toContainText('Who acts next: the buyer signs the release once the goods have arrived');
  await expect(buyer.locator('.escrow-settlement')).toContainText('Signing it confirms that you received the goods');
  await expect(buyer.locator('.escrow-settlement')).toContainText('ends your right to open a dispute');
  await expect(buyer.getByRole('button', { name: 'Confirm receipt and complete' })).toHaveCount(0);
  // Journey check 3: the signing section lists the expected outputs (who is paid what) and the network fee.
  await expect(buyer.getByRole('heading', { name: 'Expected outputs' })).toBeVisible();
  await expect(buyer.locator('.settlement-outputs li')).not.toHaveCount(0);
  await expect(buyer.locator('.settlement-outputs')).toContainText(claimTo);
  await expect(buyer.locator('.settlement-outputs')).toContainText('The vendor (the claim, less the network fee)');
  await expect(buyer.locator('.escrow-settlement')).toContainText('Network fee (to the miners, not an output)');
  await shot(buyer, info, 'p03-buyer-signs-release');
  const buyerCopy = await downloadCopy(buyer);
  const buyerSigned = await simPost(request, '/test/escrow/sign', { psbt: buyerCopy, fingerprint: buyerKey.slice(1, 9), sighash: 1 });
  await typeInto(buyer, buyer.locator('.escrow-settlement').getByLabel('Signed PSBT'), buyerSigned.psbt);
  expect(await pressSubmit(buyer, buyer.getByRole('button', { name: 'Submit signature' }))).toBe(303);
  await reloadUntil(buyer, async () => { await expect(buyer.locator('.page-head .badge')).toHaveText('Completed', { timeout: 500 }); });
  await expect(settlementStatus(buyer)).toContainText(/Broadcast to the regtest network: transaction|Signed by the buyer and the vendor/);
  await expect(buyer.locator('body')).toContainText('so the market did not sign it');
  await shot(buyer, info, 'p04-completed-buyer');
  await simPost(request, '/test/escrow/mine', { blocks: 3 });
  await reloadUntil(buyer, async () => { await expect(settlementStatus(buyer)).toContainText('Confirmed on the regtest network: transaction', { timeout: 500 }); });
  await vendor.goto(orderURL);
  await expect(vendor.locator('.page-head .badge')).toHaveText('Completed');
  await shot(vendor, info, 'p05-completed-vendor');

  // The simulated node received the release's transaction, the one the page names.
  const txid = (await settlementStatus(vendor).innerText()).match(/transaction ([0-9a-f]{64})/)?.[1];
  expect(txid).toBeTruthy();
  const state = await (await request.get(`${sim}/test/escrow/state`)).json();
  expect(Object.keys(state.sent)).toContain(txid);
});

// WR-escrow Phase 2, slice P2-c (P2-R3, P2-R5, P2-R9): the release deadline, a dispute that cancels it and the buyer's
// no-ship cancellation, on the accounts and listing the journey above made. The clock is moved through SQL on the
// test's own database (past migration 079's freeze trigger, inside one transaction). A-320: the connection details
// reach psql through libpq's environment, never its command line.
const database = new URL(process.env.E2E_ESCROW_DATABASE_URL!);
const psqlEnv = { ...process.env, PGHOST: database.hostname, PGPORT: database.port || '5432', PGUSER: decodeURIComponent(database.username),
  PGPASSWORD: decodeURIComponent(database.password), PGDATABASE: decodeURIComponent(database.pathname.slice(1)),
  PGSSLMODE: database.searchParams.get('sslmode') || 'prefer' };
const sql = (query: string) => execFileSync('psql', ['-v', 'ON_ERROR_STOP=1', '-qtAc', query], { encoding: 'utf8', env: psqlEnv }).trim();
function moveClock(orderURL: string, set: string) {
  const id = new URL(orderURL).searchParams.get('id')!;
  expect(id).toMatch(/^[0-9a-f]+$/);
  sql(`BEGIN; ALTER TABLE escrows DISABLE TRIGGER escrows_deadlines_frozen; UPDATE escrows SET ${set} WHERE order_id='${id}'; ALTER TABLE escrows ENABLE TRIGGER escrows_deadlines_frozen; COMMIT;`);
}
async function signIn(browser: Browser, baseURL: string | undefined, handle: string, pass: string): Promise<Page> {
  const page = await context(browser, baseURL);
  await page.goto('/login');
  await typeInto(page, page.getByLabel('Handle', { exact: true }), handle);
  await typeInto(page, page.getByLabel('Password', { exact: true }), pass);
  await pressSubmit(page, page.getByRole('button', { name: 'Sign in' }));
  await expect(page.locator('.account-name')).toHaveText(handle);
  return page;
}
// The buyer's new escrow order on the journey's listing, funded: its order page URL.
async function paidOrder(buyer: Page, request: APIRequestContext): Promise<string> {
  await buyer.goto(`/?q=${encodeURIComponent(title)}`);
  await tabTo(buyer, buyer.getByRole('link', { name: title, exact: true }));
  await Promise.all([buyer.waitForEvent('load'), buyer.keyboard.press('Enter')]);
  await tabTo(buyer, buyer.getByRole('link', { name: 'Review order draft' }));
  await Promise.all([buyer.waitForEvent('load'), buyer.keyboard.press('Enter')]);
  await pressSubmit(buyer, buyer.getByRole('button', { name: 'Create unfunded draft' }));
  await buyer.waitForURL(/\/order\?id=/);
  const { address: refund } = await simPost(request, '/test/escrow/address', {});
  await typeInto(buyer, buyer.getByLabel('Refund address'), refund);
  await pressSubmit(buyer, buyer.getByRole('button', { name: 'Pay into an escrow' }));
  const address = (await buyer.locator('.payment-address .mono').innerText()).trim();
  await simPost(request, '/test/escrow/deposit', { address, sats: 100_000 });
  await simPost(request, '/test/escrow/mine', { blocks: 3 });
  await reloadUntil(buyer, async () => { await expect(buyer.locator('.page-head .badge')).toHaveText('Paid', { timeout: 500 }); });
  return buyer.url();
}
// The vendor marks the order shipped with a fresh claim address; the release is built.
async function shipped(vendor: Page, orderURL: string, request: APIRequestContext) {
  await vendor.goto(orderURL);
  const ship = vendor.locator('form', { has: vendor.getByRole('button', { name: 'Mark as shipped' }) });
  const { address: claimTo } = await simPost(request, '/test/escrow/address', {});
  await typeInto(vendor, ship.getByLabel('Claim address'), claimTo);
  await typeInto(vendor, ship.getByLabel('Current password'), vendorPassword);
  expect(await pressSubmit(vendor, ship.getByRole('button', { name: 'Mark as shipped' }))).toBe(303);
  await reloadUntil(vendor, async () => { await expect(settlementStatus(vendor)).toContainText("Waiting for the vendor's and the buyer's signatures", { timeout: 500 }); });
}

test('Bitcoin escrow physical order: released at its deadline, the market co-signing the vendor\'s signature', async ({ browser, baseURL, request }, info) => {
  const buyer = await signIn(browser, baseURL, 'phys_buyer', buyerPassword);
  const vendor = await signIn(browser, baseURL, 'phys_vendor', vendorPassword);
  const { key: vendorKey } = await simPost(request, '/test/escrow/key', { name: 'pvendor' });
  const orderURL = await paidOrder(buyer, request);
  await shipped(vendor, orderURL, request);
  // The deadline, in UTC with the time left when the page loaded; the buyer's "who acts next" covers it.
  const deadline = vendor.locator('#escrow-deadline');
  await expect(deadline).toContainText(/\d{4}-\d\d-\d\d \d\d:\d\d UTC — About 13 days \d+ h left/);
  await expect(deadline).toContainText('computed when this page loaded');
  await expect(deadline).toContainText('Opening a dispute cancels the deadline');
  const vendorCopy = await downloadCopy(vendor);
  const vendorSigned = await simPost(request, '/test/escrow/sign', { psbt: vendorCopy, fingerprint: vendorKey.slice(1, 9), sighash: 1 });
  await typeInto(vendor, vendor.locator('.escrow-settlement').getByLabel('Signed PSBT'), vendorSigned.psbt);
  expect(await pressSubmit(vendor, vendor.getByRole('button', { name: 'Submit signature' }))).toBe(303);
  await expect(settlementStatus(vendor)).toContainText("Waiting for the buyer's signature");
  await buyer.goto(orderURL);
  await expect(buyer.locator('body')).toContainText('If you cannot sign, you need do nothing');
  await expect(buyer.locator('body')).not.toContainText('paused');
  await shot(buyer, info, 'p06-deadline-countdown-buyer');
  // The clock passes the deadline: the watcher completes the order and co-signs the vendor's signature.
  moveClock(orderURL, "release_at=now()-interval '1 minute'");
  await reloadUntil(buyer, async () => { await expect(buyer.locator('.page-head .badge')).toHaveText('Completed', { timeout: 500 }); });
  await expect(buyer.locator('.order-timeline')).toContainText('auto-release deadline passed');
  await reloadUntil(buyer, async () => { await expect(settlementStatus(buyer)).toContainText('Broadcast to the regtest network: transaction', { timeout: 500 }); });
  await shot(buyer, info, 'p07-deadline-released-buyer');
  await buyer.goto('/notifications');
  await expect(buyer.locator('body')).toContainText(/auto-release deadline passed \(\d{4}-\d\d-\d\d \d\d:\d\d UTC\)/);
  await simPost(request, '/test/escrow/mine', { blocks: 3 });
  await vendor.goto(orderURL);
  await reloadUntil(vendor, async () => { await expect(settlementStatus(vendor)).toContainText('Confirmed on the regtest network: transaction', { timeout: 500 }); });
  await shot(vendor, info, 'p08-deadline-confirmed-vendor');
});

test('Bitcoin escrow physical order: a dispute cancels the release deadline for good', async ({ browser, baseURL, request }, info) => {
  const buyer = await signIn(browser, baseURL, 'phys_buyer', buyerPassword);
  const vendor = await signIn(browser, baseURL, 'phys_vendor', vendorPassword);
  const orderURL = await paidOrder(buyer, request);
  await shipped(vendor, orderURL, request);
  await buyer.goto(orderURL);
  await tabTo(buyer, buyer.locator('summary', { hasText: 'Open a dispute' }));
  await buyer.keyboard.press('Enter');
  await expect(buyer.getByLabel('Describe the issue')).toHaveAccessibleDescription(/Opening a dispute cancels the deadline of \d{4}-\d\d-\d\d \d\d:\d\d UTC for good/);
  await typeInto(buyer, buyer.getByLabel('Describe the issue'), 'The parcel never arrived at my address at all.');
  expect(await pressSubmit(buyer, buyer.getByRole('button', { name: 'Submit dispute' }))).toBe(303);
  // The deadline passes while the order is disputed: it is never released, and no countdown is shown.
  moveClock(orderURL, "release_at=now()-interval '1 minute'");
  await buyer.waitForTimeout(3_000); // three watcher passes at PAYMENT_POLL_INTERVAL=1s
  await buyer.goto(orderURL);
  await expect(buyer.locator('.page-head .badge')).toHaveText('Disputed');
  await expect(buyer.locator('#escrow-deadline')).toHaveCount(0);
  await expect(buyer.locator('.order-timeline')).not.toContainText('auto-release deadline passed');
  await expect(buyer.locator('body')).not.toContainText('paused');
  await shot(buyer, info, 'p09-dispute-cancels-deadline');
  await vendor.goto('/notifications');
  await expect(vendor.locator('body')).toContainText(/release deadline of \d{4}-\d\d-\d\d \d\d:\d\d UTC no longer applies/);
});

test('Bitcoin escrow physical order: the buyer\'s no-ship cancel, refused before the window with its date and accepted after', async ({ browser, baseURL, request }, info) => {
  const buyer = await signIn(browser, baseURL, 'phys_buyer', buyerPassword);
  const { key: buyerKey } = await simPost(request, '/test/escrow/key', { name: 'pbuyer' });
  const orderURL = await paidOrder(buyer, request);
  // Before the window opens the page says when it does, and offers no cancel form.
  await expect(buyer.locator('.order-unavailable')).toContainText(/You can cancel from \d{4}-\d\d-\d\d \d\d:\d\d UTC if the vendor has not shipped/);
  await expect(buyer.getByRole('button', { name: 'Cancel order' })).toHaveCount(0);
  await shot(buyer, info, 'p10-no-ship-cancel-note');
  // The window opens: the cancel form with the refund confirmation. If the page is stale (the window is not open
  // after all), the server refuses with the date stated.
  moveClock(orderURL, "ship_by=now()-interval '1 minute'");
  await buyer.reload();
  const cancel = buyer.locator('form', { has: buyer.getByRole('button', { name: 'Cancel order' }) });
  await check(buyer, cancel.getByRole('checkbox'));
  moveClock(orderURL, "ship_by=date_trunc('minute',now())+interval '2 days'");
  const opens = sql(`SELECT to_char(ship_by AT TIME ZONE 'UTC','YYYY-MM-DD HH24:MI "UTC"') FROM escrows WHERE order_id='${new URL(orderURL).searchParams.get('id')}'`);
  expect(await pressSubmit(buyer, cancel.getByRole('button', { name: 'Cancel order' }))).toBe(409);
  await expect(buyer.locator('body')).toContainText(`You can cancel this order from ${opens} if the vendor has not marked it shipped by then`);
  // (A refusal is a bare text page, A-59, so it is not measured at 320 px here.)
  moveClock(orderURL, "ship_by=now()-interval '1 minute'");
  await buyer.goto(orderURL);
  await check(buyer, cancel.getByRole('checkbox'));
  expect(await pressSubmit(buyer, cancel.getByRole('button', { name: 'Cancel order' }))).toBe(303);
  await expect(buyer.locator('.page-head .badge')).toHaveText('Cancelled');
  await expect(buyer.getByRole('heading', { name: 'Settlement: Refund to the buyer' })).toBeVisible();
  await reloadUntil(buyer, async () => { await expect(settlementStatus(buyer)).toContainText('Waiting for a signature from the buyer or the vendor', { timeout: 500 }); });
  const copy = await downloadCopy(buyer);
  const signed = await simPost(request, '/test/escrow/sign', { psbt: copy, fingerprint: buyerKey.slice(1, 9), sighash: 1 });
  await typeInto(buyer, buyer.locator('.escrow-settlement').getByLabel('Signed PSBT'), signed.psbt);
  expect(await pressSubmit(buyer, buyer.getByRole('button', { name: 'Submit signature' }))).toBe(303);
  await reloadUntil(buyer, async () => { await expect(settlementStatus(buyer)).toContainText('Broadcast to the regtest network: transaction', { timeout: 500 }); });
  await shot(buyer, info, 'p12-no-ship-refund-broadcast');
});

// WR-escrow Phase 2, slice P2-f: the journey seat's remaining checks. The no-ship cancel and the vendor's shipment race
// on the order row lock, so exactly one wins; each loser reads which one did and when. (A refusal is a bare text page,
// A-59, so it is not measured at 320 px here.)
test('Bitcoin escrow physical order: the vendor ships just before the buyer cancels, and the buyer cancels just before the vendor ships', async ({ browser, baseURL, request }, info) => {
  const buyer = await signIn(browser, baseURL, 'phys_buyer', buyerPassword);
  const vendor = await signIn(browser, baseURL, 'phys_vendor', vendorPassword);
  const cancelForm = () => buyer.locator('form', { has: buyer.getByRole('button', { name: 'Cancel order' }) });

  // Journey check 9: the buyer's page offers the cancel (the window is open), the vendor ships, and the buyer's cancel
  // is refused with the specific message and the time of the shipment.
  const shippedFirst = await paidOrder(buyer, request);
  moveClock(shippedFirst, "ship_by=now()-interval '1 minute'");
  await buyer.goto(shippedFirst);
  await check(buyer, cancelForm().getByRole('checkbox'));
  await shipped(vendor, shippedFirst, request);
  expect(await pressSubmit(buyer, cancelForm().getByRole('button', { name: 'Cancel order' }))).toBe(409);
  await expect(buyer.locator('body')).toContainText(/The vendor marked this order shipped at \d{4}-\d\d-\d\d \d\d:\d\d UTC, so it can no longer be cancelled\. Sign the release once the goods have arrived, or open a dispute if they do not arrive\. Nothing was changed\./);
  await buyer.goto(shippedFirst);
  await expect(buyer.locator('.page-head .badge')).toHaveText('Shipped');
  await expect(buyer.getByRole('button', { name: 'Cancel order' })).toHaveCount(0);
  await shot(buyer, info, 'p13-ship-won-the-race');

  // The other way round: the vendor's ship form is open, the buyer cancels, and the vendor's shipment is refused with
  // the buyer's cancellation time and the instruction not to send the goods.
  const cancelledFirst = await paidOrder(buyer, request);
  await vendor.goto(cancelledFirst);
  const ship = vendor.locator('form', { has: vendor.getByRole('button', { name: 'Mark as shipped' }) });
  const { address: claimTo } = await simPost(request, '/test/escrow/address', {});
  await typeInto(vendor, ship.getByLabel('Claim address'), claimTo);
  await typeInto(vendor, ship.getByLabel('Current password'), vendorPassword);
  moveClock(cancelledFirst, "ship_by=now()-interval '1 minute'");
  await buyer.goto(cancelledFirst);
  await check(buyer, cancelForm().getByRole('checkbox'));
  expect(await pressSubmit(buyer, cancelForm().getByRole('button', { name: 'Cancel order' }))).toBe(303);
  await expect(buyer.locator('.page-head .badge')).toHaveText('Cancelled');
  expect(await pressSubmit(vendor, ship.getByRole('button', { name: 'Mark as shipped' }))).toBe(409);
  await expect(vendor.locator('body')).toContainText(/The buyer cancelled this order at \d{4}-\d\d-\d\d \d\d:\d\d UTC because it was not marked shipped within its dispatch window plus 48 hours, so it cannot be marked shipped: the escrow is refunded to the buyer\. Do not send the goods\. Nothing was changed\./);
  await vendor.goto(cancelledFirst);
  await expect(vendor.locator('.page-head .badge')).toHaveText('Cancelled');
  await shot(vendor, info, 'p14-cancel-won-the-race');
});

// Journey check 10: a buyer who never signs and never disputes is shown no error, only the notices and the countdown:
// the shipped notice (with the deadline in UTC) and, once the deadline is under 72 hours away, the reminder, the
// /orders banner and the countdown on the order page; the vendor is told the buyer's deadline.
test('Bitcoin escrow physical order: a buyer who never signs sees no error, and the notifications list the shipped and 72 h notices', async ({ browser, baseURL, request }, info) => {
  const buyer = await signIn(browser, baseURL, 'phys_buyer', buyerPassword);
  const vendor = await signIn(browser, baseURL, 'phys_vendor', vendorPassword);
  const orderURL = await paidOrder(buyer, request);
  await shipped(vendor, orderURL, request);
  const short = new URL(orderURL).searchParams.get('id')!.slice(0, 8);
  const response = await buyer.goto(orderURL);
  expect(response!.status()).toBe(200);
  await expect(buyer.locator('.alert.error')).toHaveCount(0);
  await expect(buyer.locator('body')).toContainText('If you cannot sign, you need do nothing');
  await buyer.goto('/notifications');
  await expect(buyer.locator('body')).toContainText(new RegExp(`Order ${short}[^]*Shipped[^]*Release deadline: \\d{4}-\\d\\d-\\d\\d \\d\\d:\\d\\d UTC`));
  await expect(buyer.locator('body')).not.toContainText('Reminder');
  await shot(buyer, info, 'p15-notifications-shipped');
  // The deadline comes within 72 hours: the watcher's next pass sends the reminder, once.
  moveClock(orderURL, "release_at=now()+interval '71 hours'");
  await buyer.goto('/notifications');
  await reloadUntil(buyer, async () => { await expect(buyer.locator('body')).toContainText('Reminder', { timeout: 500 }); });
  await expect(buyer.locator('body')).toContainText(new RegExp(`Order ${short}: Reminder — you have until \\d{4}-\\d\\d-\\d\\d \\d\\d:\\d\\d UTC to sign the release to the vendor on the order page if the goods have arrived, or to open a dispute if they have not`));
  await expect(buyer.locator('body')).toContainText(/Release deadline: \d{4}-\d\d-\d\d \d\d:\d\d UTC/);
  await shot(buyer, info, 'p16-notifications-reminder');
  await buyer.goto('/orders');
  await expect(buyer.locator('.deadline-banner')).toContainText('Release deadline in under 72 hours.');
  await expect(buyer.locator('.deadline-banner')).toContainText(/\d{4}-\d\d-\d\d \d\d:\d\d UTC/);
  await expect(buyer.locator('.deadline-banner')).toContainText('computed when this page loaded');
  await shot(buyer, info, 'p17-orders-banner');
  const page = await buyer.goto(orderURL);
  expect(page!.status()).toBe(200);
  await expect(buyer.locator('.alert.error')).toHaveCount(0);
  await expect(buyer.locator('#escrow-deadline')).toContainText(/About 2 days \d+ h left/);
  await shot(buyer, info, 'p18-order-under-72h');
  await vendor.goto('/notifications');
  await expect(vendor.locator('body')).toContainText(new RegExp(`Order ${short}: the buyer has until \\d{4}-\\d\\d-\\d\\d \\d\\d:\\d\\d UTC to sign the release or open a dispute`));
});

// Journey check 6: the administrator's release deadline length (7 to 30 days; each order takes its own copy at
// checkout). Last in the file, and set back to the default of 14 days at its end, so no other test sees it.
test('Bitcoin escrow physical order: the administrator sets 7 days, a new order gets 7 and an older order keeps 14', async ({ browser, baseURL, request }, info) => {
  const admin = await signIn(browser, baseURL, 'escrow_admin', password);
  const buyer = await signIn(browser, baseURL, 'phys_buyer', buyerPassword);
  const vendor = await signIn(browser, baseURL, 'phys_vendor', vendorPassword);
  const setDays = async (days: string) => {
    await admin.goto('/admin');
    const form = admin.locator('form', { has: admin.getByRole('button', { name: 'Save the release deadline' }) });
    const field = form.getByLabel(/^Release deadline \(whole days/);
    await tabTo(admin, field);
    await admin.keyboard.press('Control+A');
    await admin.keyboard.type(days);
    await typeInto(admin, form.getByLabel('Current password'), password);
    expect(await pressSubmit(admin, form.getByRole('button', { name: 'Save the release deadline' }))).toBe(303);
    await expect(admin.getByRole('status').filter({ hasText: 'Release deadline saved.' })).toBeVisible();
    await expect(admin.getByText(`In force: ${days} days`)).toBeVisible();
  };
  const older = await paidOrder(buyer, request);
  await shipped(vendor, older, request);
  await expect(vendor.locator('#escrow-deadline')).toContainText(/About 13 days \d+ h left/);
  await setDays('7');
  await shot(admin, info, 'p19-admin-release-days');
  const newer = await paidOrder(buyer, request);
  await shipped(vendor, newer, request);
  await expect(vendor.locator('#escrow-deadline')).toContainText(/About 6 days \d+ h left/);
  await expect(vendor.locator('#escrow-deadline')).toContainText(/\d{4}-\d\d-\d\d \d\d:\d\d UTC/);
  await shot(vendor, info, 'p20-new-order-seven-days');
  await buyer.goto(older);
  await expect(buyer.locator('#escrow-deadline')).toContainText(/About 13 days \d+ h left/);
  await buyer.goto(newer);
  await expect(buyer.locator('#escrow-deadline')).toContainText(/About 6 days \d+ h left/);
  await setDays('14');
});
