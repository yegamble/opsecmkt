import { expect, test } from '@playwright/test';
import { activateKey, buyerPassword, check, context, downloadCopy, password, pressSubmit, register, reloadUntil, settlementStatus, shot, sim, simPost, tabTo, typeInto, vendorPassword } from './escrow-helpers';

// WR-escrow Phase 1 (A-246): the Bitcoin escrow paste journey with JavaScript disabled at 320 px, every form driven by
// the keyboard alone, against the real server and PostgreSQL. The escrow node is SIMULATED
// (tests/e2e/fixtures/escrow_rpc.py): its keys, PSBTs and transactions are real bytes, but its "signatures" are
// keyed hashes only the simulator accepts, the signed PSBTs pasted here come from the simulator playing each party's
// wallet, and no chain exists. This proves the application's forms, checks, order states and pages end to end; it is
// not chain evidence (that is scripts/test-escrow-regtest.sh, on real Bitcoin Core regtest).

test('Bitcoin escrow paste journey: keys, checkout and funding, completion, the vendor\'s claim, a refused paste and a refund', async ({ page: admin, browser, baseURL, request }, info) => {
  // The marketplace, an administrator, a buyer and a vendor.
  await admin.goto('/setup');
  await typeInto(admin, admin.getByLabel('Setup token'), 'e2e-escrow-local-setup-token-at-least-32-characters');
  await typeInto(admin, admin.getByLabel('Handle', { exact: true }), 'escrow_admin');
  await typeInto(admin, admin.getByLabel('Password', { exact: true }), password);
  await pressSubmit(admin, admin.getByRole('button', { name: 'Initialize marketplace' }));
  await expect(admin).toHaveURL(/\/admin\?welcome=1$/);
  await typeInto(admin, admin.locator('.admin-auth').getByLabel('Current password'), password);
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
  await typeInto(admin, role.getByLabel('Current password'), password);
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
  await vendor.keyboard.press('ControlOrMeta+A');
  await vendor.keyboard.type('5');
  await typeInto(vendor, vendor.getByLabel(/Price \(BTC, (?:test network|main network|not configured|not yet known)\)/), '0.001');
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
    await expect(buyer.getByLabel('Pay with')).toHaveValue('BTC');
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
  await expect(vendor.getByRole('alert')).toHaveText('A signature uses a sighash type other than ALL. Sign with the default sighash type (ALL). Nothing was changed.');
  // The shared refusal page (A-514) keeps the pasted PSBT, read-only, to copy.
  await expect(vendor.getByLabel('Signed PSBT')).toHaveValue(none.psbt);
  await vendor.screenshot({ path: info.outputPath('08-refused-paste.png'), fullPage: true });
  await vendor.goto(first.url);
  await expect(settlementStatus(vendor)).toContainText('Waiting for a signature');

  // The good paste: party_signed, then the market co-signs and broadcasts it, then it confirms.
  const signed = await simPost(request, '/test/escrow/sign', { psbt: copy, fingerprint: vendorKey.slice(1, 9), sighash: 1 });
  await typeInto(vendor, vendor.locator('.escrow-settlement').getByLabel('Signed PSBT'), signed.psbt);
  expect(await pressSubmit(vendor, vendor.getByRole('button', { name: 'Submit signature' }))).toBe(303);
  await expect(settlementStatus(vendor)).toContainText(/Signed by the vendor|Signed by the market, with the vendor's signature|Broadcast to the regtest network/);
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
