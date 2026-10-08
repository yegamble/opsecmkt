import { execFileSync } from 'node:child_process';
import { expect, test, type APIRequestContext, type Browser, type Locator, type Page, type TestInfo } from '@playwright/test';
import { check, context, password, pressSubmit, register, reloadUntil, settlementStatus, shot, tabTo, typeInto } from './escrow-helpers';
import { pgpFixture } from './db-helpers';

// WR-escrow Phase 3, slice X-g2 (A-248; P3-R3 to P3-R8, P3-R11): the Monero escrow journey with JavaScript disabled at
// 320 px, every form driven by the keyboard alone, against the real server and PostgreSQL. The escrow wallet-rpc and its
// fakechain daemon are SIMULATED (tests/e2e/fixtures/xmr_escrow_rpc.py): their keys and addresses are real Monero keys
// and addresses, and their replies have monero-wallet-rpc 0.18.5.1's shapes (tests/test_xmr_escrow_rpc_fixture.py), but
// no chain exists and no coins move. This proves the application's forms, checks, order states and pages end to end; it
// is not chain evidence (that is scripts/test-escrow-xmr-regtest.sh, on a real fakechain). The tests run in order on
// one fresh database (playwright.escrow-xmr.config.ts); a second server on it runs with ESCROW_XMR off.
//
// The last test covers retention (slice X-f, P3-R9): a wallet past its 90 days is destroyed and its order's page says a
// later deposit cannot be recovered (ES-8), and a missing wallet past them is confirmed destroyed on /admin. The 90 days
// are moved through SQL; nothing waits in real time.

const sim = process.env.E2E_XMR_SIM!;
const offURL = process.env.E2E_XMR_OFF_URL!;
const feeAddress = process.env.E2E_XMR_FEE_ADDRESS!;
const buyerPassword = 'xmr-escrow-buyer-password-123';
const vendorPassword = 'xmr-escrow-vendor-password-123';
const digitalTitle = 'Monero escrow journey licence';
const physicalTitle = 'Monero escrow journey parcel';
const custody = 'held by this market in an account used only for this order, inside a wallet the market opens each month; the market can move these coins';
const fee = 1_874_400_000; // the simulator's network fee for one input and two outputs (monerod 0.18.5.1 on a young fakechain)
const price = 500_000_000_000; // 0.5 XMR, the listings' price
const marketFee = 12_500_000_000; // 2.5% (ESCROW_XMR_FEE_BPS=250)
const xmr = (pico: number) => (pico / 1e12).toFixed(12).replace(/0+$/, '').replace(/\.$/, '');
const moneroAddress = /^[48][1-9A-HJ-NP-Za-km-z]{94}$/;

async function simPost(request: APIRequestContext, path: string, data: object): Promise<any> {
  const r = await request.post(`${sim}${path}`, { data });
  expect(r.ok(), `${path}: ${await r.text()}`).toBeTruthy();
  return r.json();
}
const address = async (request: APIRequestContext) => (await simPost(request, '/test/xmr/address', {})).address as string;
const deposit = (request: APIRequestContext, to: string, amount: number) => simPost(request, '/test/xmr/deposit', { address: to, amount });
const mine = (request: APIRequestContext, blocks: number) => simPost(request, '/test/xmr/mine', { blocks });
async function chain(request: APIRequestContext): Promise<any> {
  return (await request.get(`${sim}/test/xmr/state`)).json();
}

// The clock and the restore settings are moved through SQL on the test's own database (A-320: the connection details
// reach psql through libpq's environment, never its command line).
const database = new URL(process.env.E2E_ESCROW_XMR_DATABASE_URL!);
const psqlEnv = { ...process.env, PGHOST: database.hostname, PGPORT: database.port || '5432', PGUSER: decodeURIComponent(database.username),
  PGPASSWORD: decodeURIComponent(database.password), PGDATABASE: decodeURIComponent(database.pathname.slice(1)),
  PGSSLMODE: database.searchParams.get('sslmode') || 'prefer' };
const sql = (query: string) => execFileSync('psql', ['-v', 'ON_ERROR_STOP=1', '-qtAc', query], { encoding: 'utf8', env: psqlEnv }).trim();
const orderID = (url: string) => { const id = new URL(url).searchParams.get('id')!; expect(id).toMatch(/^[0-9a-f]+$/); return id; };
function moveClock(url: string, set: string) {
  sql(`BEGIN; ALTER TABLE escrows DISABLE TRIGGER escrows_deadlines_frozen; UPDATE escrows SET ${set} WHERE order_id='${orderID(url)}'; ALTER TABLE escrows ENABLE TRIGGER escrows_deadlines_frozen; COMMIT;`);
}
const setting = (key: string, on: boolean) => sql(on ? `INSERT INTO settings(key,value) VALUES('${key}','true') ON CONFLICT (key) DO UPDATE SET value='true'`
  : `DELETE FROM settings WHERE key='${key}'`);

