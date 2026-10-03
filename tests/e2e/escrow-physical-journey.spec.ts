import { expect, test, type Page } from '@playwright/test';
import { activateKey, context, downloadCopy, password, pressSubmit, register, reloadUntil, settlementStatus, shot, sim, simPost, tabTo, typeInto } from './escrow-helpers';
import { pgpFixture } from './db-helpers';

// WR-escrow Phase 2, slice P2-b (A-247; P2-R1, P2-R2): a physical escrow order released by the buyer's signature, with
// JavaScript disabled at 320 px and every form driven by the keyboard alone, against the real server and PostgreSQL.
// The escrow node is SIMULATED (tests/e2e/fixtures/escrow_rpc.py): its PSBTs and transactions are real bytes, but its
// "signatures" are keyed hashes only the simulator accepts, played for each party's wallet. Not chain evidence (that is
// TestEscrowRegtestPhysical in scripts/test-escrow-regtest.sh). Slice P2-f extends this file to the full journey.
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
  await vendor.keyboard.type('5');
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

  // The vendor signs first (the simulated wallet signs the downloaded copy); the release waits for the buyer.
  const vendorCopy = await downloadCopy(vendor);
  const vendorSigned = await simPost(request, '/test/escrow/sign', { psbt: vendorCopy, fingerprint: vendorKey.slice(1, 9), sighash: 1 });
  await typeInto(vendor, vendor.locator('.escrow-settlement').getByLabel('Signed PSBT'), vendorSigned.psbt);
  expect(await pressSubmit(vendor, vendor.getByRole('button', { name: 'Submit signature' }))).toBe(303);
  await expect(settlementStatus(vendor)).toContainText("Waiting for the buyer's signature");
  await expect(vendor.locator('.page-head .badge')).toHaveText('Shipped');

  // P2-R2: the buyer signs the release on the order page; signing confirms receipt.
  await buyer.goto(orderURL);
  await expect(buyer.locator('.page-head .badge')).toHaveText('Shipped');
  await expect(buyer.locator('body')).toContainText('Who acts next: the buyer signs the release once the goods have arrived');
  await expect(buyer.locator('.escrow-settlement')).toContainText('Signing it confirms that you received the goods');
  await expect(buyer.locator('.escrow-settlement')).toContainText('ends your right to open a dispute');
  await expect(buyer.getByRole('button', { name: 'Confirm receipt and complete' })).toHaveCount(0);
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
