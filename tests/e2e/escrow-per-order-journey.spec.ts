import { expect, test } from '@playwright/test';
import { activateKey, administrator, buyerPassword, context, password, pressSubmit, pressSubmitGet, register, shot, simPost, tabTo, typeInto, vendorPassword } from './escrow-helpers';

// A-455 (shape (b) slice 3; rulings P4-R2, P4-R18, ES-9): a vendor registers a per-order wallet key and activates it by
// signing the practice transaction of a per-order wallet, with JavaScript disabled at 320 px and every form driven by
// the keyboard alone, against the real server and PostgreSQL; a buyer sees the vendor's key type on the listing and the
// vendor page before checkout, and the order page says a per-order wallet cannot take an escrow yet (A-456 builds it).
// The escrow node is SIMULATED (tests/e2e/fixtures/escrow_rpc.py, which derives rooted keys by real BIP32 public
// derivation): its "signatures" are keyed hashes only it accepts, played for the vendor's per-order practice wallet. Not
// chain evidence: TestEscrowRegtestPerOrderPractice in scripts/test-escrow-regtest.sh signs the practice with Core.
const title = 'Escrow per-order wallet licence';

test('Per-order wallet key: registration, practice signing, the key type on the listing and vendor pages', async ({ page: admin, browser, baseURL, request }, info) => {
  await administrator(admin);
  const buyer = await context(browser, baseURL);
  const vendor = await context(browser, baseURL);
  await register(buyer, 'po_buyer', buyerPassword);
  await register(vendor, 'po_vendor', vendorPassword);
  await admin.goto('/admin');
  const role = admin.locator('form', { has: admin.getByRole('button', { name: 'Update role' }) });
  await typeInto(admin, role.getByLabel('Account handle'), 'po_vendor');
  await tabTo(admin, role.getByLabel('Role'));
  await role.getByLabel('Role').press('v');
  await expect(role.getByLabel('Role')).toHaveValue('vendor');
  await typeInto(admin, role.getByLabel('Current password'), password);
  await pressSubmit(admin, role.getByRole('button', { name: 'Update role' }));
  await expect(admin.getByRole('status').filter({ hasText: 'Account role saved.' })).toBeVisible();

  // A digital listing with automatic delivery (one that takes escrow).
  await vendor.goto('/vendor-dashboard');
  await tabTo(vendor, vendor.getByText('+ Publish a new listing', { exact: true }));
  await vendor.keyboard.press('Enter');
  await typeInto(vendor, vendor.getByLabel('Listing title'), title);
  await typeInto(vendor, vendor.getByLabel('Description', { exact: true }), 'Simulated escrow node; no real coins.');
  await tabTo(vendor, vendor.getByLabel('Fulfillment type'));
  await vendor.getByLabel('Fulfillment type').press('d');
  await expect(vendor.getByLabel('Fulfillment type')).toHaveValue('digital');
  await tabTo(vendor, vendor.getByLabel('Stock', { exact: true }));
  await vendor.keyboard.press('ControlOrMeta+A');
  await vendor.keyboard.type('5');
  await typeInto(vendor, vendor.getByLabel(/Price \(BTC, (?:test network|main network|not configured|not yet known)\)/), '0.001');
  await typeInto(vendor, vendor.getByLabel('Monero reference price'), '0.5');
  await typeInto(vendor, vendor.getByLabel('Automatic delivery content'), 'LICENCE-PER-ORDER-0001');
  await pressSubmit(vendor, vendor.getByRole('button', { name: 'Publish listing' }));

  // Before the vendor has an active key, the listing says so.
  const openListing = async () => {
    await buyer.goto(`/?q=${encodeURIComponent(title)}`);
    await tabTo(buyer, buyer.getByRole('link', { name: title, exact: true }));
    await Promise.all([buyer.waitForEvent('load'), buyer.keyboard.press('Enter')]);
  };
  await openListing();
  const keyType = buyer.locator('.key-values div').filter({ has: buyer.getByText("Vendor's Bitcoin escrow key", { exact: true }) }).locator('dd');
  await expect(keyType).toHaveText(/^None active/);

  // The vendor registers a per-order wallet key: the layout is chosen with the arrow keys in its radio group.
  const { key } = await simPost(request, '/test/escrow/key', { name: 'po-vendor' });
  await vendor.goto('/escrow-key');
  await expect(vendor.getByText("Electrum's own seed type", { exact: false })).toBeVisible();
  const core = vendor.getByLabel('Bitcoin Core: one wallet signs every escrow');
  const perOrder = vendor.getByLabel('A per-order wallet: Electrum with a BIP39 seed, or Sparrow');
  await expect(core).toBeChecked();
  await tabTo(vendor, core);
  await vendor.keyboard.press('ArrowDown');
  await expect(perOrder).toBeChecked();
  await typeInto(vendor, vendor.getByLabel('Extended public key with its origin'), key);
  await typeInto(vendor, vendor.getByLabel('Current password'), vendorPassword);
  expect(await pressSubmit(vendor, vendor.getByRole('button', { name: 'Register key' }))).toBe(303);
  await expect(vendor.locator('.page-head .badge')).toHaveText('Inactive');
  const typeRow = vendor.locator('.key-values div').filter({ has: vendor.getByText('Key type', { exact: true }) }).locator('dd');
  await expect(typeRow).toHaveText('Per-order wallet');
  await vendor.reload();
  await expect(perOrder).toBeChecked();
  await shot(vendor, info, 'po-01-registered');

  // The practice: the practice wallet's path, two rooted cosigner keys, its first address and the PSBT.
  await pressSubmitGet(vendor, vendor.getByRole('button', { name: 'Show the practice transaction' }));
  await expect(vendor.locator('.escrow-practice-path')).toHaveText(`[${key.slice(1, 9)}/48h/1h/0h/2h/2147483647]`);
  await expect(vendor.locator('.escrow-practice-derivation')).toHaveText('m/48h/1h/0h/2h/2147483647');
  await expect(vendor.locator('.escrow-cosigner')).toHaveCount(2);
  for (const c of await vendor.locator('.escrow-cosigner').allInnerTexts()) expect(c.trim()).toMatch(/^Vpub[1-9A-HJ-NP-Za-km-z]{107}$/);
  await expect(vendor.locator('.escrow-practice-address')).toHaveText(/^bcrt1q[0-9a-z]{58}$/);
  const practice = (await vendor.locator('#practice textarea[readonly]').inputValue()).trim();
  expect(practice).toMatch(/^cHNidP8/);
  await shot(vendor, info, 'po-02-practice');

  // A signature with another sighash type is refused and nothing changes; the wallet's ALL signature activates the key.
  const { psbt: none } = await simPost(request, '/test/escrow/sign', { psbt: practice, fingerprint: key.slice(1, 9), sighash: 2 });
  await typeInto(vendor, vendor.getByLabel('Signed practice PSBT'), none);
  expect(await pressSubmit(vendor, vendor.getByRole('button', { name: 'Check signature' }))).toBe(400);
  await expect(vendor.locator('body')).toContainText('The signature uses a sighash type other than ALL.'); // a plain-text refusal (A-59), not measured at 320 px
  await vendor.goto('/escrow-key');
  await expect(vendor.locator('.page-head .badge')).toHaveText('Inactive');
  await pressSubmitGet(vendor, vendor.getByRole('button', { name: 'Show the practice transaction' }));
  const { psbt } = await simPost(request, '/test/escrow/sign', { psbt: practice, fingerprint: key.slice(1, 9), sighash: 1 });
  await typeInto(vendor, vendor.getByLabel('Signed practice PSBT'), psbt);
  expect(await pressSubmit(vendor, vendor.getByRole('button', { name: 'Check signature' }))).toBe(303);
  await expect(vendor.locator('.page-head .badge')).toHaveText('Active');
  await expect(typeRow).toHaveText('Per-order wallet');
  await expect(vendor.locator('#practice')).toHaveCount(0);
  await shot(vendor, info, 'po-03-active');
  await vendor.goto('/account');
  await expect(vendor.locator('.key-values div').filter({ has: vendor.getByText('Bitcoin escrow key', { exact: true }) })).toContainText('(per-order wallet)');

  // The buyer sees the vendor's key type on the listing and the vendor page.
  await openListing();
  await expect(keyType).toHaveText(/^Per-order wallet/);
  await shot(buyer, info, 'po-04-listing-key-type');
  await tabTo(buyer, buyer.getByRole('link', { name: 'po_vendor', exact: true }));
  await Promise.all([buyer.waitForEvent('load'), buyer.keyboard.press('Enter')]);
  await expect(buyer.getByText(/^Bitcoin escrow key: Per-order wallet/)).toBeVisible();
  await shot(buyer, info, 'po-05-vendor-key-type');

  // A buyer with an active Bitcoin Core key: the draft's order page names the vendor's per-order wallet key beside the
  // buyer's Bitcoin Core key (a mixed pair, P4-R18, A-456a), and offers no escrow form.
  await activateKey(buyer, request, 'po-buyer', buyerPassword, info);
  await openListing();
  await tabTo(buyer, buyer.getByRole('link', { name: 'Review order draft' }));
  await Promise.all([buyer.waitForEvent('load'), buyer.keyboard.press('Enter')]);
  await pressSubmit(buyer, buyer.getByRole('button', { name: 'Create unfunded draft' }));
  await buyer.waitForURL(/\/order\?id=/);
  await expect(buyer.getByText("The vendor's Bitcoin escrow key is for a per-order wallet", { exact: false })).toBeVisible();
  await expect(buyer.getByRole('button', { name: 'Pay into an escrow' })).toHaveCount(0);
  await shot(buyer, info, 'po-06-order-per-order-vendor');
});