async function signIn(browser: Browser, baseURL: string | undefined, handle: string, pass: string): Promise<Page> {
  const page = await context(browser, baseURL);
  await page.goto('/login');
  await typeInto(page, page.getByLabel('Handle', { exact: true }), handle);
  await typeInto(page, page.getByLabel('Password', { exact: true }), pass);
  await pressSubmit(page, page.getByRole('button', { name: 'Sign in' }));
  await expect(page.locator('.account-name')).toHaveText(handle);
  return page;
}
const badge = (page: Page) => page.locator('.page-head .badge');
const figure = (page: Page, label: string) => page.locator('.payment-figures div').filter({ has: page.getByText(label, { exact: true }) });
// The settlement section titled title (newest first, so .first() is the newest of that title).
const settlement = (page: Page, title: string) => page.locator('section.escrow-settlement', { has: page.getByRole('heading', { name: new RegExp(`^Settlement \\d+: ${title}$`) }) });
const txOf = async (section: Locator) => (await section.getByRole('status').first().innerText()).match(/transaction ([0-9a-f]{64})/)?.[1];

// A draft of the listing title in XMR (the reference currency chosen by keyboard), on its order page.
async function draft(buyer: Page, title: string) {
  await buyer.goto(`/?q=${encodeURIComponent(title)}`);
  await tabTo(buyer, buyer.getByRole('link', { name: title, exact: true }));
  await Promise.all([buyer.waitForEvent('load'), buyer.keyboard.press('Enter')]);
  await tabTo(buyer, buyer.getByRole('link', { name: 'Review order draft' }));
  await Promise.all([buyer.waitForEvent('load'), buyer.keyboard.press('Enter')]);
  await tabTo(buyer, buyer.getByLabel('Reference currency'));
  // Native-select typeahead chooses the Monero reference price without relying on platform popup key behavior.
  await buyer.keyboard.type('0.5');
  await expect(buyer.getByLabel('Reference currency')).toHaveValue('XMR');
  await pressSubmit(buyer, buyer.getByRole('button', { name: 'Create unfunded draft' }));
  await buyer.waitForURL(/\/order\?id=/);
}
// Monero escrow checkout from the draft: a real refund address from the simulator; returns the escrow address.
async function checkout(buyer: Page, request: APIRequestContext) {
  const offer = buyer.locator('section.escrow-checkout');
  await expect(offer.getByRole('heading', { name: 'Or pay into a Monero escrow (local test chain only)' })).toBeVisible();
  const refund = await address(request);
  await typeInto(buyer, offer.getByLabel('Refund address'), refund);
  expect(await pressSubmit(buyer, offer.getByRole('button', { name: 'Pay into a Monero escrow' }))).toBe(303);
  await expect(badge(buyer)).toHaveText('Awaiting payment');
  const escrow = (await buyer.locator('.payment-address .mono').innerText()).trim();
  expect(escrow).toMatch(moneroAddress);
  expect(escrow.startsWith('8')).toBeTruthy(); // an account's address 0 is a subaddress
  return { escrow, refund, url: buyer.url() };
}
// A funded escrow order of the listing: paid (physical) or completed (digital).
async function funded(buyer: Page, request: APIRequestContext, title: string) {
  await draft(buyer, title);
  const order = await checkout(buyer, request);
  await deposit(request, order.escrow, price);
  await mine(request, 10);
  await reloadUntil(buyer, async () => { await expect(badge(buyer)).toHaveText(title === digitalTitle ? 'Completed' : 'Paid', { timeout: 500 }); });
  return order;
}
// The vendor marks a paid physical order shipped with a fresh claim address and the password.
async function ship(vendor: Page, request: APIRequestContext, url: string) {
  await vendor.goto(url);
  const form = vendor.locator('#xmr-escrow-ship form');
  const claimTo = await address(request);
  await typeInto(vendor, form.getByLabel('Claim address'), claimTo);
  await typeInto(vendor, form.getByLabel('Current password'), vendorPassword);
  expect(await pressSubmit(vendor, form.getByRole('button', { name: 'Mark shipped' }))).toBe(303);
  await expect(badge(vendor)).toHaveText('Shipped');
  return claimTo;
}
// The vendor claims a completed order's escrow with a fresh address and the password.
async function claim(vendor: Page, request: APIRequestContext, url: string) {
  await vendor.goto(url);
  const form = vendor.locator('#xmr-escrow-claim form');
  const claimTo = await address(request);
  await typeInto(vendor, form.getByLabel('Claim address'), claimTo);
  await typeInto(vendor, form.getByLabel('Current password'), vendorPassword);
  expect(await pressSubmit(vendor, form.getByRole('button', { name: 'Request the claim' }))).toBe(303);
  return claimTo;
}
// Waits until the section's transaction is sent, mines 10 blocks and waits until it is confirmed; returns its hash.
async function sentAndConfirmed(page: Page, request: APIRequestContext, title: string, info: TestInfo, name: string) {
  const section = () => settlement(page, title).first();
  await reloadUntil(page, async () => { await expect(section().getByRole('status').first()).toContainText('Sent to the fakechain network: transaction ', { timeout: 500 }); });
  const tx = await txOf(section());
  expect(tx).toBeTruthy();
  await shot(page, info, `${name}-sent`);
  await mine(request, 10);
  await reloadUntil(page, async () => { await expect(section().getByRole('status').first()).toHaveText(`Confirmed on the fakechain network: transaction ${tx}.`, { timeout: 500 }); });
  await shot(page, info, `${name}-confirmed`);
  return tx!;
}
// The outputs of transaction tx the simulated chain recorded, as "address amount".
async function paid(request: APIRequestContext, tx: string): Promise<string[]> {
  return (await chain(request)).txs[tx].outputs.map((o: any) => `${o.address} ${o.amount}`);
}

