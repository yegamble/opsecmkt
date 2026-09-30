import { expect, test, type Browser, type Page } from '@playwright/test';
import { execFile, execFileSync } from 'node:child_process';
import { promisify } from 'node:util';
import { expectNotice, pgpFixture, signedIn, setRole, submitAndLoad } from './db-helpers';

// The legs after the first test reuse its marketplace and administrator; a failure skips them.
test.describe.configure({ mode: 'serial' });

const execute = promisify(execFile);
async function chain(...args: string[]) {
  const result = await execute('python3', ['tests/e2e/fixtures/local-chain.py', ...args], { timeout: 180_000 });
  return JSON.parse(result.stdout);
}
async function refreshUntil(page: Page, assertion: () => Promise<void>) {
  await expect(async () => { await page.reload(); await assertion(); }).toPass({ timeout: 60_000, intervals: [1000, 2000] });
}
const currencies = (process.env.LOCAL_CHAIN_CURRENCIES || 'BTC,XMR').split(',');
if (currencies.some(currency => !['BTC', 'XMR'].includes(currency))) throw new Error('Only BTC and XMR local chains are supported');
// txid, when given, is the payout's recorded transaction: the recipient's wallet must have received exactly it.
async function verifyReceipt(currency: string, address: string, total: number, txid?: string) {
  const receipt = await chain('received', currency, address);
  expect(receipt.transactions).toBe(1);
  expect(receipt.amount).toBeGreaterThan(0);
  // Every payout on both coins, Monero releases included (A-232), is sent with the fee taken from the amount.
  expect(receipt.fee).toBeGreaterThan(0);
  expect(receipt.amount + receipt.fee).toBe(total);
  if (txid !== undefined) expect(receipt.txids).toEqual([txid]);
  // The run's evidence (A-103): each payout received on chain, its fee and transaction.
  console.log(`received on chain: ${currency} total ${total} = amount ${receipt.amount} + fee ${receipt.fee}, txid ${receipt.txids.join(',')}`);
}
// Read-only checks of the run's own database (payouts, payments and the watcher's poll time); nothing is written.
// A-320: the connection details reach psql through libpq's environment, never its command line, so a failed query's
// error ("Command failed: psql …", kept in the reports) carries no password. psql 16 does not expand a URI in
// PGDATABASE, hence the separate variables.
const database = new URL(process.env.E2E_CHAIN_DATABASE_URL!);
const psqlEnv = { ...process.env, PGHOST: database.hostname, PGPORT: database.port || '5432', PGUSER: decodeURIComponent(database.username),
  PGPASSWORD: decodeURIComponent(database.password), PGDATABASE: decodeURIComponent(database.pathname.slice(1)),
  PGSSLMODE: database.searchParams.get('sslmode') || 'prefer' };
const sql = (query: string) => execFileSync('psql', ['-v', 'ON_ERROR_STOP=1', '-qtAc', query], { encoding: 'utf8', env: psqlEnv }).trim();
type Payout = { kind: string, amount: number, state: string, txid: string, ambiguous: boolean, address: string, error: string };
function payouts(orderID: string): Payout[] {
  expect(orderID).toMatch(/^[0-9a-f]+$/);
  return JSON.parse(sql(`SELECT COALESCE(json_agg(json_build_object('kind',kind,'amount',amount,'state',state,'txid',txid,'ambiguous',send_ambiguous,
    'address',address,'error',error) ORDER BY id),'[]') FROM payouts WHERE order_id='${orderID}'`));
}
// No payout anywhere in the run was sent without a definite answer, failed, or is stuck in sending.
function noAmbiguousSends() {
  expect(sql("SELECT count(*) FROM payouts WHERE send_ambiguous OR state IN ('sending','failed')")).toBe('0');
}
// Waits for the payment watcher to finish at least `cycles` more polls of currency (payment_status.last_poll).
async function watcherCycles(currency: string, cycles = 3) {
  const poll = () => sql(`SELECT COALESCE(last_poll::text,'') FROM payment_status WHERE currency='${currency}'`);
  let last = poll(), seen = 0;
  await expect.poll(() => {
    const now = poll();
    if (now !== last) [last, seen] = [now, seen + 1];
    return seen;
  }, { timeout: 60_000, intervals: [250] }).toBeGreaterThanOrEqual(cycles);
}
const password = 'local-chain-browser-password-123';

