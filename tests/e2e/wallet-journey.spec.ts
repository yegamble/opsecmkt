import { expect, test, type Page } from '@playwright/test';
import { expectNotice, pgpFixture, setRole, signedIn, submitAndLoad } from './db-helpers';
import { waitForStyles } from './styles';

// Real server/PostgreSQL and production RPC adapters, deterministic simulated wallet responses.
// This proves application integration, not real-chain settlement or cryptographic address validation.
const password = 'wallet-browser-admin-password-123';
const vendorPassword = 'wallet-browser-vendor-password-123';
const rpc = 'http://127.0.0.1:18083';
// The dispute extends the marketplace and accounts initialized by the first journey.
test.describe.configure({ mode: 'serial' });
// Submit a form and wait for the response page, so a following reload (of this or another page) cannot
// race the in-flight POST and its navigation.
async function submit(page: Page, button: ReturnType<Page['getByRole']>) {
  await Promise.all([page.waitForEvent('load'), button.click()]);
}
async function reloadUntil(page: Page, assertion: () => Promise<void>) {
  await expect(async () => { await page.reload(); await assertion(); }).toPass({ timeout: 20_000, intervals: [300, 500, 1000] });
}
// A-115: real 64-hex order IDs, txids and deposit tables never widen the page at phone or desktop width (tables
// scroll inside .table-wrap). Checks 390 px (the project viewport), then 320 and 1280 px, and restores the size.
async function fitsAt(page: Page, label: string) {
  const original = page.viewportSize()!;
  for (const width of [390, 320, 1280]) {
    await page.setViewportSize({ width, height: original.height });
    await waitForStyles(page);
    expect(await page.evaluate(() => document.documentElement.scrollWidth), `${label} at ${width}px`).toBeLessThanOrEqual(width);
  }
  await page.setViewportSize(original);
}