test('Monero escrow: checkout with the custody copy, funding at 9 and 10 confirmations, the vendor\'s claim, and an underpaid order cancelled and refunded', async ({ page: admin, browser, baseURL, request }, info) => {
  // The marketplace, an administrator, a buyer and a vendor (with a PGP key, so a physical listing takes escrow, A-83).
  await admin.goto('/setup');
  await typeInto(admin, admin.getByLabel('Setup token'), 'e2e-escrow-xmr-local-setup-token-at-least-32-characters');
  await typeInto(admin, admin.getByLabel('Handle', { exact: true }), 'xmr_admin');
  await typeInto(admin, admin.getByLabel('Password', { exact: true }), password);
  await pressSubmit(admin, admin.getByRole('button', { name: 'Initialize marketplace' }));
  await expect(admin).toHaveURL(/\/admin\?welcome=1$/);
  await typeInto(admin, admin.locator('.admin-auth').getByLabel('Current password'), password);
  await pressSubmit(admin, admin.locator('.admin-auth').getByRole('button', { name: 'Turn CAPTCHA off' }));
  const buyer = await context(browser, baseURL);
  const vendor = await context(browser, baseURL);
  await register(buyer, 'xmr_buyer', buyerPassword);
  await register(vendor, 'xmr_vendor', vendorPassword);
  await admin.goto('/admin');
  const role = admin.locator('form', { has: admin.getByRole('button', { name: 'Update role' }) });
  await typeInto(admin, role.getByLabel('Account handle'), 'xmr_vendor');
  await tabTo(admin, role.getByLabel('Role'));
  await role.getByLabel('Role').press('v');
  await expect(role.getByLabel('Role')).toHaveValue('vendor');
  await typeInto(admin, role.getByLabel('Current password'), password);
  await pressSubmit(admin, role.getByRole('button', { name: 'Update role' }));
  await expect(admin.getByRole('status').filter({ hasText: 'Account role saved.' })).toBeVisible();
  await vendor.goto('/account');
  const profile = vendor.locator('form', { has: vendor.getByRole('button', { name: 'Save profile' }) });
  await typeInto(vendor, profile.getByLabel('PGP public key', { exact: true }), pgpFixture('recipient.pub.asc'));
  await typeInto(vendor, profile.getByLabel('Current password', { exact: true }), vendorPassword);
  expect(await pressSubmit(vendor, profile.getByRole('button', { name: 'Save profile' }))).toBe(303);
  for (const [title, kind] of [[digitalTitle, 'digital'], [physicalTitle, 'physical']]) {
    await vendor.goto('/vendor-dashboard');
    await tabTo(vendor, vendor.getByText('+ Publish a new listing', { exact: true }));
    await vendor.keyboard.press('Enter');
    await typeInto(vendor, vendor.getByLabel('Listing title'), title);
    await typeInto(vendor, vendor.getByLabel('Description', { exact: true }), 'Simulated Monero escrow journey; no real coins.');
    if (kind === 'digital') {
      await tabTo(vendor, vendor.getByLabel('Fulfillment type'));
      await vendor.getByLabel('Fulfillment type').press('d');
      await expect(vendor.getByLabel('Fulfillment type')).toHaveValue('digital');
    }
    await tabTo(vendor, vendor.getByLabel('Stock', { exact: true }));
    await vendor.keyboard.press('ControlOrMeta+A');
    await vendor.keyboard.type('40');
    await typeInto(vendor, vendor.getByLabel('Bitcoin reference price'), '0.001');
    await typeInto(vendor, vendor.getByLabel('Monero reference price'), '0.5');
    if (kind === 'digital') await typeInto(vendor, vendor.getByLabel('Automatic delivery content'), 'LICENCE-XMR-ESCROW-0001');
    await pressSubmit(vendor, vendor.getByRole('button', { name: 'Publish listing' }));
  }

  // Checkout: the offer and the order page state the custody sentence verbatim (A-402), the escrow address and amount.
  await draft(buyer, digitalTitle);
  await expect(buyer.locator('section.escrow-checkout')).toContainText(`the coins are ${custody}.`);
  await shot(buyer, info, '01-checkout-offer');
  const first = await checkout(buyer, request);
  await expect(buyer.getByRole('status').filter({ hasText: 'Monero escrow created.' })).toBeVisible();
  await expect(buyer.locator('main')).toContainText(`The coins in this escrow are ${custody}.`);
  await expect(figure(buyer, 'Required')).toContainText('0.5 XMR');
  await expect(figure(buyer, 'Your refund address (fixed for this order)')).toContainText(first.refund);
  await expect(figure(buyer, 'Market fee')).toContainText('2.5% of the price');
  await expect(figure(buyer, 'Escrow status')).toContainText('Waiting for a deposit');
  await shot(buyer, info, '02-awaiting-payment');

  // Funding: in the pool, at 9 confirmations it still waits; at 10 the digital order completes on funding.
  const dep = (await deposit(request, first.escrow, price)).txid;
  await reloadUntil(buyer, async () => { await expect(figure(buyer, 'Escrow status')).toContainText('Deposit seen. Waiting for 10 confirmations.', { timeout: 500 }); });
  await expect(buyer.locator('main')).toContainText('The full price has been seen.');
  await mine(request, 9);
  const row = buyer.locator('.payment-deposits tbody tr', { hasText: dep });
  await reloadUntil(buyer, async () => { await expect(row.locator('td').nth(2)).toHaveText('9', { timeout: 500 }); });
  await expect(row.locator('td').nth(3)).toHaveText('Waiting for confirmations');
  await expect(badge(buyer)).toHaveText('Awaiting payment');
  await expect(figure(buyer, 'Escrow status')).toContainText('Deposit seen. Waiting for 10 confirmations.');
  await shot(buyer, info, '03-nine-confirmations');
  await mine(request, 1);
  await reloadUntil(buyer, async () => { await expect(badge(buyer)).toHaveText('Completed', { timeout: 500 }); });
  await expect(buyer.getByText('LICENCE-XMR-ESCROW-0001')).toBeVisible();
  await expect(figure(buyer, 'Escrow status')).toContainText('Funded and completed');
  await expect(row.locator('td').nth(3)).toHaveText('Confirmed');
  await shot(buyer, info, '04-completed-on-funding');

  // The vendor's claim: a fresh address and the password step-up; a wrong password is refused and changes nothing.
  await vendor.goto(first.url);
  await expect(vendor.getByRole('heading', { name: 'Claim the escrowed coins' })).toBeVisible();
  await shot(vendor, info, '05-claim-form');
  const claimForm = vendor.locator('#xmr-escrow-claim form');
  const claimTo = await address(request);
  await typeInto(vendor, claimForm.getByLabel('Claim address'), claimTo);
  await typeInto(vendor, claimForm.getByLabel('Current password'), 'not-the-vendor-password');
  expect(await pressSubmit(vendor, claimForm.getByRole('button', { name: 'Request the claim' }))).toBe(401);
  await vendor.goto(first.url);
  await expect(vendor.locator('section.escrow-settlement')).toHaveCount(0);
  await typeInto(vendor, claimForm.getByLabel('Claim address'), claimTo);
  await typeInto(vendor, claimForm.getByLabel('Current password'), vendorPassword);
  expect(await pressSubmit(vendor, claimForm.getByRole('button', { name: 'Request the claim' }))).toBe(303);
  await expect(vendor.getByRole('status').filter({ hasText: 'Claim requested.' })).toBeVisible();
  const claimTx = await sentAndConfirmed(vendor, request, "The vendor's claim", info, '06-claim');
  // The page shows the transaction, the outputs in XMR (up to 12 decimals) and the network fee.
  const section = settlement(vendor, "The vendor's claim");
  await expect(section).toContainText(`The transaction spends 0.5 XMR from this order's escrow account and pays exactly these outputs; the network fee of ${xmr(fee)} XMR was taken from the first.`);
  await expect(section.locator('.settlement-outputs li').nth(0)).toContainText(`The vendor (the claim, less the network fee): ${xmr(price - marketFee - fee)} XMR`);
  await expect(section.locator('.settlement-outputs li').nth(0)).toContainText(claimTo);
  await expect(section.locator('.settlement-outputs li').nth(1)).toContainText(`The market fee: ${xmr(marketFee)} XMR`);
  const value = (label: string) => section.locator('dl div').filter({ has: vendor.getByText(label, { exact: true }) }).locator('dd');
  await expect(value('Network fee (to the miners, not an output)')).toHaveText(`${xmr(fee)} XMR`);
  await expect(value('Transaction')).toHaveText(claimTx);
  expect((await paid(request, claimTx)).sort()).toEqual([`${claimTo} ${price - marketFee - fee}`, `${feeAddress} ${marketFee}`].sort());
  await buyer.goto(first.url);
  await expect(settlement(buyer, "The vendor's claim").locator('.settlement-outputs li').nth(0)).toContainText('(shown only to its recipient)');
  await shot(buyer, info, '07-claim-confirmed-buyer');

  // An underpaid order: confirmed below the price, the buyer cancels it, and the refund is built and sent.
  await draft(buyer, digitalTitle);
  const second = await checkout(buyer, request);
  await deposit(request, second.escrow, 200_000_000_000);
  await mine(request, 10);
  await reloadUntil(buyer, async () => { await expect(figure(buyer, 'Escrow status')).toContainText('Confirmed deposits are below the price', { timeout: 500 }); });
  await expect(figure(buyer, 'Remaining to send')).toContainText('0.3 XMR');
  await expect(badge(buyer)).toHaveText('Awaiting payment');
  await shot(buyer, info, '08-underpaid');
  const cancel = buyer.locator('form', { has: buyer.getByRole('button', { name: 'Cancel order' }) });
  await expect(cancel).toContainText('Cancelling refunds the 0.2 XMR in this escrow, less the network fee, to the buyer\'s refund address given at checkout.');
  await check(buyer, cancel.locator('input[name="payout_confirmed"]'));
  expect(await pressSubmit(buyer, cancel.getByRole('button', { name: 'Cancel order' }))).toBe(303);
  await expect(badge(buyer)).toHaveText('Cancelled');
  const refundTx = await sentAndConfirmed(buyer, request, 'Refund to the buyer', info, '09-underpaid-refund');
  await expect(settlement(buyer, 'Refund to the buyer').locator('.settlement-outputs li').first())
    .toContainText(`The buyer's refund address (the refund, less the network fee): ${xmr(200_000_000_000 - fee)} XMR`);
  expect(await paid(request, refundTx)).toContain(`${second.refund} ${200_000_000_000 - fee}`);
});