test('isolated real chains: seeded buyer/vendor partial deposits, confirmation, fulfillment and moderator refund', async ({ page: admin, browser, baseURL }) => {
  await admin.goto('/setup');
  await admin.getByLabel('Setup token').fill('local-chain-browser-test-setup-token-at-least-32');
  await admin.getByLabel('Handle', { exact: true }).fill('chain_admin');
  await admin.getByLabel('Password', { exact: true }).fill(password);
  await admin.getByRole('button', { name: 'Initialize marketplace' }).click();
  await expect(admin).toHaveURL(/\/admin\?welcome=1$/);
  await admin.locator('.admin-auth').getByRole('button', { name: 'Turn CAPTCHA off' }).click();
  const buyer = await signedIn(browser, baseURL, 'chain_buyer', password, true);
  const vendor = await signedIn(browser, baseURL, 'chain_vendor', password, true);
  const moderator = await signedIn(browser, baseURL, 'chain_moderator', password, true);
  await setRole(admin, 'chain_vendor', 'vendor');
  await setRole(admin, 'chain_moderator', 'moderator');
  // A-83: buyers can pay for the physical listing only while its vendor has a key that can receive the encrypted
  // shipping address.
  await vendor.goto('/account');
  const profile = vendor.locator('form', { has: vendor.getByRole('button', { name: 'Save profile' }) });
  await profile.getByLabel('PGP public key', { exact: true }).fill(pgpFixture('recipient.pub.asc'));
  await profile.getByLabel('Current password', { exact: true }).fill(password);
  await Promise.all([vendor.waitForEvent('load'), profile.getByRole('button', { name: 'Save profile' }).click()]);
  await expect(vendor).toHaveURL(/\/account$/);
  await expectNotice(vendor, 'Profile saved.');

  for (const currency of currencies) {
    const total = currency === 'BTC' ? 100_000 : 500_000_000_000;
    const destination = (await chain('address', currency)).address;
    await vendor.goto('/account');
    const payoutForm = vendor.locator('form', { has: vendor.getByRole('button', { name: `Save ${currency} address` }) });
    await payoutForm.locator('input[name="address"]').fill(destination);
    await payoutForm.getByLabel('Current password', { exact: true }).fill(password);
    await payoutForm.getByRole('button').click();
    await expect(payoutForm.locator('input[name="address"]')).toHaveValue(destination);
    await vendor.goto('/vendor-dashboard');
    await vendor.getByText('+ Publish a new listing', { exact: true }).click();
    const title = `Local-chain ${currency} seeded listing`;
    await vendor.getByLabel('Listing title').fill(title);
    await vendor.getByLabel('Description', { exact: true }).fill('Isolated real node browser test. Locally mined test coins only.');
    await vendor.getByLabel('Fulfillment type').selectOption(currency === 'BTC' ? 'physical' : 'digital');
    await vendor.getByLabel('Stock', { exact: true }).fill('3');
    await vendor.getByLabel('Bitcoin reference price').fill('0.001');
    await vendor.getByLabel('Monero reference price').fill('0.5');
    await vendor.getByRole('button', { name: 'Publish listing' }).click();
    await buyer.goto(`/?q=${encodeURIComponent(title)}`);
    await buyer.getByRole('link', { name: title, exact: true }).click();
    await buyer.waitForURL(/\/product\?id=/);
    const productURL = buyer.url();
    const makeOrder = async () => {
      await buyer.goto(productURL);
      await buyer.getByRole('link', { name: 'Review order draft' }).click();
      await buyer.getByLabel('Reference currency').selectOption(currency);
      await buyer.getByRole('button', { name: 'Create unfunded draft' }).click();
      await buyer.getByRole('button', { name: 'Request payment address' }).click();
      await expect(buyer.locator('.page-head .badge')).toHaveText('Awaiting payment');
      return { url: buyer.url(), address: (await buyer.locator('.payment-address .mono').innerText()).trim() };
    };
    const order = await makeOrder();
    await chain('deposit', currency, order.address, currency === 'BTC' ? '0.0004' : '0.2');
    await chain('mine', currency, '12');
    await refreshUntil(buyer, async () => {
      await expect(buyer.locator('.payment-figures div').filter({ has: buyer.getByText('Remaining to send', { exact: true }) }))
        .toContainText(currency === 'BTC' ? '0.0006' : '0.3', { timeout: 1000 });
    });
    await expect(buyer.locator('.page-head .badge')).toHaveText('Awaiting payment');
    await expect(buyer.locator('.payment-address .mono')).toHaveText(order.address);
    await chain('deposit', currency, order.address, currency === 'BTC' ? '0.0006' : '0.3');
    await chain('mine', currency, '12');
    await refreshUntil(buyer, async () => { await expect(buyer.locator('.page-head .badge')).toHaveText('Paid', { timeout: 1000 }); });
    await vendor.goto(order.url);
    if (currency === 'BTC') await vendor.getByRole('button', { name: 'Mark as shipped' }).click();
    else {
      await vendor.getByLabel('Delivery content', { exact: true }).fill('Real-chain test digital delivery');
      await vendor.getByRole('button', { name: 'Deliver digital content', exact: true }).click();
    }
    await buyer.reload();
    const price = currency === 'BTC' ? '0.001 BTC' : '0.5 XMR';
    await buyer.getByRole('checkbox', { name: `Release ${price} (test network) to chain_vendor — exactly the price; network fee deducted. This is final.`, exact: true }).check();
    await buyer.getByRole('button', { name: 'Confirm receipt and complete' }).click();
    await expect(buyer.locator('.page-head .badge')).toHaveText('Completed');
    await refreshUntil(buyer, async () => {
      await expect(buyer.locator('.payment-figures div').filter({ has: buyer.getByText('Payout status', { exact: true }) })).toContainText('Sent', { timeout: 1000 });
    });
    await expect(buyer.locator('main')).toContainText(`${currency === 'BTC' ? 'Bitcoin' : 'Monero'} network fee is deducted from this payout`);
    await chain('mine', currency, '12');
    await verifyReceipt(currency, destination, total);

    const refundAddress = (await chain('address', currency)).address;
    await buyer.goto('/account');
    const refundForm = buyer.locator('form', { has: buyer.getByRole('button', { name: `Save ${currency} address` }) });
    await refundForm.locator('input[name="address"]').fill(refundAddress);
    await refundForm.getByLabel('Current password', { exact: true }).fill(password);
    await refundForm.getByRole('button').click();
    const dispute = await makeOrder();
    await chain('deposit', currency, dispute.address, currency === 'BTC' ? '0.001' : '0.5');
    await chain('mine', currency, '12');
    await refreshUntil(buyer, async () => { await expect(buyer.locator('.page-head .badge')).toHaveText('Paid', { timeout: 1000 }); });
    await buyer.getByText('Open a dispute', { exact: true }).click();
    await buyer.getByLabel('Describe the issue').fill('Local-chain browser test refund request.');
    await buyer.getByRole('button', { name: 'Submit dispute' }).click();
    await buyer.goto(dispute.url);
    await expect(buyer.locator('.page-head .badge')).toHaveText('Disputed');
    await moderator.goto('/moderator');
    const resolution = moderator.locator('article.message').filter({ has: moderator.locator(`a[href="/order?id=${new URL(dispute.url).searchParams.get('id')}"]`) });
    await resolution.getByRole('radio', { name: `Refund ${price} (test network) to chain_buyer — exactly the price; network fee deducted`, exact: true }).check();
    await resolution.getByLabel('Decision', { exact: true }).fill('Return this isolated test payment to the buyer.');
    await resolution.getByRole('checkbox', { name: 'I checked the amount and who receives it for the outcome I chose. Resolving is final.' }).check();
    await resolution.getByLabel('Current password', { exact: true }).fill(password); // A-32
    await resolution.getByRole('button', { name: 'Resolve dispute' }).click();
    await buyer.goto(dispute.url);
    await refreshUntil(buyer, async () => {
      await expect(buyer.locator('.page-head .badge')).toHaveText('Resolved');
      await expect(buyer.locator('.payment-figures div').filter({ has: buyer.getByText('Payout status', { exact: true }) })).toContainText('Sent', { timeout: 1000 });
    });
    await chain('mine', currency, '12');
    await verifyReceipt(currency, refundAddress, total);
  }
  await Promise.all([buyer.context().close(), vendor.context().close(), moderator.context().close()]);
});