test('BTC and XMR: partial payment, intake pause, confirmation, fulfillment and one payout', async ({ page: admin, browser, baseURL, request }) => {
  await admin.goto('/setup');
  await admin.getByLabel('Setup token').fill('e2e-wallet-local-setup-token-at-least-32-characters');
  await admin.getByLabel('Handle', { exact: true }).fill('wallet_admin');
  await admin.getByLabel('Password', { exact: true }).fill(password);
  await admin.getByRole('button', { name: 'Initialize marketplace' }).click();
  await expect(admin).toHaveURL(/\/admin\?welcome=1$/);
  await admin.locator('.admin-auth').getByLabel('Current password').fill(password);
  await admin.locator('.admin-auth').getByRole('button', { name: 'Turn CAPTCHA off' }).click();
  const buyer = await signedIn(browser, baseURL, 'wallet_buyer', 'wallet-browser-buyer-password-123', true);
  const second = await signedIn(browser, baseURL, 'wallet_second', 'wallet-browser-second-password-123', true);
  // The digital XMR listing belongs to wallet_vendor; wallet_admin is the BTC vendor, which must also add a PGP key for
  // its physical listing (A-83). (Correct password confirmations spend no per-account budget since A-274.)
  const vendor = await signedIn(browser, baseURL, 'wallet_vendor', vendorPassword, true);
  await setRole(admin, 'wallet_vendor', 'vendor', password);

  for (const currency of ['BTC', 'XMR']) {
    const digital = currency === 'XMR';
    const [seller, sellerHandle, sellerPassword] = digital ? [vendor, 'wallet_vendor', vendorPassword] : [admin, 'wallet_admin', password];
    const destination = digital ? '5' + 'a'.repeat(94) : 'bcrt1qfixturepayout000000000000';
    await seller.goto('/account');
    const payout = seller.locator('form', { has: seller.getByRole('button', { name: `Save ${currency} address` }) });
    await payout.locator('input[name="address"]').fill(destination);
    await payout.getByLabel('Current password', { exact: true }).fill(sellerPassword);
    await payout.getByRole('button').click();
    await expect(payout.locator('input[name="address"]')).toHaveValue(destination);
    await seller.goto('/vendor-dashboard');
    await seller.getByText('+ Publish a new listing', { exact: true }).click();
    const title = `Wallet ${currency} journey`;
    await seller.getByLabel('Listing title').fill(title);
    await seller.getByLabel('Description', { exact: true }).fill('Deterministic RPC fixture journey; no real coins.');
    await seller.getByLabel('Fulfillment type').selectOption(digital ? 'digital' : 'physical');
    await seller.getByLabel('Stock', { exact: true }).fill('3');
    await seller.getByLabel(/Price \(BTC, (?:test network|main network|not configured|not yet known)\)/).fill('0.001');
    await seller.getByLabel('Monero reference price').fill('0.5');
    await submitAndLoad(seller, seller.getByRole('button', { name: 'Publish listing' }));
    await buyer.goto(`/?q=${encodeURIComponent(title)}`);
    await buyer.getByRole('link', { name: title, exact: true }).click();
    await buyer.waitForURL(/\/product\?id=/);
    const productURL = buyer.url();
    // A-83: neither vendor has a PGP key yet. The physical listing says a shipping address could not be sent.
    const noKey = 'has no usable encryption key, so you could not send a shipping address';
    if (digital) await expect(buyer.locator('main')).not.toContainText(noKey);
    else await expect(buyer.locator('.order-unavailable')).toContainText(`No usable key. wallet_admin ${noKey}`);
    await buyer.getByRole('link', { name: 'Review order draft' }).click();
    if (!digital) await expect(buyer.locator('.order-unavailable')).toContainText(noKey);
    await buyer.getByLabel('Pay with').selectOption(currency);
    await buyer.getByRole('button', { name: 'Create unfunded draft' }).click();
    // Measure the draft page, not the checkout page mid-navigation (CI 36261986487 measured 504 px unstyled).
    await buyer.waitForURL(/\/order\?id=/);
    await fitsAt(buyer, `${currency} draft`);
    if (!digital) {
      // No payment is offered until the vendor adds a key that can receive the encrypted address.
      await expect(buyer.locator('.order-unavailable')).toHaveText('Unavailable. This vendor has no usable encryption key, so you could not send your shipping address. Ask them to add one.');
      await expect(buyer.getByRole('button', { name: 'Request payment address' })).toHaveCount(0);
      await admin.goto('/account');
      const profile = admin.locator('form', { has: admin.getByRole('button', { name: 'Save profile' }) });
      await profile.getByLabel('PGP public key', { exact: true }).fill(pgpFixture('recipient.pub.asc'));
      await profile.getByLabel('Current password', { exact: true }).fill(password); // A-152
      await submit(admin, profile.getByRole('button', { name: 'Save profile' }));
      await buyer.reload();
      await expect(buyer.locator('.order-unavailable')).toHaveCount(0);
    }
    await buyer.getByRole('button', { name: 'Request payment address' }).click();
    await expectNotice(buyer, 'Payment address ready.');
    const orderURL = buyer.url();
    await expect(buyer.locator('.page-head .badge')).toHaveText('Awaiting payment');
    await fitsAt(buyer, `${currency} awaiting payment`);
    const address = (await buyer.locator('.payment-address .mono').innerText()).trim();
    await admin.goto('/admin');
    const pause = admin.locator('form', { has: admin.getByRole('button', { name: `Save new ${currency} payments` }) });
    await pause.getByRole('radio', { name: /^Paused/ }).check();
    await pause.getByLabel('Current password', { exact: true }).fill(password);
    await pause.getByRole('button').click();
    await expect(admin.getByRole('heading', { name: `${currency} intake: Paused` })).toBeVisible();
    await expect(admin.getByRole('heading', { name: `${digital ? 'BTC' : 'XMR'} intake: Enabled when wallet is available` })).toBeVisible();
    await second.goto(productURL);
    await second.getByRole('link', { name: 'Review order draft' }).click();
    await second.getByLabel('Pay with').selectOption(currency);
    await second.getByRole('button', { name: 'Create unfunded draft' }).click();
    await expect(second.getByRole('heading', { name: 'New payments paused' })).toBeVisible();
    await expect(second.getByRole('button', { name: 'Request payment address' })).toHaveCount(0);
    const total = digital ? 500_000_000_000 : 100_000;
    expect((await request.post(`${rpc}/test/deposit`, { data: { address, amount: total * 0.4 } })).ok()).toBeTruthy();
    const remaining = buyer.locator('.payment-figures div').filter({ has: buyer.getByText('Remaining to send', { exact: true }) });
    await reloadUntil(buyer, async () => { await expect(remaining).toContainText(digital ? '0.3' : '0.0006', { timeout: 500 }); });
    await expect(buyer.locator('.page-head .badge')).toHaveText('Awaiting payment');
    await expect(buyer.locator('.payment-deposits tbody tr')).toHaveCount(1);
    await fitsAt(buyer, `${currency} partial deposit`);
    expect((await request.post(`${rpc}/test/deposit`, { data: { address, amount: total * 0.6 } })).ok()).toBeTruthy();
    await reloadUntil(buyer, async () => { await expect(buyer.getByText('Do not send another payment', { exact: false })).toBeVisible({ timeout: 500 }); });
    await expect(buyer.locator('.page-head .badge')).toHaveText('Awaiting payment');
    expect((await request.post(`${rpc}/test/confirm`, { data: { address } })).ok()).toBeTruthy();
    await reloadUntil(buyer, async () => { await expect(buyer.locator('.page-head .badge')).toHaveText('Paid', { timeout: 500 }); });
    await fitsAt(buyer, `${currency} paid (buyer)`);
    // Existing addresses were monitored while intake was paused; now resume new requests.
    const resume = admin.locator('form', { has: admin.getByRole('button', { name: `Save new ${currency} payments` }) });
    await resume.getByRole('radio', { name: /^On:/ }).check();
    await resume.getByLabel('Current password', { exact: true }).fill(password);
    await submit(admin, resume.getByRole('button'));
    await second.reload();
    await expect(second.getByRole('button', { name: 'Request payment address' })).toBeVisible();
    // The paid physical order asks for the address, encrypted to the vendor's key (A-83: the key is usable).
    if (!digital) await expect(buyer.getByRole('link', { name: 'Send encrypted shipping details ↗' })).toBeVisible();
    await seller.goto(orderURL);
    await fitsAt(seller, `${currency} paid (vendor)`);
    if (digital) {
      await seller.getByLabel('Delivery content', { exact: true }).fill('Fixture digital delivery token');
      await submit(seller, seller.getByRole('button', { name: 'Deliver digital content', exact: true }));
    } else {
      // A-80: the note to the buyer warns against addresses and tracking numbers in plain text.
      await expect(seller.getByLabel('Note to the buyer (optional)')).toHaveAccessibleDescription(/^Stored unencrypted\..*Never include an address, real name or tracking number/);
      await submit(seller, seller.getByRole('button', { name: 'Mark as shipped' }));
    }
    await buyer.reload();
    if (digital) await expect(buyer.getByRole('region', { name: 'Digital delivery' })).toContainText('Fixture digital delivery token');
    // A-122: the complete form states the release (amount, recipient, difference from the price) and refuses
    // to complete until it is ticked.
    const release = buyer.getByRole('checkbox', { name: `Release ${digital ? '0.5 XMR' : '0.001 BTC'} (test network) to ${sellerHandle} — exactly the price; network fee deducted. This is final.`, exact: true });
    await expect(release).toHaveAttribute('required', '');
    await expect(buyer.locator('input[name="amount_seen"]')).toHaveValue(String(total));
    await release.check();
    await buyer.getByRole('button', { name: 'Confirm receipt and complete' }).click();
    await expect(buyer.locator('.page-head .badge')).toHaveText('Completed');
    await expect.poll(async () => ((await (await request.get(`${rpc}/test/state`)).json()).payouts as any[]).filter(p => p.currency === currency).length).toBe(1);
    const state = await (await request.get(`${rpc}/test/state`)).json();
    // A-232: the release, on either coin, is sent with the fee taken from it: the vendor receives the price less the fee.
    const [sent] = state.payouts.filter((p: any) => p.currency === currency);
    expect(sent).toEqual(expect.objectContaining({ address: destination, amount: total, fee_from_amount: true }));
    expect(sent.fee).toBeGreaterThan(0);
    expect(sent.received + sent.fee).toBe(total);
    await reloadUntil(buyer, async () => { await expect(buyer.locator('.payment-figures div').filter({ has: buyer.getByText('Payout status', { exact: true }) })).toContainText('Sent', { timeout: 500 }); });
    await buyer.reload();
    await fitsAt(buyer, `${currency} completed`);
    const checks = (await (await request.get(`${rpc}/test/state`)).json()).checks;
    await expect.poll(async () => (await (await request.get(`${rpc}/test/state`)).json()).checks).toBeGreaterThan(checks + 2);
    expect(((await (await request.get(`${rpc}/test/state`)).json()).payouts as any[]).filter(p => p.currency === currency)).toHaveLength(1);
    await buyer.getByRole('combobox', { name: 'Rating', exact: true }).selectOption('4');
    await expect(buyer.getByLabel('Review (optional)')).toHaveAccessibleDescription(/The vendor sees your review, with your handle, on this order\..*Never include an address, real name or tracking number/);
    await buyer.getByLabel('Review (optional)').fill(`Verified ${currency} fixture purchase arrived as described.`);
    await submit(buyer, buyer.getByRole('button', { name: 'Publish review' }));
    await buyer.reload();
    await expect(buyer.getByRole('button', { name: 'Publish review' })).toHaveCount(0);
    const publicContext = await browser.newContext({ baseURL, javaScriptEnabled: false });
    const publicPage = await publicContext.newPage();
    await publicPage.goto(productURL);
    await expect(publicPage.locator('main')).toContainText(`Verified ${currency} fixture purchase arrived as described.`);
    await expect(publicPage.locator('main')).toContainText('Completed order (test network)');
    await expect(publicPage.locator('main')).not.toContainText('wallet_buyer');
    await expect(publicPage.locator('main')).not.toContainText(new URL(orderURL).searchParams.get('id')!);
    await publicContext.close();
  }
  await buyer.context().close();
  await second.context().close();
  await vendor.context().close();
});