test('Monero escrow physical order: shipped with a claim address, Confirm receipt with the password, the release sent', async ({ browser, baseURL, request }, info) => {
  const buyer = await signIn(browser, baseURL, 'xmr_buyer', buyerPassword);
  const vendor = await signIn(browser, baseURL, 'xmr_vendor', vendorPassword);
  const order = await funded(buyer, request, physicalTitle);
  await expect(figure(buyer, 'Escrow status')).toContainText('Funded');
  await vendor.goto(order.url);
  await expect(vendor.getByRole('heading', { name: 'Mark shipped' })).toBeVisible();
  await shot(vendor, info, 'p01-ship-form');
  const claimTo = await ship(vendor, request, order.url);
  // Nothing is built before the outcome (081): the release waits, and the deadline is stated in UTC.
  await expect(settlementStatus(vendor)).toHaveText('Requested: the market builds and sends this release only once the order is completed or its dispute is decided for the vendor. Nothing is sent before.');
  await expect(vendor.locator('#xmr-escrow-deadline')).toContainText(/\d{4}-\d\d-\d\d \d\d:\d\d UTC — About 13 days \d+ h left/);
  await shot(vendor, info, 'p02-shipped-vendor');
  await buyer.goto(order.url);
  await expect(badge(buyer)).toHaveText('Shipped');
  await expect(buyer.locator('#xmr-escrow-confirm')).toContainText('This cannot be undone, and the order can then no longer be disputed.');
  await shot(buyer, info, 'p03-confirm-form');
  const confirm = buyer.locator('#xmr-escrow-confirm form');
  await typeInto(buyer, confirm.getByLabel('Current password'), 'not-the-buyer-password');
  expect(await pressSubmit(buyer, confirm.getByRole('button', { name: 'Confirm receipt' }))).toBe(401);
  await buyer.goto(order.url);
  await expect(badge(buyer)).toHaveText('Shipped');
  await typeInto(buyer, confirm.getByLabel('Current password'), buyerPassword);
  expect(await pressSubmit(buyer, confirm.getByRole('button', { name: 'Confirm receipt' }))).toBe(303);
  await expect(buyer.getByRole('status').filter({ hasText: 'Receipt confirmed.' })).toBeVisible();
  await expect(badge(buyer)).toHaveText('Completed');
  await expect(buyer.getByRole('button', { name: 'Submit dispute' })).toHaveCount(0);
  const tx = await sentAndConfirmed(buyer, request, 'Release to the vendor', info, 'p04-release');
  expect((await paid(request, tx)).sort()).toEqual([`${claimTo} ${price - marketFee - fee}`, `${feeAddress} ${marketFee}`].sort());
});