// A-103: overpaid, large and underpaid legs on the same real chains, each with fresh accounts (their own password
// confirmation budgets) and fresh recipient addresses, so every receipt below is exactly one payout.
const priceOf = (currency: string) => currency === 'BTC' ? 100_000 : 500_000_000_000;
// Atomic units as the pages print them: 150000 satoshi is "0.0015", 800000000000 piconero "0.8".
function coins(currency: string, atomic: number) {
  const decimals = currency === 'BTC' ? 8 : 12;
  const digits = String(atomic).padStart(decimals + 1, '0');
  const fraction = digits.slice(-decimals).replace(/0+$/, '');
  return digits.slice(0, -decimals) + (fraction ? '.' + fraction : '');
}
async function legParties(browser: Browser, baseURL: string | undefined, leg: string) {
  const admin = await signedIn(browser, baseURL, 'chain_admin', password, false);
  const vendor = await signedIn(browser, baseURL, `chain_${leg}_vendor`, password, true);
  const buyer = await signedIn(browser, baseURL, `chain_${leg}_buyer`, password, true);
  await setRole(admin, `chain_${leg}_vendor`, 'vendor');
  return { admin, vendor, buyer, close: () => Promise.allSettled([admin, vendor, buyer].map(page => page.context().close())) };
}
async function saveAddress(page: Page, currency: string, address: string) {
  await page.goto('/account');
  const form = page.locator('form', { has: page.getByRole('button', { name: `Save ${currency} address` }) });
  await form.locator('input[name="address"]').fill(address);
  await form.getByLabel('Current password', { exact: true }).fill(password);
  await submitAndLoad(page, form.getByRole('button'));
  await expect(form.locator('input[name="address"]')).toHaveValue(address);
}
async function publishDigitalListing(vendor: Page, title: string) {
  await vendor.goto('/vendor-dashboard');
  await vendor.getByText('+ Publish a new listing', { exact: true }).click();
  await vendor.getByLabel('Listing title').fill(title);
  await vendor.getByLabel('Description', { exact: true }).fill('Isolated real node browser test. Locally mined test coins only.');
  await vendor.getByLabel('Fulfillment type').selectOption('digital');
  await vendor.getByLabel('Stock', { exact: true }).fill('3');
  await vendor.getByLabel('Bitcoin reference price').fill('0.001');
  await vendor.getByLabel('Monero reference price').fill('0.5');
  await submitAndLoad(vendor, vendor.getByRole('button', { name: 'Publish listing' }));
}
async function placeOrder(buyer: Page, title: string, currency: string) {
  await buyer.goto(`/?q=${encodeURIComponent(title)}`);
  await buyer.getByRole('link', { name: title, exact: true }).click();
  await buyer.getByRole('link', { name: 'Review order draft' }).click();
  await buyer.getByLabel('Reference currency').selectOption(currency);
  await buyer.getByRole('button', { name: 'Create unfunded draft' }).click();
  await buyer.getByRole('button', { name: 'Request payment address' }).click();
  await expect(buyer.locator('.page-head .badge')).toHaveText('Awaiting payment');
  const url = buyer.url();
  return { url, id: new URL(url).searchParams.get('id')!, address: (await buyer.locator('.payment-address .mono').innerText()).trim() };
}
// Pays the order with one deposit of paid, confirms it and waits for Paid; the vendor then delivers.
async function payAndDeliver(buyer: Page, vendor: Page, currency: string, order: { url: string, address: string }, paid: number) {
  await chain('deposit', currency, order.address, coins(currency, paid));
  await chain('mine', currency, '12');
  await refreshUntil(buyer, async () => { await expect(buyer.locator('.page-head .badge')).toHaveText('Paid', { timeout: 1000 }); });
  await vendor.goto(order.url);
  await vendor.getByLabel('Delivery content', { exact: true }).fill('Real-chain test digital delivery');
  await submitAndLoad(vendor, vendor.getByRole('button', { name: 'Deliver digital content', exact: true }));
  await buyer.reload();
  await expect(buyer.locator('.page-head .badge')).toHaveText('Delivered');
  await expect(buyer.locator('input[name="amount_seen"]')).toHaveValue(String(paid));
}
const payoutStatus = (page: Page, kind: string) => page.locator('.payment-figures', { has: page.getByText(kind, { exact: true }) })
  .locator('div', { has: page.getByText('Payout status', { exact: true }) });

