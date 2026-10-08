import { writeFileSync } from 'node:fs';
import { expect, test, type APIRequestContext, type Page, type TestInfo } from '@playwright/test';
import { administrator, buyerPassword, context, downloadCopy, pressSubmit, pressSubmitGet, register, reloadUntil, settlementStatus, shot, sim, simPost, tabTo, typeInto, vendorPassword } from './escrow-helpers';

// A-456b (rulings P4-R2, P4-R17, P4-R18): a per-order (shape (b)) Bitcoin escrow end to end with JavaScript disabled at
// 320 px, every form driven by the keyboard alone, against the real server and PostgreSQL: both parties register a
// per-order wallet key and sign its practice transaction, the buyer pays into a per-order escrow, each party reads its
// own order wallet setup page, the escrow is funded (simulated), the vendor claims it and signs the release by
// uploading its signed PSBT file, the market co-signs it through the escrow signer and it confirms. The escrow node and
// the escrow signer are SIMULATED (tests/e2e/fixtures/escrow_rpc.py: per-order keys by real BIP32 public derivation,
// keyed-hash "signatures" only it accepts; the signer makes cmd/escrow-signer's refusals). Not chain evidence:
// TestEscrowRegtestShapeBRelease in scripts/test-escrow-regtest.sh runs Core and the real signer.
const title = 'Per-order escrow settle licence';

// Registers a per-order wallet key (the layout chosen with the arrow keys) and signs its practice in the simulated wallet.
async function perOrderKey(page: Page, request: APIRequestContext, name: string, pass: string, info: TestInfo): Promise<string> {
  const { key } = await simPost(request, '/test/escrow/key', { name });
  await page.goto('/escrow-key');
  await tabTo(page, page.getByLabel('Bitcoin Core: one wallet signs every escrow'));
  await page.keyboard.press('ArrowDown');
  await expect(page.getByLabel('A per-order wallet: Electrum with a BIP39 seed, or Sparrow')).toBeChecked();
  await typeInto(page, page.getByLabel('Extended public key with its origin'), key);
  await typeInto(page, page.getByLabel('Current password'), pass);
  expect(await pressSubmit(page, page.getByRole('button', { name: 'Register key' }))).toBe(303);
  await pressSubmitGet(page, page.getByRole('button', { name: 'Show the practice transaction' }));
  const practice = (await page.locator('#practice textarea[readonly]').inputValue()).trim();
  const { psbt } = await simPost(request, '/test/escrow/sign', { psbt: practice, fingerprint: key.slice(1, 9), sighash: 1 });
  await typeInto(page, page.getByLabel('Signed practice PSBT'), psbt);
  expect(await pressSubmit(page, page.getByRole('button', { name: 'Check signature' }))).toBe(303);
  await expect(page.locator('.page-head .badge')).toHaveText('Active');
  await shot(page, info, `${name}-01-per-order-key-active`);
  return key;
}