test('Monero escrow physical order: released at its deadline by the watcher', async ({ browser, baseURL, request }, info) => {
  const buyer = await signIn(browser, baseURL, 'xmr_buyer', buyerPassword);
  const vendor = await signIn(browser, baseURL, 'xmr_vendor', vendorPassword);
  const order = await funded(buyer, request, physicalTitle);
  const claimTo = await ship(vendor, request, order.url);
  await buyer.goto(order.url);
  await expect(buyer.locator('main')).toContainText('If you cannot confirm receipt, you need do nothing');
  await shot(buyer, info, 'd01-deadline-countdown');
  moveClock(order.url, "release_at=now()-interval '1 minute'");
  await reloadUntil(buyer, async () => { await expect(badge(buyer)).toHaveText('Completed', { timeout: 500 }); });
  await expect(buyer.locator('.order-timeline')).toContainText('auto-release deadline passed');
  const tx = await sentAndConfirmed(buyer, request, 'Release to the vendor', info, 'd02-deadline-release');
  expect(await paid(request, tx)).toContain(`${claimTo} ${price - marketFee - fee}`);
});

test('Monero escrow dispute decided for the buyer: the resolve form states the consequence, the refund is sent', async ({ browser, baseURL, request }, info) => {
  const admin = await signIn(browser, baseURL, 'xmr_admin', password);
  const buyer = await signIn(browser, baseURL, 'xmr_buyer', buyerPassword);
  const vendor = await signIn(browser, baseURL, 'xmr_vendor', vendorPassword);
  const order = await funded(buyer, request, physicalTitle);
  await ship(vendor, request, order.url);
  await buyer.goto(order.url);
  await tabTo(buyer, buyer.locator('summary', { hasText: 'Open a dispute' }));
  await buyer.keyboard.press('Enter');
  await typeInto(buyer, buyer.getByLabel('Describe the issue'), 'The parcel never arrived at my address at all.');
  expect(await pressSubmit(buyer, buyer.getByRole('button', { name: 'Submit dispute' }))).toBe(303);
  await expect(buyer.getByRole('status').filter({ hasText: 'Dispute opened.' })).toBeVisible();
  await buyer.goto(order.url);
  await expect(badge(buyer)).toHaveText('Disputed');
  await expect(buyer.locator('#xmr-escrow-deadline')).toHaveCount(0);
  await expect(buyer.locator('main')).toContainText('no longer applies: opening the dispute cancelled it for good.');
  await shot(buyer, info, 'r00-disputed-buyer');

  // A-416, P3-R7: the decision is final once made; the form says so before it is submitted.
  await admin.goto('/moderator');
  const item = admin.locator('article.message').filter({ has: admin.locator(`a[href="/order?id=${orderID(order.url)}"]`) });
  const form = item.locator('form[action="/resolve"]');
  await expect(form).toContainText('The decision is paid from this order\'s escrow account by a transaction the market builds and sends');
  await expect(form).toContainText('this cannot be undone, and the decision cannot be changed once it is made.');
  const refund = form.getByRole('radio', { name: /^Refund to xmr_buyer: .*; the release to the vendor requested when the order was shipped is cancelled$/ });
  await shot(admin, info, 'r01-resolve-form');
  // The outcome radios are one tab stop (the first, release, while neither is checked); the arrow key moves to refund.
  await tabTo(admin, form.getByRole('radio', { name: /^Release to xmr_vendor: / }));
  await admin.keyboard.press('ArrowDown');
  await expect(refund).toBeChecked();
  await typeInto(admin, form.getByLabel('Decision', { exact: true }), 'The parcel did not arrive; the escrow goes back to the buyer.');
  await check(admin, form.getByRole('checkbox', { name: 'I checked the amount and who receives it for the outcome I chose. The market sends these coins on its next pass; this cannot be undone.' }));
  await typeInto(admin, form.getByLabel('Current password', { exact: true }), password);
  expect(await pressSubmit(admin, form.getByRole('button', { name: 'Resolve dispute' }))).toBe(303);

  await buyer.goto(order.url);
  await expect(badge(buyer)).toHaveText('Resolved');
  await expect(settlement(buyer, 'Release to the vendor').getByRole('status').first()).toHaveText('Superseded: a changed dispute decision replaced it before it was built.');
  const tx = await sentAndConfirmed(buyer, request, 'Refund to the buyer', info, 'r02-dispute-refund');
  expect(await paid(request, tx)).toContain(`${order.refund} ${price - fee}`);
});