test('isolated real chains: an overpaid order releases the price and returns the excess, each sent once', async ({ browser, baseURL }) => {
  test.setTimeout(900_000);
  const { vendor, buyer, close } = await legParties(browser, baseURL, 'overpaid');
  try {
    for (const currency of currencies) {
      // The excess is below the price, so its return is sent without review.
      const paid = currency === 'BTC' ? 150_000 : 800_000_000_000, excess = paid - priceOf(currency);
      const vendorAddress = (await chain('address', currency)).address, returnAddress = (await chain('address', currency)).address;
      await saveAddress(vendor, currency, vendorAddress);
      await saveAddress(buyer, currency, returnAddress);
      const title = `Local-chain ${currency} overpaid listing`;
      await publishDigitalListing(vendor, title);
      const order = await placeOrder(buyer, title, currency);
      await payAndDeliver(buyer, vendor, currency, order, paid);
      await expect(buyer.getByRole('note').filter({ hasText: 'Overpaid.' })).toContainText(`You sent ${coins(currency, excess)} ${currency} more than the price (test network). Completing releases the price to the vendor and returns the excess to your saved ${currency} payout address, with the network fee deducted.`);
      await buyer.getByRole('checkbox', { name: `Release ${coins(currency, priceOf(currency))} ${currency} (test network) to chain_overpaid_vendor (the price) and return the ${coins(currency, excess)} ${currency} paid over the price to chain_overpaid_buyer; network fee deducted from each. This is final.`, exact: true }).check();
      await submitAndLoad(buyer, buyer.getByRole('button', { name: 'Confirm receipt and complete' }));
      await expectNotice(buyer, 'Order completed.');
      await refreshUntil(buyer, async () => {
        await expect(buyer.locator('.page-head .badge')).toHaveText('Completed');
        await expect(payoutStatus(buyer, 'Release to vendor')).toContainText('Sent', { timeout: 1000 });
        await expect(payoutStatus(buyer, 'Return to buyer')).toContainText('Sent', { timeout: 1000 });
      });
      const rows = payouts(order.id);
      expect(rows).toEqual([
        expect.objectContaining({ kind: 'release', amount: priceOf(currency), state: 'sent', ambiguous: false, address: vendorAddress, error: '' }),
        expect.objectContaining({ kind: 'return', amount: excess, state: 'sent', ambiguous: false, address: returnAddress, error: '' }),
      ]);
      noAmbiguousSends();
      await expect(buyer.getByRole('heading', { name: 'Return of the amount paid over the price' })).toBeVisible();
      await expect(buyer.locator('main')).toContainText(rows[1].txid);
      await vendor.goto(order.url);
      await expect(payoutStatus(vendor, 'Return to buyer')).toContainText('Sent');
      await expect(vendor.locator('main')).not.toContainText(returnAddress);
      await chain('mine', currency, '12');
      await verifyReceipt(currency, vendorAddress, priceOf(currency), rows[0].txid);
      await verifyReceipt(currency, returnAddress, excess, rows[1].txid);
      // Further watcher passes send nothing more.
      await watcherCycles(currency);
      await verifyReceipt(currency, vendorAddress, priceOf(currency), rows[0].txid);
      await verifyReceipt(currency, returnAddress, excess, rows[1].txid);
      expect(payouts(order.id)).toEqual(rows);
      noAmbiguousSends();
    }
  } finally {
    await close();
  }
});