test('funded dispute: encrypted moderator evidence, independent resolution and one buyer refund', async ({ browser, baseURL, request }) => {
  const admin = await signedIn(browser, baseURL, 'wallet_admin', password, false);
  const buyer = await signedIn(browser, baseURL, 'wallet_buyer', 'wallet-browser-buyer-password-123', false);
  const moderator = await signedIn(browser, baseURL, 'wallet_moderator', 'wallet-browser-moderator-password-123', true);
  await setRole(admin, 'wallet_moderator', 'moderator', password);
  await moderator.goto('/account');
  const profile = moderator.locator('form', { has: moderator.getByRole('button', { name: 'Save profile' }) });
  await profile.getByLabel('PGP public key', { exact: true }).fill(pgpFixture('recipient.pub.asc'));
  await profile.getByLabel('Current password', { exact: true }).fill('wallet-browser-moderator-password-123'); // A-152
  await submitAndLoad(moderator, profile.getByRole('button', { name: 'Save profile' }));
  await buyer.goto('/account');
  const refundAddress = 'bcrt1qfixturebuyerrefund00000000';
  const addressForm = buyer.locator('form', { has: buyer.getByRole('button', { name: 'Save BTC address' }) });
  await addressForm.locator('input[name="address"]').fill(refundAddress);
  await addressForm.getByLabel('Current password', { exact: true }).fill('wallet-browser-buyer-password-123');
  await submitAndLoad(buyer, addressForm.getByRole('button'));
  await buyer.goto('/?q=Wallet%20BTC%20journey');
  await buyer.getByRole('link', { name: 'Wallet BTC journey', exact: true }).click();
  await buyer.getByRole('link', { name: 'Review order draft' }).click();
  await buyer.getByRole('button', { name: 'Create unfunded draft' }).click();
  await buyer.getByRole('button', { name: 'Request payment address' }).click();
  await expectNotice(buyer, 'Payment address ready.');
  const orderURL = buyer.url();
  const address = (await buyer.locator('.payment-address .mono').innerText()).trim();
  expect((await request.post(`${rpc}/test/deposit`, { data: { address, amount: 100_000 } })).ok()).toBeTruthy();
  expect((await request.post(`${rpc}/test/confirm`, { data: { address } })).ok()).toBeTruthy();
  await reloadUntil(buyer, async () => { await expect(buyer.locator('.page-head .badge')).toHaveText('Paid', { timeout: 500 }); });
  // A-80: both dispute forms warn that the reason is plain text and point to the encrypted staff contacts.
  await buyer.goto('/disputes');
  await expect(buyer.getByLabel('Describe the issue')).toHaveAccessibleDescription(/^Stored unencrypted\..*Never include an address, real name or tracking number.*Contact dispute staff/);
  await fitsAt(buyer, 'disputes form');
  await buyer.goto(orderURL);
  await buyer.getByText('Open a dispute', { exact: true }).click();
  await expect(buyer.getByLabel('Describe the issue')).toHaveAccessibleDescription(/^Stored unencrypted\..*Never include an address, real name or tracking number.*Contact dispute staff/);
  await buyer.getByLabel('Describe the issue').fill('Please review the encrypted evidence sent to the independent moderator.');
  await submitAndLoad(buyer, buyer.getByRole('button', { name: 'Submit dispute' }));
  await buyer.goto(orderURL);
  await expect(buyer.locator('.page-head .badge')).toHaveText('Disputed');
  await fitsAt(buyer, 'disputed order');
  await buyer.goto('/disputes');
  await expect(buyer.getByRole('link', { name: `Order ${new URL(orderURL).searchParams.get('id')}` })).toBeVisible();
  await fitsAt(buyer, 'disputes list');
  await buyer.goto(orderURL);
  const staff = buyer.getByRole('region', { name: 'Contact dispute staff' });
  await expect(staff).not.toContainText('wallet_admin');
  await staff.locator('summary', { hasText: 'wallet_moderator' }).click();
  await expect(staff).toContainText('Ownership not verified');
  await staff.getByRole('link', { name: 'Message wallet_moderator' }).click();
  await expect(buyer.getByLabel('Recipient handle', { exact: true })).toHaveValue('wallet_moderator');
  await buyer.getByLabel('PGP encrypted message').fill(pgpFixture('message-to-recipient.asc'));
  await submitAndLoad(buyer, buyer.getByRole('button', { name: 'Send encrypted text ↗' }));
  await moderator.goto('/messages');
  await expect(moderator.locator('article.message textarea[readonly]')).toHaveValue(pgpFixture('message-to-recipient.asc').trim());
  await expect(moderator.locator('.pgp-message-status')).toContainText('to recipient’s key');
  await admin.goto('/moderator');
  await expect(admin.getByRole('button', { name: 'Resolve dispute' })).toHaveCount(0);
  await moderator.goto('/moderator');
  await fitsAt(moderator, 'moderation desk');
  // A-161: each outcome states its payout; the decision warns against identifying text; a ticked confirmation is required.
  await expect(moderator.getByRole('radio', { name: 'Release 0.001 BTC (test network) to wallet_admin — exactly the price; network fee deducted', exact: true })).toBeVisible();
  await moderator.getByRole('radio', { name: 'Refund 0.001 BTC (test network) to wallet_buyer — exactly the price; network fee deducted', exact: true }).check();
  await expect(moderator.getByLabel('Decision', { exact: true })).toHaveAccessibleDescription(/^Stored unencrypted\..*Never include an address, real name or tracking number/);
  await moderator.getByLabel('Decision', { exact: true }).fill('Fixture review completed: return the confirmed test payment to the buyer.');
  await moderator.getByRole('checkbox', { name: 'I checked the amount and who receives it for the outcome I chose. Resolving is final.' }).check();
  // A-32: resolving queues the payout, so it needs the moderator's current password.
  await moderator.locator('form[action="/resolve"]').getByLabel('Current password', { exact: true }).fill('wallet-browser-moderator-password-123');
  await submitAndLoad(moderator, moderator.getByRole('button', { name: 'Resolve dispute' }));
  await buyer.goto(orderURL);
  await expect(buyer.locator('.page-head .badge')).toHaveText('Resolved');
  const refunds = async () => ((await (await request.get(`${rpc}/test/state`)).json()).payouts as any[]).filter(p => p.address === refundAddress);
  await expect.poll(async () => (await refunds()).length).toBe(1);
  expect(await refunds()).toEqual([expect.objectContaining({ currency: 'BTC', amount: 100_000 })]);
  const checks = (await (await request.get(`${rpc}/test/state`)).json()).checks;
  await expect.poll(async () => (await (await request.get(`${rpc}/test/state`)).json()).checks).toBeGreaterThan(checks + 2);
  expect(await refunds()).toHaveLength(1);
  await reloadUntil(buyer, async () => { await expect(buyer.locator('.payment-figures div').filter({ has: buyer.getByText('Payout status', { exact: true }) })).toContainText('Sent', { timeout: 500 }); });
  for (const page of [admin, buyer, moderator]) await page.context().close();
});