test('Monero escrow administrator actions: re-request a failed settlement (A-401) and retry a set-aside one, each with the password', async ({ browser, baseURL, request }, info) => {
  const admin = await signIn(browser, baseURL, 'xmr_admin', password);
  const buyer = await signIn(browser, baseURL, 'xmr_buyer', buyerPassword);
  const vendor = await signIn(browser, baseURL, 'xmr_vendor', vendorPassword);

  // The daemon refuses the claim's stored transaction each time it is relayed: failed; the administrator re-requests it,
  // and the rebuilt transaction (the same coins, another hash) is taken.
  const failedOrder = await funded(buyer, request, digitalTitle);
  await simPost(request, '/test/xmr/refuse-relay', {});
  await claim(vendor, request, failedOrder.url);
  const failed = () => settlement(vendor, "The vendor's claim").last();
  await reloadUntil(vendor, async () => { await expect(failed().getByRole('status').first()).toContainText('was built and stored but never mined. The Monero daemon refused the stored transaction', { timeout: 500 }); });
  await shot(vendor, info, 'a01-failed-vendor');
  await admin.goto(failedOrder.url);
  const rerequest = admin.locator('form[action="/admin/escrow-xmr-rerequest"]');
  await expect(rerequest).toContainText('The new transaction spends exactly the same coins to the same destinations and amounts');
  await shot(admin, info, 'a02-rerequest-form');
  const failedID = await rerequest.locator('input[name="settlement_id"]').inputValue();
  await typeInto(admin, rerequest.getByLabel('Current password'), password);
  expect(await pressSubmit(admin, rerequest.getByRole('button', { name: `Re-request settlement ${failedID}` }))).toBe(303);
  await expect(admin.getByRole('status').filter({ hasText: 'Re-request recorded.' })).toBeVisible();
  await reloadUntil(admin, async () => {
    await expect(admin.locator(`#xmr-settlement-${failedID} [role="status"]`).first()).toContainText(/^Superseded by settlement \d+ after an administrator's re-request/, { timeout: 500 });
  });
  const again = settlement(admin, "The vendor's claim").first();
  await reloadUntil(admin, async () => { await expect(again.getByRole('status').first()).toContainText(`Sent to the fakechain network: transaction `, { timeout: 500 }); });
  await expect(again.getByRole('status').first()).toContainText(`It re-requests settlement ${failedID}: the same coins, to the same destinations.`);
  await shot(admin, info, 'a03-rerequest-sent');
  await mine(request, 10);
  await vendor.goto(failedOrder.url);
  await reloadUntil(vendor, async () => { await expect(settlement(vendor, "The vendor's claim").first().getByRole('status').first()).toContainText('Confirmed on the fakechain network', { timeout: 500 }); });

  // Building the claim drops the escrow wallet-rpc twice: it is set aside; the administrator asks for it again.
  const asideOrder = await funded(buyer, request, digitalTitle);
  await simPost(request, '/test/xmr/crash-transfer', { count: 2 });
  await claim(vendor, request, asideOrder.url);
  await reloadUntil(vendor, async () => { await expect(settlementStatus(vendor)).toContainText('Building this settlement made the escrow wallet stop responding; it is set aside', { timeout: 500 }); });
  await shot(vendor, info, 'a04-set-aside-vendor');
  await admin.goto(asideOrder.url);
  const retry = admin.locator('form[action="/admin/escrow-xmr-retry-build"]');
  const asideID = await retry.locator('input[name="settlement_id"]').inputValue();
  await shot(admin, info, 'a05-retry-form');
  await typeInto(admin, retry.getByLabel('Current password'), password);
  expect(await pressSubmit(admin, retry.getByRole('button', { name: `Build settlement ${asideID} again` }))).toBe(303);
  await expect(admin.getByRole('status').filter({ hasText: 'Recorded. The market builds this settlement on its next pass' })).toBeVisible();
  await sentAndConfirmed(admin, request, "The vendor's claim", info, 'a06-retried');
});

test('Monero escrow paused after a restore: no checkout and no settlement until the operator finishes, then the sweep', async ({ browser, baseURL, request }, info) => {
  const buyer = await signIn(browser, baseURL, 'xmr_buyer', buyerPassword);
  const vendor = await signIn(browser, baseURL, 'xmr_vendor', vendorPassword);
  const order = await funded(buyer, request, digitalTitle);
  setting('payments_recovery_required', true);
  try {
    await draft(buyer, digitalTitle);
    await expect(buyer.locator('section.escrow-checkout .order-unavailable')).toContainText('Monero escrow is paused while this market restores its records from a backup');
    await expect(buyer.getByRole('button', { name: 'Pay into a Monero escrow' })).toHaveCount(0);
    await shot(buyer, info, 'g01-paused-checkout');
    await claim(vendor, request, order.url);
    await expect(vendor.locator('main')).toContainText('Paused after a restore from backup.');
    await vendor.waitForTimeout(3_000); // three watcher passes at PAYMENT_POLL_INTERVAL=1s: nothing is built
    await vendor.reload();
    await expect(settlementStatus(vendor)).toContainText('Paused: the market restored its records from a backup');
    await shot(vendor, info, 'g02-paused-claim');
  } finally {
    setting('payments_recovery_required', false);
  }
  setting('xmr_escrow_sweep_required', true);
  try {
    await buyer.reload();
    await expect(buyer.locator('section.escrow-checkout .order-unavailable')).toContainText('Monero escrow is paused until this market has checked its escrow wallets');
    await shot(buyer, info, 'g03-sweep-checkout');
    await sentAndConfirmed(vendor, request, "The vendor's claim", info, 'g04-after-restore'); // settlements are not held by the sweep
  } finally {
    setting('xmr_escrow_sweep_required', false);
  }
  await buyer.reload();
  await expect(buyer.getByRole('button', { name: 'Pay into a Monero escrow' })).toBeVisible();
});

test('Monero escrow switched off: the order page says so and offers no Monero escrow form', async ({ browser, baseURL, request }, info) => {
  // An order paid and waiting for its shipment, read on this market's second server, which runs with ESCROW_XMR off.
  const buyer = await signIn(browser, baseURL, 'xmr_buyer', buyerPassword);
  const order = await funded(buyer, request, physicalTitle);
  const path = new URL(order.url).pathname + new URL(order.url).search;
  for (const [handle, pass, name] of [['xmr_buyer', buyerPassword, 'buyer'], ['xmr_vendor', vendorPassword, 'vendor']]) {
    const page = await signIn(browser, offURL, handle, pass);
    await page.goto(path);
    await expect(badge(page)).toHaveText('Paid');
    await expect(page.locator('main')).toContainText('Monero escrow is switched off on this market.');
    await expect(page.locator('main')).toContainText(`The coins in this escrow are ${custody}.`);
    await expect(page.locator('form[action^="/escrow-xmr/"]')).toHaveCount(0);
    await shot(page, info, `o01-off-${name}`);
    if (name === 'buyer') {
      await draft(page, digitalTitle);
      await expect(page.locator('form[action="/orders/escrow-xmr"]')).toHaveCount(0);
      await expect(page.getByRole('heading', { name: 'Or pay into a Monero escrow (local test chain only)' })).toHaveCount(0);
      await shot(page, info, 'o02-off-draft');
    }
  }
});

// The wallet that holds order url's account, and a wallet's state.
const walletOf = (url: string) => sql(`SELECT w.name FROM escrows es JOIN xmr_escrow_slots s ON s.id=es.xmr_slot_id JOIN xmr_escrow_wallets w ON w.id=s.wallet_id WHERE es.order_id='${orderID(url)}'`);
const walletState = (name: string) => sql(`SELECT state FROM xmr_escrow_wallets WHERE name='${name}'`);
// The active wallet is closed, as at a month's start (its free accounts retired); the watcher creates and activates the
// month's next wallet with its free accounts, and returns the active wallet's name once checkout can claim one.
async function nextWallet(): Promise<string> {
  sql("BEGIN; UPDATE xmr_escrow_slots SET state='retired',updated=now() WHERE state='free'; UPDATE xmr_escrow_wallets SET state='closing',updated=now() WHERE state='active'; COMMIT;");
  await expect.poll(() => sql("SELECT count(*) FROM xmr_escrow_slots s JOIN xmr_escrow_wallets w ON w.id=s.wallet_id WHERE w.state='active' AND s.state='free'"), { timeout: 60_000 }).not.toBe('0');
  return sql("SELECT name FROM xmr_escrow_wallets WHERE state='active'");
}
// Moves retained wallet name's last order close (and so its destruction date) 91 days back.
function past90Days(name: string) {
  sql(`BEGIN; ALTER TABLE xmr_escrow_wallets DISABLE TRIGGER xmr_escrow_wallets_kept;
    UPDATE xmr_escrow_wallets SET last_closed_at=last_closed_at-interval '91 days',destroy_after=destroy_after-interval '91 days' WHERE name='${name}';
    ALTER TABLE xmr_escrow_wallets ENABLE TRIGGER xmr_escrow_wallets_kept; COMMIT;`);
}

test('Monero escrow retention: a wallet past its 90 days destroyed, a later deposit unrecoverable (ES-8), a missing wallet confirmed destroyed on /admin', async ({ browser, baseURL, request }, info) => {
  const admin = await signIn(browser, baseURL, 'xmr_admin', password);
  const buyer = await signIn(browser, baseURL, 'xmr_buyer', buyerPassword);
  const vendor = await signIn(browser, baseURL, 'xmr_vendor', vendorPassword);

  // An order in a wallet of its own, claimed, the claim followed to 100 confirmations; then its wallet closes.
  const kept = await nextWallet();
  const order = await funded(buyer, request, digitalTitle);
  expect(walletOf(order.url)).toBe(kept);
  await claim(vendor, request, order.url);
  await sentAndConfirmed(vendor, request, "The vendor's claim", info, 'k01-claim');
  await mine(request, 100);
  await expect.poll(() => sql(`SELECT count(*) FROM xmr_settlements WHERE order_id='${orderID(order.url)}' AND reorg_safe_at IS NOT NULL`), { timeout: 60_000 }).toBe('1');
  await nextWallet();
  await expect.poll(() => walletState(kept), { timeout: 60_000 }).toBe('retained');

  // 90 days after its last order closed (the order's history and the wallet's dates moved back), with nothing left in
  // it, the watcher destroys it; the order page says that Monero sent to its address since cannot be recovered.
  sql(`UPDATE order_events SET created=created-interval '91 days' WHERE order_id='${orderID(order.url)}'`);
  past90Days(kept);
  await expect.poll(() => walletState(kept), { timeout: 60_000 }).toBe('destroyed');
  await buyer.goto(order.url);
  const lost = buyer.getByRole('status').filter({ hasText: 'Do not send anything to this escrow address.' });
  await expect(lost).toContainText(/Monero sent to this order's address after \d{4}-\d\d-\d\d \d\d:\d\d UTC cannot be recovered: the escrow wallet that held its account was destroyed, 90 days after its last order closed\.$/);
  const deposits = await buyer.locator('.payment-deposits tbody tr').count();
  await deposit(request, order.escrow, 100_000_000_000);
  await mine(request, 10);
  await buyer.waitForTimeout(3_000); // three watcher passes at PAYMENT_POLL_INTERVAL=1s: a destroyed wallet is never opened
  await buyer.reload();
  await expect(lost).toContainText('cannot be recovered: the escrow wallet that held its account was destroyed');
  await expect(buyer.locator('.payment-deposits tbody tr')).toHaveCount(deposits);
  await shot(buyer, info, 'k02-destroyed-order');

  // A wallet with no order, retained when it closes, whose files are gone: missing. Past its 90 days, /admin lists it,
  // and an administrator confirms its destruction by keyboard with the password.
  const gone = await nextWallet();
  await nextWallet();
  await expect.poll(() => walletState(gone), { timeout: 60_000 }).toBe('retained');
  await simPost(request, '/test/xmr/remove-wallet', { name: gone });
  await expect.poll(() => walletState(gone), { timeout: 60_000 }).toBe('missing');
  past90Days(gone);
  await admin.goto('/admin');
  const missing = admin.locator('[aria-labelledby="xmr-escrow-missing"] tbody tr', { hasText: gone });
  await expect(missing).toBeVisible();
  const destroyed = admin.locator('[aria-labelledby="xmr-escrow-destroyed"] tbody tr');
  await expect(destroyed.filter({ hasText: `${kept} ${kept}.keys` })).toBeVisible();
  await shot(admin, info, 'k03-admin-missing');
  const confirm = missing.locator('form[action="/admin/escrow-xmr-destroyed"]');
  await check(admin, confirm.getByRole('checkbox', { name: /this wallet's files are gone, and nothing in it is owed to anyone$/ }));
  await typeInto(admin, confirm.getByLabel('Current password'), 'not-the-admin-password');
  expect(await pressSubmit(admin, confirm.getByRole('button', { name: 'Confirm destruction' }))).toBe(401);
  expect(walletState(gone)).toBe('missing');
  await admin.goto('/admin');
  await check(admin, confirm.getByRole('checkbox', { name: /this wallet's files are gone, and nothing in it is owed to anyone$/ }));
  await typeInto(admin, confirm.getByLabel('Current password'), password);
  expect(await pressSubmit(admin, confirm.getByRole('button', { name: 'Confirm destruction' }))).toBe(303);
  expect(walletState(gone)).toBe('destroyed');
  await expect(admin.locator('[aria-labelledby="xmr-escrow-missing"]')).toHaveCount(0);
  await expect(destroyed.filter({ hasText: `${gone} ${gone}.keys` })).toBeVisible();
  await expect(destroyed.filter({ hasText: `${kept} ${kept}.keys` })).toBeVisible();
  await shot(admin, info, 'k04-admin-destroyed');
});