test('isolated real chains: a return larger than the price is held until an administrator releases it, then sent once', async ({ browser, baseURL }) => {
  test.setTimeout(900_000);
  const { admin, vendor, buyer, close } = await legParties(browser, baseURL, 'large');
  try {
    for (const currency of currencies) {
      // Two and a half times the price: the excess is more than the price, so its return is held (largeHold).
      const paid = currency === 'BTC' ? 250_000 : 1_250_000_000_000, excess = paid - priceOf(currency);
      const vendorAddress = (await chain('address', currency)).address, returnAddress = (await chain('address', currency)).address;
      await saveAddress(vendor, currency, vendorAddress);
      await saveAddress(buyer, currency, returnAddress);
      const title = `Local-chain ${currency} large return listing`;
      await publishDigitalListing(vendor, title);
      const order = await placeOrder(buyer, title, currency);
      await payAndDeliver(buyer, vendor, currency, order, paid);
      await expect(buyer.getByRole('note').filter({ hasText: 'Overpaid.' })).toContainText(`You sent ${coins(currency, excess)} ${currency} more than the price (test network). Completing releases the price to the vendor. The excess is more than the price, so an administrator reviews its return to your saved ${currency} payout address (network fee deducted) before it is sent.`);
      await buyer.getByRole('checkbox', { name: `Release ${coins(currency, priceOf(currency))} ${currency} (test network) to chain_large_vendor (the price) and return the ${coins(currency, excess)} ${currency} paid over the price to chain_large_buyer; network fee deducted from each; the return is more than the price, so an administrator reviews it before it is sent. This is final.`, exact: true }).check();
      await submitAndLoad(buyer, buyer.getByRole('button', { name: 'Confirm receipt and complete' }));
      await expectNotice(buyer, 'Order completed.');
      await refreshUntil(buyer, async () => {
        await expect(buyer.locator('.page-head .badge')).toHaveText('Completed');
        await expect(payoutStatus(buyer, 'Release to vendor')).toContainText('Sent', { timeout: 1000 });
        await expect(payoutStatus(buyer, 'Return to buyer')).toContainText('Held for review', { timeout: 1000 });
      });
      await expect(buyer.locator('main')).toContainText('has not been sent. It is more than the order price, so an administrator reviews it before it is sent.');
      let rows = payouts(order.id);
      expect(rows).toEqual([
        expect.objectContaining({ kind: 'release', amount: priceOf(currency), state: 'sent', ambiguous: false, address: vendorAddress, error: '' }),
        expect.objectContaining({ kind: 'return', amount: excess, state: 'held', ambiguous: false, address: returnAddress, txid: '' }),
      ]);
      expect(rows[1].error.startsWith('Larger than the order price:')).toBe(true);
      await chain('mine', currency, '12');
      await verifyReceipt(currency, vendorAddress, priceOf(currency), rows[0].txid);
      // The watcher never lifts this hold: after more passes nothing has reached the buyer.
      await watcherCycles(currency);
      expect(payouts(order.id)).toEqual(rows);
      expect((await chain('received', currency, returnAddress)).transactions).toBe(0);
      noAmbiguousSends();

      await admin.goto('/admin');
      const row = () => admin.locator('tr', { has: admin.getByText(order.id, { exact: true }) }).filter({ hasText: 'Return of overpayment' });
      await expect(row()).toContainText(`${coins(currency, excess)} ${currency}`);
      await expect(row()).toContainText('Held: larger than the order price — review before releasing');
      const link = row().getByRole('link', { name: /^Resolve payout \d+$/ });
      const target = (await link.getAttribute('href'))!;
      await link.click();
      const details = admin.locator(`details.payout-actions${target}`);
      await details.locator('summary').click();
      const release = details.locator('form', { has: admin.getByRole('button', { name: 'Release held payout', exact: true }) });
      await release.getByRole('checkbox', { name: 'Reviewed: this payment is a genuine mistake and may be returned', exact: true }).check();
      await release.getByLabel('Current password', { exact: true }).fill(password);
      await submitAndLoad(admin, release.getByRole('button', { name: 'Release held payout', exact: true }));
      await expectNotice(admin, 'Payout released.');
      await refreshUntil(buyer, async () => {
        await expect(payoutStatus(buyer, 'Return to buyer')).toContainText('Sent', { timeout: 1000 });
      });
      rows = payouts(order.id);
      expect(rows).toEqual([
        expect.objectContaining({ kind: 'release', amount: priceOf(currency), state: 'sent', ambiguous: false }),
        expect.objectContaining({ kind: 'return', amount: excess, state: 'sent', ambiguous: false, address: returnAddress, error: '' }),
      ]);
      noAmbiguousSends();
      await expect(buyer.locator('main')).toContainText(rows[1].txid);
      await chain('mine', currency, '12');
      await verifyReceipt(currency, returnAddress, excess, rows[1].txid);
      await watcherCycles(currency);
      await verifyReceipt(currency, vendorAddress, priceOf(currency), rows[0].txid);
      await verifyReceipt(currency, returnAddress, excess, rows[1].txid);
      expect(payouts(order.id)).toEqual(rows);
      noAmbiguousSends();
      await admin.reload();
      await expect(row()).toContainText(rows[1].txid);
      await expect(row().getByRole('link', { name: /^Resolve payout / })).toHaveCount(0);
    }
  } finally {
    await close();
  }
});