test('vendor cancels a paid physical order, refunds once and restores stock', async ({ browser, baseURL, request }) => {
  const vendor = await signedIn(browser, baseURL, 'wallet_admin', password, false);
  const buyer = await signedIn(browser, baseURL, 'wallet_buyer', 'wallet-browser-buyer-password-123', false);
  try {
    await buyer.goto('/?q=Wallet%20BTC%20journey');
    await buyer.getByRole('link', { name: 'Wallet BTC journey', exact: true }).click();
    await buyer.waitForURL(/\/product\?id=/);
    const productURL = buyer.url();
    const stock = buyer.locator('.key-values div', { has: buyer.getByText('Available stock', { exact: true }) });
    const before = Number(await stock.locator('dd').innerText());
    await buyer.getByRole('link', { name: 'Review order draft' }).click();
    await buyer.getByRole('button', { name: 'Create unfunded draft' }).click();
    await buyer.getByRole('button', { name: 'Request payment address' }).click();
    await expectNotice(buyer, 'Payment address ready.');
    const orderURL = buyer.url();
    const address = (await buyer.locator('.payment-address .mono').innerText()).trim();
    const beforePayouts = (await (await request.get(`${rpc}/test/state`)).json()).payouts.length;
    expect((await request.post(`${rpc}/test/deposit`, { data: { address, amount: 100_000 } })).ok()).toBeTruthy();
    expect((await request.post(`${rpc}/test/confirm`, { data: { address } })).ok()).toBeTruthy();
    await reloadUntil(buyer, async () => { await expect(buyer.locator('.page-head .badge')).toHaveText('Paid', { timeout: 500 }); });
    await expect(buyer.getByRole('button', { name: 'Cancel order', exact: true })).toHaveCount(0);
    await vendor.goto(orderURL);
    await expect(vendor.getByLabel('Reason (optional)')).toHaveAccessibleDescription(/^Stored unencrypted\..*Never include an address, real name or tracking number/);
    await vendor.getByLabel('Reason (optional)').fill('Fixture vendor unable to fulfill this paid order.');
    // A-122: cancelling a paid order states the refund and needs its confirmation.
    await vendor.getByRole('checkbox', { name: 'Refund 0.001 BTC (test network) to wallet_buyer — exactly the price; network fee deducted. This is final.', exact: true }).check();
    await submit(vendor, vendor.getByRole('button', { name: 'Cancel order', exact: true }));
    await buyer.reload();
    await expect(buyer.locator('.page-head .badge')).toHaveText('Cancelled');
    await reloadUntil(buyer, async () => { await expect(buyer.locator('.payment-figures div', { has: buyer.getByText('Payout status', { exact: true }) })).toContainText('Sent', { timeout: 500 }); });
    const payouts = (await (await request.get(`${rpc}/test/state`)).json()).payouts;
    expect(payouts).toHaveLength(beforePayouts + 1);
    expect(payouts.at(-1)).toEqual(expect.objectContaining({ currency: 'BTC', address: 'bcrt1qfixturebuyerrefund00000000', amount: 100_000 }));
    // A-214: only the buyer sees the refund's transaction ID (on Bitcoin it reveals the buyer's payout address);
    // the vendor who forced the refund sees neither it on the order page nor in a notification.
    const refundTxid: string = payouts.at(-1).txid;
    await expect(buyer.locator('.payment-figures div', { has: buyer.getByText('Payout transaction', { exact: true }) })).toContainText(refundTxid);
    await buyer.goto(productURL);
    await expect(stock.locator('dd')).toHaveText(String(before));
    await vendor.reload();
    await expect(vendor.getByRole('button', { name: 'Cancel order', exact: true })).toHaveCount(0);
    await expect(vendor.locator('.payment-figures div', { has: vendor.getByText('Payout status', { exact: true }) })).toContainText('Sent');
    await expect(vendor.locator('main')).not.toContainText(refundTxid);
    await vendor.goto('/notifications');
    await expect(vendor.locator('main')).not.toContainText(refundTxid);
  } finally {
    await Promise.allSettled([vendor.context().close(), buyer.context().close()]);
  }
});