// Opens the order wallet setup page from the order page with the keyboard and checks what it shows the party.
async function setupPage(page: Page, key: string, address: string, info: TestInfo, name: string) {
  await tabTo(page, page.getByRole('link', { name: 'Set up your order wallet' }));
  await Promise.all([page.waitForEvent('load'), page.keyboard.press('Enter')]);
  await expect(page).toHaveURL(/\/escrow-setup\?id=/);
  await expect(page.locator('.escrow-setup-path')).toHaveText(new RegExp(`^\\[${key.slice(1, 9)}/48h/1h/0h/2h/[0-9]+\\]$`));
  await expect(page.locator('.escrow-setup-derivation')).toHaveText(/^m\/48h\/1h\/0h\/2h\/[0-9]+$/);
  await expect(page.locator('.escrow-cosigner')).toHaveCount(2);
  for (const c of await page.locator('.escrow-cosigner').allInnerTexts()) expect(c.trim()).toMatch(/^Vpub[1-9A-HJ-NP-Za-km-z]{107}$/);
  await expect(page.locator('.escrow-setup-address')).toHaveText(address);
  await expect(page.locator('.escrow-setup-sparrow')).toHaveText(/^wsh\(sortedmulti\(2,.*\/<0;1>\/\*\)\)#[0-9a-z]{8}$/);
  await expect(page.getByText('Pay only to the escrow address', { exact: false })).toBeVisible();
  await shot(page, info, name);
  await page.goBack();
}

test('Per-order escrow: keys, checkout, setup pages, funding, the release uploaded and co-signed by the escrow signer, confirmed', async ({ page: admin, browser, baseURL, request }, info) => {
  await administrator(admin);
  const buyer = await context(browser, baseURL);
  const vendor = await context(browser, baseURL);
  await register(buyer, 'pos_buyer', buyerPassword);
  await register(vendor, 'pos_vendor', vendorPassword);
  await admin.goto('/admin');
  const role = admin.locator('form', { has: admin.getByRole('button', { name: 'Update role' }) });
  await typeInto(admin, role.getByLabel('Account handle'), 'pos_vendor');
  await tabTo(admin, role.getByLabel('Role'));
  await role.getByLabel('Role').press('v');
  await expect(role.getByLabel('Role')).toHaveValue('vendor');
  await pressSubmit(admin, role.getByRole('button', { name: 'Update role' }));
  await expect(admin.getByRole('status').filter({ hasText: 'Account role saved.' })).toBeVisible();

  await vendor.goto('/vendor-dashboard');
  await tabTo(vendor, vendor.getByText('+ Publish a new listing', { exact: true }));
  await vendor.keyboard.press('Enter');
  await typeInto(vendor, vendor.getByLabel('Listing title'), title);
  await typeInto(vendor, vendor.getByLabel('Description', { exact: true }), 'Simulated escrow node and signer; no real coins.');
  await tabTo(vendor, vendor.getByLabel('Fulfillment type'));
  await vendor.getByLabel('Fulfillment type').press('d');
  await expect(vendor.getByLabel('Fulfillment type')).toHaveValue('digital');
  await tabTo(vendor, vendor.getByLabel('Stock', { exact: true }));
  await vendor.keyboard.press('ControlOrMeta+A');
  await vendor.keyboard.type('5');
  await typeInto(vendor, vendor.getByLabel('Bitcoin reference price'), '0.001');
  await typeInto(vendor, vendor.getByLabel('Monero reference price'), '0.5');
  await typeInto(vendor, vendor.getByLabel('Automatic delivery content'), 'LICENCE-PER-ORDER-SETTLE-0001');
  await pressSubmit(vendor, vendor.getByRole('button', { name: 'Publish listing' }));

  // Register and practice: both parties' keys are per-order wallet keys.
  const buyerKey = await perOrderKey(buyer, request, 'pos-buyer', buyerPassword, info);
  const vendorKey = await perOrderKey(vendor, request, 'pos-vendor', vendorPassword, info);

  // Checkout: a per-order escrow.
  await buyer.goto(`/?q=${encodeURIComponent(title)}`);
  await tabTo(buyer, buyer.getByRole('link', { name: title, exact: true }));
  await Promise.all([buyer.waitForEvent('load'), buyer.keyboard.press('Enter')]);
  await tabTo(buyer, buyer.getByRole('link', { name: 'Review order draft' }));
  await Promise.all([buyer.waitForEvent('load'), buyer.keyboard.press('Enter')]);
  await pressSubmit(buyer, buyer.getByRole('button', { name: 'Create unfunded draft' }));
  await buyer.waitForURL(/\/order\?id=/);
  const { address: refund } = await simPost(request, '/test/escrow/address', {});
  await typeInto(buyer, buyer.getByLabel('Refund address'), refund);
  expect(await pressSubmit(buyer, buyer.getByRole('button', { name: 'Pay into an escrow' }))).toBe(303);
  await expect(buyer.locator('.page-head .badge')).toHaveText('Awaiting payment');
  const orderURL = buyer.url();
  const address = (await buyer.locator('.payment-address .mono').innerText()).trim();
  expect(address).toMatch(/^bcrt1q[0-9a-z]{58}$/);
  await expect(buyer.locator('#escrow-order-wallet')).toContainText('Pay only to the escrow address above');
  await shot(buyer, info, 'pos-02-awaiting-payment');

  // Each party's own setup page.
  await setupPage(buyer, buyerKey, address, info, 'pos-03-setup-buyer');
  await vendor.goto(orderURL);
  await setupPage(vendor, vendorKey, address, info, 'pos-04-setup-vendor');

  // Funding (simulated): the full price, three blocks; the digital order completes.
  await simPost(request, '/test/escrow/deposit', { address, sats: 100_000 });
  await simPost(request, '/test/escrow/mine', { blocks: 3 });
  await reloadUntil(buyer, async () => { await expect(buyer.locator('.page-head .badge')).toHaveText('Completed', { timeout: 500 }); });

  // The vendor's claim, built by the market.
  await vendor.goto(orderURL);
  const { address: claimTo } = await simPost(request, '/test/escrow/address', {});
  await typeInto(vendor, vendor.getByLabel('Claim address'), claimTo);
  await typeInto(vendor, vendor.locator('.escrow-claim').getByLabel('Current password'), vendorPassword);
  expect(await pressSubmit(vendor, vendor.getByRole('button', { name: 'Request the claim' }))).toBe(303);
  await reloadUntil(vendor, async () => { await expect(settlementStatus(vendor)).toContainText('Waiting for a signature', { timeout: 500 }); });
  await expect(vendor.locator('.escrow-settlement')).toContainText("Load it into this order's wallet");
  await shot(vendor, info, 'pos-05-claim-awaiting-signature');

  // The vendor signs in its order wallet (simulated) and uploads the binary PSBT file; the paste stays empty.
  const copy = await downloadCopy(vendor);
  const signed = await simPost(request, '/test/escrow/sign', { psbt: copy, fingerprint: vendorKey.slice(1, 9), sighash: 1 });
  const file = info.outputPath('vendor-signed.psbt');
  writeFileSync(file, Buffer.from(signed.psbt, 'base64'));
  const upload = vendor.locator('.escrow-settlement').getByLabel('Or upload it as a file');
  await tabTo(vendor, upload);
  await upload.setInputFiles(file);
  expect(await pressSubmit(vendor, vendor.getByRole('button', { name: 'Submit signature' }))).toBe(303);
  await expect(settlementStatus(vendor)).toContainText(/Signed by the vendor|Broadcast to the regtest network/);

  // The market co-signs through the escrow signer and broadcasts; it confirms.
  await reloadUntil(vendor, async () => { await expect(settlementStatus(vendor)).toContainText('Broadcast to the regtest network: transaction', { timeout: 500 }); });
  await shot(vendor, info, 'pos-06-claim-broadcast');
  await simPost(request, '/test/escrow/mine', { blocks: 3 });
  await reloadUntil(vendor, async () => { await expect(settlementStatus(vendor)).toContainText('Confirmed on the regtest network: transaction', { timeout: 500 }); });
  await shot(vendor, info, 'pos-07-claim-confirmed');
  await buyer.goto(orderURL);
  await expect(settlementStatus(buyer)).toContainText('Confirmed on the regtest network: transaction');

  // The simulated node saw this escrow's settlement broadcast, spending the escrow's deposit.
  const state = await (await request.get(`${sim}/test/escrow/state`)).json();
  const deposit = state.deposits.find((d: { address: string }) => d.address === address);
  expect(deposit).toBeTruthy();
  expect(Object.values(state.sent).length).toBeGreaterThan(0);
});