test('isolated real chains: cancelling an underpaid order refunds the confirmed deposit once to the saved address', async ({ browser, baseURL }) => {
  test.setTimeout(900_000);
  const { vendor, buyer, close } = await legParties(browser, baseURL, 'underpaid');
  try {
    for (const currency of currencies) {
      const paid = currency === 'BTC' ? 40_000 : 200_000_000_000, remaining = priceOf(currency) - paid;
      const refundAddress = (await chain('address', currency)).address;
      await saveAddress(buyer, currency, refundAddress);
      const title = `Local-chain ${currency} underpaid listing`;
      await publishDigitalListing(vendor, title);
      const order = await placeOrder(buyer, title, currency);
      await chain('deposit', currency, order.address, coins(currency, paid));
      await chain('mine', currency, '12');
      const refund = buyer.getByRole('checkbox', { name: `Refund ${coins(currency, paid)} ${currency} (test network) to chain_underpaid_buyer — ${coins(currency, remaining)} ${currency} less than the price; network fee deducted. This is final.`, exact: true });
      await refreshUntil(buyer, async () => {
        await expect(buyer.locator('.payment-figures div').filter({ has: buyer.getByText('Remaining to send', { exact: true }) }))
          .toContainText(coins(currency, remaining), { timeout: 1000 });
        await expect(refund).toBeVisible({ timeout: 1000 });
      });
      await expect(buyer.locator('.page-head .badge')).toHaveText('Awaiting payment');
      await expect(buyer.locator('main')).toContainText(`A deposit has arrived but does not cover the price. Cancelling returns one unit of stock and queues one refund of every confirmed deposit to your saved ${currency} payout address (it waits until you save one)`);
      await expect(buyer.locator('input[name="amount_seen"]')).toHaveValue(String(paid));
      await refund.check();
      await submitAndLoad(buyer, buyer.getByRole('button', { name: 'Cancel order', exact: true }));
      await expectNotice(buyer, 'Order cancelled.');
      await refreshUntil(buyer, async () => {
        await expect(buyer.locator('.page-head .badge')).toHaveText('Cancelled');
        await expect(payoutStatus(buyer, 'Refund to buyer')).toContainText('Sent', { timeout: 1000 });
      });
      const rows = payouts(order.id);
      expect(rows).toEqual([expect.objectContaining({ kind: 'refund', amount: paid, state: 'sent', ambiguous: false, address: refundAddress, error: '' })]);
      noAmbiguousSends();
      await expect(buyer.locator('main')).toContainText(rows[0].txid);
      await chain('mine', currency, '12');
      await verifyReceipt(currency, refundAddress, paid, rows[0].txid);
      await watcherCycles(currency);
      await verifyReceipt(currency, refundAddress, paid, rows[0].txid);
      expect(payouts(order.id)).toEqual(rows);
      noAmbiguousSends();
    }
  } finally {
    await close();
  }
});