// A-120: a refund to a buyer with no payout address waits; its recipient (only) is told why and what to do, on the
// order page and on /account#payout, and saving an address sends it once. A fresh buyer: it has no address.
test('a refund waiting for a payout address tells its recipient what to do and is sent once', async ({ browser, baseURL, request }) => {
  const vendor = await signedIn(browser, baseURL, 'wallet_admin', password, false);
  const refundeePassword = 'wallet-browser-refundee-password-123';
  const refundee = await signedIn(browser, baseURL, 'wallet_refundee', refundeePassword, true);
  try {
    await refundee.goto('/?q=Wallet%20BTC%20journey');
    await refundee.getByRole('link', { name: 'Wallet BTC journey', exact: true }).click();
    await refundee.getByRole('link', { name: 'Review order draft' }).click();
    await refundee.getByRole('button', { name: 'Create unfunded draft' }).click();
    await refundee.getByRole('button', { name: 'Request payment address' }).click();
    await expectNotice(refundee, 'Payment address ready.');
    const orderURL = refundee.url();
    const orderID = new URL(orderURL).searchParams.get('id')!;
    const address = (await refundee.locator('.payment-address .mono').innerText()).trim();
    expect((await request.post(`${rpc}/test/deposit`, { data: { address, amount: 100_000 } })).ok()).toBeTruthy();
    expect((await request.post(`${rpc}/test/confirm`, { data: { address } })).ok()).toBeTruthy();
    await reloadUntil(refundee, async () => { await expect(refundee.locator('.page-head .badge')).toHaveText('Paid', { timeout: 500 }); });
    await vendor.goto(orderURL);
    await vendor.getByRole('checkbox', { name: 'Refund 0.001 BTC (test network) to wallet_refundee — exactly the price; network fee deducted. This is final.', exact: true }).check();
    await submit(vendor, vendor.getByRole('button', { name: 'Cancel order', exact: true }));
    await expect(vendor.locator('.page-head .badge')).toHaveText('Cancelled');
    // The other party sees a closed order, not the refund's next step.
    await expect(vendor.locator('main')).toContainText('No further changes are possible');
    await expect(vendor.getByRole('link', { name: 'Your payouts not sent yet' })).toHaveCount(0);
    await refundee.reload();
    await expect(refundee.locator('.page-head .badge')).toHaveText('Cancelled');
    await expect(refundee.locator('main')).not.toContainText('No further changes are possible');
    await expect(refundee.getByRole('status').filter({ hasText: 'has not been sent' })).toContainText('Save a BTC payout address on your account page');
    await expect(refundee.locator('.payment-figures div', { has: refundee.getByText('Payout status', { exact: true }) })).toContainText('Waiting for your payout address');
    await fitsAt(refundee, 'refund waiting for an address');
    await Promise.all([refundee.waitForURL(/\/account#payout$/), refundee.getByRole('link', { name: 'Your payouts not sent yet' }).click()]);
    const unsent = refundee.getByRole('region', { name: 'Payouts to you not sent yet' });
    const row = unsent.locator('tr', { has: refundee.getByRole('link', { name: orderID.slice(0, 8), exact: true }) });
    await expect(row).toContainText('Refund of 0.001 BTC');
    await expect(row).toContainText('Waiting for your payout address');
    await expect(row).toContainText('None yet');
    await expect(row.locator('td').nth(1)).toHaveText(/#\d+/);
    // A-220(b): the recipient can read the status column at phone width without horizontal scrolling.
    const originalViewport = refundee.viewportSize()!;
    await refundee.setViewportSize({ width: 320, height: originalViewport.height });
    await waitForStyles(refundee);
    const tableSize = await unsent.locator('table').evaluate(table => ({ client: table.clientWidth, scroll: table.scrollWidth }));
    expect(tableSize.scroll, 'unsent payout table at 320px').toBeLessThanOrEqual(tableSize.client + 1);
    const statusBox = await row.locator('td').nth(4).boundingBox();
    expect(statusBox, 'status cell exists at 320px').not.toBeNull();
    expect(statusBox!.x, 'status cell starts within the viewport at 320px').toBeGreaterThanOrEqual(0);
    expect(statusBox!.x + statusBox!.width, 'status cell is visible at 320px').toBeLessThanOrEqual(320);
    await refundee.setViewportSize(originalViewport);
    await waitForStyles(refundee);
    await fitsAt(refundee, 'unsent payouts');
    const refundAddress = 'bcrt1qfixturerefundee00000000000';
    const refunds = async () => ((await (await request.get(`${rpc}/test/state`)).json()).payouts as any[]).filter(p => p.address === refundAddress);
    const form = refundee.locator('form', { has: refundee.getByRole('button', { name: 'Save BTC address' }) });
    await form.locator('input[name="address"]').fill(refundAddress);
    await form.getByLabel('Current password', { exact: true }).fill(refundeePassword);
    await submit(refundee, form.getByRole('button'));
    await expect.poll(async () => (await refunds()).length).toBe(1);
    expect(await refunds()).toEqual([expect.objectContaining({ currency: 'BTC', amount: 100_000 })]);
    await reloadUntil(refundee, async () => { await expect(unsent).toHaveCount(0, { timeout: 500 }); });
    // Sent: the order is closed for both; only the recipient sees where the refund went.
    await refundee.goto(orderURL);
    await expect(refundee.locator('.payment-figures div', { has: refundee.getByText('Payout status', { exact: true }) })).toContainText('Sent');
    await expect(refundee.locator('.payment-figures div', { has: refundee.getByText('Destination (shown only to you)', { exact: true }) })).toContainText(refundAddress);
    await expect(refundee.locator('main')).toContainText('No further changes are possible');
    await vendor.reload();
    await expect(vendor.locator('main')).not.toContainText(refundAddress);
    expect(await refunds()).toHaveLength(1);
  } finally {
    await Promise.allSettled([vendor.context().close(), refundee.context().close()]);
  }
});

test('automatic digital delivery and administrator recovery of a rejected payout', async ({ browser, baseURL, request }) => {
  const admin = await signedIn(browser, baseURL, 'wallet_admin', password, false);
  const vendor = await signedIn(browser, baseURL, 'wallet_vendor', vendorPassword, false);
  const buyer = await signedIn(browser, baseURL, 'wallet_buyer', 'wallet-browser-buyer-password-123', false);
  try {
    await vendor.goto('/vendor-dashboard');
    await vendor.getByRole('link', { name: 'Edit Wallet XMR journey', exact: true }).click();
    const content = 'Private automatically delivered fixture download token';
    await vendor.getByLabel('Automatic delivery content').fill(content);
    await submitAndLoad(vendor, vendor.getByRole('button', { name: 'Save changes' }));
    await buyer.goto('/?q=Wallet%20XMR%20journey');
    await buyer.getByRole('link', { name: 'Wallet XMR journey', exact: true }).click();
    await expect(buyer.locator('main')).not.toContainText(content);
    await buyer.getByRole('link', { name: 'Review order draft' }).click();
    await buyer.getByLabel('Pay with').selectOption('XMR');
    await buyer.getByRole('button', { name: 'Create unfunded draft' }).click();
    await buyer.getByRole('button', { name: 'Request payment address' }).click();
    await expectNotice(buyer, 'Payment address ready.');
    const orderURL = buyer.url();
    const orderID = new URL(orderURL).searchParams.get('id')!;
    await expect(buyer.locator('main')).not.toContainText(content);
    const address = (await buyer.locator('.payment-address .mono').innerText()).trim();
    expect((await request.post(`${rpc}/test/deposit`, { data: { address, amount: 500_000_000_000 } })).ok()).toBeTruthy();
    expect((await request.post(`${rpc}/test/confirm`, { data: { address } })).ok()).toBeTruthy();
    await reloadUntil(buyer, async () => { await expect(buyer.locator('.page-head .badge')).toHaveText('Delivered', { timeout: 500 }); });
    await expect(buyer.getByRole('region', { name: 'Digital delivery' })).toContainText(content);
    const before = (await (await request.get(`${rpc}/test/state`)).json()).payouts.length;
    expect((await request.post(`${rpc}/test/fail-next-send`, { data: { currency: 'XMR' } })).ok()).toBeTruthy();
    await buyer.getByRole('checkbox', { name: 'Release 0.5 XMR (test network) to wallet_vendor — exactly the price; network fee deducted. This is final.', exact: true }).check();
    await buyer.getByRole('button', { name: 'Confirm receipt and complete' }).click();
    await reloadUntil(buyer, async () => { await expect(buyer.locator('.payment-figures div', { has: buyer.getByText('Payout status', { exact: true }) })).toContainText('Failed', { timeout: 500 }); });
    await admin.goto('/admin');
    let payout = admin.locator('tr', { has: admin.getByText(orderID, { exact: true }) });
    await expect(payout).toContainText('Fixture rejected send before broadcast');
    // A-281: the payout's row links its resolve form, which sits under the payouts table.
    const resolveForm = async () => {
      const link = admin.locator('tr', { has: admin.getByText(orderID, { exact: true }) }).getByRole('link', { name: /^Resolve payout \d+$/ });
      const target = (await link.getAttribute('href'))!;
      await link.click();
      const details = admin.locator(`details.payout-actions${target}`);
      await details.locator('summary').click();
      return details;
    };
    const checks = (await (await request.get(`${rpc}/test/state`)).json()).checks;
    await expect.poll(async () => (await (await request.get(`${rpc}/test/state`)).json()).checks).toBeGreaterThan(checks + 2);
    expect((await (await request.get(`${rpc}/test/state`)).json()).payouts).toHaveLength(before);
    // A-120: the vendor desk counts the recipient's unsent payouts and links to them; the list gives the fixed
    // copy for a definite failure, never the wallet's error text.
    await vendor.goto('/vendor-dashboard');
    await Promise.all([vendor.waitForURL(/\/account#payout$/), vendor.getByRole('link', { name: '1 unsent payout(s) to you', exact: true }).click()]);
    const unsent = vendor.getByRole('region', { name: 'Payouts to you not sent yet' }).locator('tr', { has: vendor.getByRole('link', { name: orderID.slice(0, 8), exact: true }) });
    await expect(unsent).toContainText('The market wallet refused the send before broadcasting; administrators have been notified');
    await expect(unsent).not.toContainText('Fixture rejected send');
    // A-121: saving a new address leaves the failed payout on its address and names the order to its owner; the
    // administrator moves it to the current address after checking it, and the requeue below pays that address once.
    const moved = '5' + 'b'.repeat(94);
    await vendor.goto('/account');
    const save = vendor.locator('form', { has: vendor.getByRole('button', { name: 'Save XMR address' }) });
    await save.locator('input[name="address"]').fill(moved);
    await save.getByLabel('Current password', { exact: true }).fill(vendorPassword);
    await submit(vendor, save.getByRole('button'));
    await vendor.goto('/notifications');
    await expect(vendor.locator('main')).toContainText(`1 unsent payout(s) still use your previous address (order ${orderID.slice(0, 8)}); an administrator must confirm the change`);
    await admin.goto('/admin');
    let resolve = await resolveForm();
    const move = resolve.locator('form', { has: admin.getByRole('button', { name: "Use the account's current address", exact: true }) });
    await expect(move).toContainText(`Payout address: ${'5' + 'a'.repeat(94)}. Account's current address: ${moved} (last changed `);
    await expect(move).toContainText(/last changed \d{4}-\d\d-\d\d \d\d:\d\d UTC\)/);
    await move.getByRole('checkbox', { name: 'Current address checked: the account owner confirmed it is theirs', exact: true }).check();
    await move.getByLabel('Current password', { exact: true }).fill(password);
    await submit(admin, move.getByRole('button'));
    await expect(admin.locator('main')).toContainText('Moved payout');
    payout = admin.locator('tr', { has: admin.getByText(orderID, { exact: true }) });
    await expect(payout).toContainText('Fixture rejected send before broadcast');
    resolve = await resolveForm();
    await expect(resolve.getByRole('button', { name: "Use the account's current address", exact: true })).toHaveCount(0);
    expect((await (await request.get(`${rpc}/test/state`)).json()).payouts).toHaveLength(before);
    let requeue = resolve.locator('form', { has: admin.getByRole('button', { name: 'Requeue payout', exact: true }) });
    await requeue.getByLabel('Current password', { exact: true }).fill('incorrect-password');
    await requeue.getByRole('button').click();
    await expect(admin.locator('body')).toContainText('Password incorrect');
    await admin.goto('/admin');
    resolve = await resolveForm();
    requeue = resolve.locator('form', { has: admin.getByRole('button', { name: 'Requeue payout', exact: true }) });
    await requeue.getByLabel('Current password', { exact: true }).fill(password);
    await submit(admin, requeue.getByRole('button'));
    await reloadUntil(buyer, async () => { await expect(buyer.locator('.payment-figures div', { has: buyer.getByText('Payout status', { exact: true }) })).toContainText('Sent', { timeout: 500 }); });
    expect((await (await request.get(`${rpc}/test/state`)).json()).payouts).toHaveLength(before + 1);
    expect(((await (await request.get(`${rpc}/test/state`)).json()).payouts as any[]).at(-1)).toEqual(expect.objectContaining({ currency: 'XMR', address: moved, amount: 500_000_000_000, fee_from_amount: true, received: 500_000_000_000 - 30_000_000 }));
    await admin.reload();
    await expect(admin.locator('tr', { has: admin.getByText(orderID, { exact: true }) }).getByRole('link', { name: /^Resolve payout / })).toHaveCount(0);
    await expect(admin.locator('main')).toContainText('Requeued payout');
    // A-174: every check reads the wallet and node versions. The verified monero-wallet-rpc (1.31, a release
    // build) raises no warning; another version is named with the bump checklist until the verified one is back.
    const providers = admin.locator('.admin-payments');
    const warning = 'XMR wallet version not verified';
    await expect(providers).toContainText('monero-wallet-rpc RPC version 1.31, release build');
    await expect(providers).toContainText('Bitcoin Core 31.1.0');
    await expect(providers).not.toContainText(warning);
    try {
      expect((await request.post(`${rpc}/test/wallet-version`, { data: { version: (1 << 16) | 35, release: true } })).ok()).toBeTruthy();
      await reloadUntil(admin, async () => { await expect(providers).toContainText(`${warning}: monero-wallet-rpc RPC version 1.35, release build.`, { timeout: 500 }); });
      await expect(providers).toContainText('docs/operator-guide.md#node-image-versions');
      await fitsAt(admin, 'unverified wallet warning');
    } finally {
      expect((await request.post(`${rpc}/test/wallet-version`, { data: { version: (1 << 16) | 31, release: true } })).ok()).toBeTruthy();
    }
    await reloadUntil(admin, async () => { await expect(providers).not.toContainText(warning, { timeout: 500 }); });
  } finally {
    await Promise.allSettled([admin.context().close(), vendor.context().close(), buyer.context().close()]);
  }
});

// A-313: an overpaid order completes with two payouts, the price to the vendor and the excess back to the buyer, each
// sent once through the wallet. A fresh buyer (wallet_admin's password confirmations are spent above).
test('an overpaid BTC order releases the price and returns the excess, each sent once', async ({ browser, baseURL, request }) => {
  const vendor = await signedIn(browser, baseURL, 'wallet_admin', password, false);
  const overpayerPassword = 'wallet-browser-overpayer-password-123';
  const buyer = await signedIn(browser, baseURL, 'wallet_overpayer', overpayerPassword, true);
  try {
    const vendorAddress = 'bcrt1qfixturepayout000000000000';
    const returnAddress = 'bcrt1qfixtureoverpayreturn00000';
    await buyer.goto('/account');
    const form = buyer.locator('form', { has: buyer.getByRole('button', { name: 'Save BTC address' }) });
    await form.locator('input[name="address"]').fill(returnAddress);
    await form.getByLabel('Current password', { exact: true }).fill(overpayerPassword);
    await submit(buyer, form.getByRole('button'));
    await expect(form.locator('input[name="address"]')).toHaveValue(returnAddress);
    await buyer.goto('/?q=Wallet%20BTC%20journey');
    await buyer.getByRole('link', { name: 'Wallet BTC journey', exact: true }).click();
    await buyer.getByRole('link', { name: 'Review order draft' }).click();
    await submit(buyer, buyer.getByRole('button', { name: 'Create unfunded draft' }));
    await buyer.getByRole('button', { name: 'Request payment address' }).click();
    await expectNotice(buyer, 'Payment address ready.');
    const orderURL = buyer.url();
    const address = (await buyer.locator('.payment-address .mono').innerText()).trim();
    const sends = async () => ((await (await request.get(`${rpc}/test/state`)).json()).payouts as any[]);
    const before = (await sends()).length;
    // One deposit of one and a half times the price.
    expect((await request.post(`${rpc}/test/deposit`, { data: { address, amount: 150_000 } })).ok()).toBeTruthy();
    expect((await request.post(`${rpc}/test/confirm`, { data: { address } })).ok()).toBeTruthy();
    await reloadUntil(buyer, async () => { await expect(buyer.locator('.page-head .badge')).toHaveText('Paid', { timeout: 500 }); });
    await vendor.goto(orderURL);
    await submit(vendor, vendor.getByRole('button', { name: 'Mark as shipped' }));
    await buyer.reload();
    // A-122: the complete form states both payouts.
    const release = buyer.getByRole('checkbox', { name: 'Release 0.001 BTC (test network) to wallet_admin (the price) and return the 0.0005 BTC paid over the price to wallet_overpayer; network fee deducted from each. This is final.', exact: true });
    await expect(buyer.locator('input[name="amount_seen"]')).toHaveValue('150000');
    await release.check();
    await submit(buyer, buyer.getByRole('button', { name: 'Confirm receipt and complete' }));
    await expect(buyer.locator('.page-head .badge')).toHaveText('Completed');
    await expect.poll(async () => (await sends()).length - before, { timeout: 30_000 }).toBe(2);
    const sent = (await sends()).slice(before);
    expect(sent.filter(p => p.address === vendorAddress)).toEqual([expect.objectContaining({ currency: 'BTC', amount: 100_000, fee_from_amount: true })]);
    expect(sent.filter(p => p.address === returnAddress)).toEqual([expect.objectContaining({ currency: 'BTC', amount: 50_000, fee_from_amount: true })]);
    const returnTxid: string = sent.find(p => p.address === returnAddress).txid;
    const status = (kind: string) => buyer.locator('.payment-figures', { has: buyer.getByText(kind, { exact: true }) })
      .locator('div', { has: buyer.getByText('Payout status', { exact: true }) });
    await reloadUntil(buyer, async () => {
      await expect(status('Release to vendor')).toContainText('Sent', { timeout: 500 });
      await expect(status('Return to buyer')).toContainText('Sent', { timeout: 500 });
    });
    await expect(buyer.getByRole('heading', { name: 'Return of the amount paid over the price' })).toBeVisible();
    await expect(buyer.locator('main')).toContainText(returnTxid);
    await fitsAt(buyer, 'overpaid order completed');
    // More watcher passes send nothing further.
    const checks = (await (await request.get(`${rpc}/test/state`)).json()).checks;
    await expect.poll(async () => (await (await request.get(`${rpc}/test/state`)).json()).checks).toBeGreaterThan(checks + 2);
    expect((await sends()).length - before).toBe(2);
    // A-214: the vendor sees the return's status, not its transaction ID or the buyer's address.
    await vendor.goto(orderURL);
    await expect(vendor.locator('.payment-figures', { has: vendor.getByText('Return to buyer', { exact: true }) })).toContainText('Sent');
    await expect(vendor.locator('main')).not.toContainText(returnTxid);
    await expect(vendor.locator('main')).not.toContainText(returnAddress);
  } finally {
    await Promise.allSettled([vendor.context().close(), buyer.context().close()]);
  }
});