// A-103 / A-211: an underpaid order left alone is cancelled by the watcher once the payment window (PAYMENT_EXPIRY,
// 10 minutes in this run) has passed since both the address and the last deposit, and the confirmed deposit is
// refunded once. A top-up sent to the same address afterwards is recorded after the close and flagged for staff,
// never paid out. Both coins' orders are funded first, so the window is waited out once.
test('isolated real chains: an underpaid order expires with one refund, and a later top-up is flagged, not paid', async ({ browser, baseURL }) => {
  test.setTimeout(1_800_000);
  const moderator = await signedIn(browser, baseURL, 'chain_moderator', password, false);
  const legs: { currency: string, parties: Awaited<ReturnType<typeof legParties>>, order: Awaited<ReturnType<typeof placeOrder>>, paid: number, refundAddress: string }[] = [];
  try {
    for (const currency of currencies) {
      const parties = await legParties(browser, baseURL, `expiry_${currency.toLowerCase()}`);
      const paid = priceOf(currency) * 2 / 5, remaining = priceOf(currency) - paid;
      const refundAddress = (await chain('address', currency)).address;
      await saveAddress(parties.buyer, currency, refundAddress);
      const title = `Local-chain ${currency} underpaid expiry listing`;
      await publishDigitalListing(parties.vendor, title);
      const order = await placeOrder(parties.buyer, title, currency);
      await chain('deposit', currency, order.address, coins(currency, paid));
      await chain('mine', currency, '12');
      await refreshUntil(parties.buyer, async () => {
        await expect(parties.buyer.locator('.payment-figures div').filter({ has: parties.buyer.getByText('Remaining to send', { exact: true }) }))
          .toContainText(coins(currency, remaining), { timeout: 1000 });
      });
      legs.push({ currency, parties, order, paid, refundAddress });
    }
    for (const { currency, parties: { buyer, vendor }, order, paid, refundAddress } of legs) {
      const short = order.id.slice(0, 8);
      // The watcher cancels it about 10 minutes after the deposit was first seen.
      await expect(async () => {
        await buyer.reload();
        await expect(buyer.locator('.page-head .badge')).toHaveText('Cancelled', { timeout: 1000 });
        await expect(payoutStatus(buyer, 'Refund to buyer')).toContainText('Sent', { timeout: 1000 });
      }).toPass({ timeout: 900_000, intervals: [5000] });
      const rows = payouts(order.id);
      expect(rows).toEqual([expect.objectContaining({ kind: 'refund', amount: paid, state: 'sent', ambiguous: false, address: refundAddress, error: '' })]);
      noAmbiguousSends();
      await expect(buyer.locator('main')).toContainText(rows[0].txid);
      const expired = `Order ${short}: the payment window ended with ${coins(currency, paid)} of ${coins(currency, priceOf(currency))} ${currency} received, so the order was cancelled. A refund of ${coins(currency, paid)} ${currency} to your saved ${currency} payout address is queued; the network fee is taken from it.`;
      await buyer.goto('/notifications');
      await expect(buyer.locator('article.message').filter({ hasText: expired })).toHaveCount(1);
      await chain('mine', currency, '12');
      await verifyReceipt(currency, refundAddress, paid, rows[0].txid);

      // A late top-up to the same address: recorded after the close, flagged, never paid out.
      const late = priceOf(currency) * 3 / 10;
      const lateTxid: string = (await chain('deposit', currency, order.address, coins(currency, late))).txid;
      expect(lateTxid).toMatch(/^[0-9a-f]{64}$/);
      await chain('mine', currency, '12');
      const lateRow = () => sql(`SELECT amount||'|'||after_close||'|'||flagged||'|'||credited FROM payments WHERE order_id='${order.id}' AND txid='${lateTxid}'`);
      await expect.poll(lateRow, { timeout: 60_000, intervals: [500] }).toBe(`${late}|true|true|false`);
      const told = `Order ${short}: ${coins(currency, late)} ${currency} (TESTNET) arrived at this order's deposit address after the order closed and was not paid out; it is not refunded automatically. Market staff were notified and review it.`;
      await refreshUntil(buyer, async () => { await expect(buyer.locator('article.message').filter({ hasText: told })).toHaveCount(1, { timeout: 1000 }); });
      await expect(buyer.locator('main')).not.toContainText(lateTxid);
      await vendor.goto('/notifications');
      await expect(vendor.locator('main')).not.toContainText('after the order closed');
      await moderator.goto('/moderator');
      const flag = moderator.locator('tr.payment-review-row').filter({ hasText: order.id }).filter({ hasText: lateTxid });
      await expect(flag).toContainText('Open: Deposit after settlement or after the order closed, not paid out');
      await expect(flag).toContainText(`${coins(currency, late)} ${currency}`);
      expect(payouts(order.id)).toEqual(rows);
      await verifyReceipt(currency, refundAddress, paid, rows[0].txid);
      // Further watcher passes pay nothing more and tell the buyer nothing more.
      await watcherCycles(currency);
      expect(payouts(order.id)).toEqual(rows);
      expect(lateRow()).toBe(`${late}|true|true|false`);
      await verifyReceipt(currency, refundAddress, paid, rows[0].txid);
      await buyer.reload();
      await expect(buyer.locator('article.message').filter({ hasText: told })).toHaveCount(1);
      noAmbiguousSends();
    }
  } finally {
    await Promise.allSettled([moderator.context().close(), ...legs.map(leg => leg.parties.close())]);
  }
});
