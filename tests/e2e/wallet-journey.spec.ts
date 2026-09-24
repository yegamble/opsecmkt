import { expect, test, type Page } from '@playwright/test';
import { signedIn } from './db-helpers';

// Real server/PostgreSQL and production RPC adapters, deterministic simulated wallet responses.
// This proves application integration, not real-chain settlement or cryptographic address validation.
const password = 'wallet-browser-admin-password-123';
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

test('BTC and XMR: partial payment, intake pause, confirmation, fulfillment and one payout', async ({ page: admin, browser, baseURL, request }) => {
  await admin.goto('/setup');
  await admin.getByLabel('Setup token').fill('e2e-wallet-local-setup-token-at-least-32-characters');
  await admin.getByLabel('Handle', { exact: true }).fill('wallet_admin');
  await admin.getByLabel('Password', { exact: true }).fill(password);
  await admin.getByRole('button', { name: 'Initialize marketplace' }).click();
  await expect(admin).toHaveURL(/\/admin\?welcome=1$/);
  await admin.locator('.admin-auth').getByRole('button', { name: 'Turn CAPTCHA off' }).click();
  const buyer = await signedIn(browser, baseURL, 'wallet_buyer', 'wallet-browser-buyer-password-123', true);
  const second = await signedIn(browser, baseURL, 'wallet_second', 'wallet-browser-second-password-123', true);

  for (const currency of ['BTC', 'XMR']) {
    const digital = currency === 'XMR';
    const destination = digital ? '5' + 'a'.repeat(94) : 'bcrt1qfixturepayout000000000000';
    await admin.goto('/account');
    const payout = admin.locator('form', { has: admin.getByRole('button', { name: `Save ${currency} address` }) });
    await payout.locator('input[name="address"]').fill(destination);
    await payout.getByLabel('Current password', { exact: true }).fill(password);
    await payout.getByRole('button').click();
    await expect(payout.locator('input[name="address"]')).toHaveValue(destination);
    await admin.goto('/vendor-dashboard');
    await admin.getByText('+ Publish a new listing', { exact: true }).click();
    const title = `Wallet ${currency} journey`;
    await admin.getByLabel('Listing title').fill(title);
    await admin.getByLabel('Description', { exact: true }).fill('Deterministic RPC fixture journey; no real coins.');
    await admin.getByLabel('Fulfillment type').selectOption(digital ? 'digital' : 'physical');
    await admin.getByLabel('Stock', { exact: true }).fill('3');
    await admin.getByLabel('Bitcoin reference price').fill('0.001');
    await admin.getByLabel('Monero reference price').fill('0.5');
    await admin.getByRole('button', { name: 'Publish listing' }).click();
    await buyer.goto(`/?q=${encodeURIComponent(title)}`);
    await buyer.getByRole('link', { name: title, exact: true }).click();
    await buyer.waitForURL(/\/product\?id=/);
    const productURL = buyer.url();
    await buyer.getByRole('link', { name: 'Review order draft' }).click();
    await buyer.getByLabel('Reference currency').selectOption(currency);
    await buyer.getByRole('button', { name: 'Create unfunded draft' }).click();
    await buyer.getByRole('button', { name: 'Request payment address' }).click();
    await buyer.waitForURL(/&saved=1$/);
    const orderURL = buyer.url();
    await expect(buyer.locator('.page-head .badge')).toHaveText('Awaiting payment');
    const address = (await buyer.locator('.payment-address .mono').innerText()).trim();
    await admin.goto('/admin');
    const pause = admin.locator('form', { has: admin.getByRole('button', { name: `Pause new ${currency} payments` }) });
    await pause.getByLabel('Current password', { exact: true }).fill(password);
    await pause.getByRole('button').click();
    await expect(admin.getByRole('button', { name: `Enable new ${currency} payments` })).toBeVisible();
    await expect(admin.getByRole('button', { name: `Pause new ${digital ? 'BTC' : 'XMR'} payments` })).toBeVisible();
    await second.goto(productURL);
    await second.getByRole('link', { name: 'Review order draft' }).click();
    await second.getByLabel('Reference currency').selectOption(currency);
    await second.getByRole('button', { name: 'Create unfunded draft' }).click();
    await expect(second.getByRole('heading', { name: 'New payments paused' })).toBeVisible();
    await expect(second.getByRole('button', { name: 'Request payment address' })).toHaveCount(0);
    const total = digital ? 500_000_000_000 : 100_000;
    expect((await request.post(`${rpc}/test/deposit`, { data: { address, amount: total * 0.4 } })).ok()).toBeTruthy();
    const remaining = buyer.locator('.payment-figures div').filter({ has: buyer.getByText('Remaining to send', { exact: true }) });
    await reloadUntil(buyer, async () => { await expect(remaining).toContainText(digital ? '0.3' : '0.0006', { timeout: 500 }); });
    await expect(buyer.locator('.page-head .badge')).toHaveText('Awaiting payment');
    expect((await request.post(`${rpc}/test/deposit`, { data: { address, amount: total * 0.6 } })).ok()).toBeTruthy();
    await reloadUntil(buyer, async () => { await expect(buyer.getByText('Do not send another payment', { exact: false })).toBeVisible({ timeout: 500 }); });
    await expect(buyer.locator('.page-head .badge')).toHaveText('Awaiting payment');
    expect((await request.post(`${rpc}/test/confirm`, { data: { address } })).ok()).toBeTruthy();
    await reloadUntil(buyer, async () => { await expect(buyer.locator('.page-head .badge')).toHaveText('Paid', { timeout: 500 }); });
    // Existing addresses were monitored while intake was paused; now resume new requests.
    const resume = admin.locator('form', { has: admin.getByRole('button', { name: `Enable new ${currency} payments` }) });
    await resume.getByLabel('Current password', { exact: true }).fill(password);
    await submit(admin, resume.getByRole('button'));
    await second.reload();
    await expect(second.getByRole('button', { name: 'Request payment address' })).toBeVisible();
    await admin.goto(orderURL);
    if (digital) {
      await admin.getByLabel('Delivery content', { exact: true }).fill('Fixture digital delivery token');
      await submit(admin, admin.getByRole('button', { name: 'Deliver digital content', exact: true }));
    } else {
      // A-80: the note to the buyer warns against addresses and tracking numbers in plain text.
      await expect(admin.getByLabel('Note to the buyer (optional)')).toHaveAccessibleDescription(/^Stored unencrypted\..*Never include an address, real name or tracking number/);
      await submit(admin, admin.getByRole('button', { name: 'Mark as shipped' }));
    }
    await buyer.reload();
    if (digital) await expect(buyer.getByRole('region', { name: 'Digital delivery' })).toContainText('Fixture digital delivery token');
    await buyer.getByRole('button', { name: 'Confirm receipt and complete' }).click();
    await expect(buyer.locator('.page-head .badge')).toHaveText('Completed');
    await expect.poll(async () => ((await (await request.get(`${rpc}/test/state`)).json()).payouts as any[]).filter(p => p.currency === currency).length).toBe(1);
    const state = await (await request.get(`${rpc}/test/state`)).json();
    expect(state.payouts.filter((p: any) => p.currency === currency)).toEqual([expect.objectContaining({ address: destination, amount: total })]);
    await reloadUntil(buyer, async () => { await expect(buyer.locator('.payment-figures div').filter({ has: buyer.getByText('Payout status', { exact: true }) })).toContainText('Sent', { timeout: 500 }); });
    await buyer.reload();
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
    await expect(publicPage.locator('main')).toContainText('Verified purchase');
    await expect(publicPage.locator('main')).not.toContainText('wallet_buyer');
    await expect(publicPage.locator('main')).not.toContainText(new URL(orderURL).searchParams.get('id')!);
    await publicContext.close();
  }
  await buyer.context().close();
  await second.context().close();
});

test('funded dispute: encrypted moderator evidence, independent resolution and one buyer refund', async ({ browser, baseURL, request }) => {
  const { setRole, pgpFixture } = await import('./db-helpers');
  const admin = await signedIn(browser, baseURL, 'wallet_admin', password, false);
  const buyer = await signedIn(browser, baseURL, 'wallet_buyer', 'wallet-browser-buyer-password-123', false);
  const moderator = await signedIn(browser, baseURL, 'wallet_moderator', 'wallet-browser-moderator-password-123', true);
  await setRole(admin, 'wallet_moderator', 'moderator');
  await moderator.goto('/account');
  await moderator.getByLabel('PGP public key', { exact: true }).fill(pgpFixture('recipient.pub.asc'));
  await moderator.getByRole('button', { name: 'Save profile' }).click();
  await buyer.goto('/account');
  const refundAddress = 'bcrt1qfixturebuyerrefund00000000';
  const addressForm = buyer.locator('form', { has: buyer.getByRole('button', { name: 'Save BTC address' }) });
  await addressForm.locator('input[name="address"]').fill(refundAddress);
  await addressForm.getByLabel('Current password', { exact: true }).fill('wallet-browser-buyer-password-123');
  await addressForm.getByRole('button').click();
  await buyer.goto('/?q=Wallet%20BTC%20journey');
  await buyer.getByRole('link', { name: 'Wallet BTC journey', exact: true }).click();
  await buyer.getByRole('link', { name: 'Review order draft' }).click();
  await buyer.getByRole('button', { name: 'Create unfunded draft' }).click();
  await buyer.getByRole('button', { name: 'Request payment address' }).click();
  await buyer.waitForURL(/&saved=1$/);
  const orderURL = buyer.url();
  const address = (await buyer.locator('.payment-address .mono').innerText()).trim();
  expect((await request.post(`${rpc}/test/deposit`, { data: { address, amount: 100_000 } })).ok()).toBeTruthy();
  expect((await request.post(`${rpc}/test/confirm`, { data: { address } })).ok()).toBeTruthy();
  await reloadUntil(buyer, async () => { await expect(buyer.locator('.page-head .badge')).toHaveText('Paid', { timeout: 500 }); });
  // A-80: both dispute forms warn that the reason is plain text and point to the encrypted staff contacts.
  await buyer.goto('/disputes');
  await expect(buyer.getByLabel('Describe the issue')).toHaveAccessibleDescription(/^Stored unencrypted\..*Never include an address, real name or tracking number.*Contact dispute staff/);
  await buyer.goto(orderURL);
  await buyer.getByText('Open a dispute', { exact: true }).click();
  await expect(buyer.getByLabel('Describe the issue')).toHaveAccessibleDescription(/^Stored unencrypted\..*Never include an address, real name or tracking number.*Contact dispute staff/);
  await buyer.getByLabel('Describe the issue').fill('Please review the encrypted evidence sent to the independent moderator.');
  await buyer.getByRole('button', { name: 'Submit dispute' }).click();
  await buyer.goto(orderURL);
  await expect(buyer.locator('.page-head .badge')).toHaveText('Disputed');
  const staff = buyer.getByRole('region', { name: 'Contact dispute staff' });
  await expect(staff).not.toContainText('wallet_admin');
  await staff.locator('summary', { hasText: 'wallet_moderator' }).click();
  await expect(staff).toContainText('Ownership not verified');
  await staff.getByRole('link', { name: 'Message wallet_moderator' }).click();
  await expect(buyer.getByLabel('Recipient handle', { exact: true })).toHaveValue('wallet_moderator');
  await buyer.getByLabel('PGP encrypted message').fill(pgpFixture('message-to-recipient.asc'));
  await buyer.getByRole('button', { name: 'Send encrypted text ↗' }).click();
  await moderator.goto('/messages');
  await expect(moderator.locator('article.message pre')).toHaveText(pgpFixture('message-to-recipient.asc').trim());
  await expect(moderator.locator('.pgp-message-status')).toContainText('to recipient’s key');
  await admin.goto('/moderator');
  await expect(admin.getByRole('button', { name: 'Resolve dispute' })).toHaveCount(0);
  await moderator.goto('/moderator');
  await moderator.getByRole('radio', { name: 'Refund to buyer' }).check();
  await moderator.getByLabel('Decision', { exact: true }).fill('Fixture review completed: return the confirmed test payment to the buyer.');
  await moderator.getByRole('button', { name: 'Resolve dispute' }).click();
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
    await buyer.waitForURL(/&saved=1$/);
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
    await submit(vendor, vendor.getByRole('button', { name: 'Cancel order', exact: true }));
    await buyer.reload();
    await expect(buyer.locator('.page-head .badge')).toHaveText('Cancelled');
    await reloadUntil(buyer, async () => { await expect(buyer.locator('.payment-figures div', { has: buyer.getByText('Payout status', { exact: true }) })).toContainText('Sent', { timeout: 500 }); });
    const payouts = (await (await request.get(`${rpc}/test/state`)).json()).payouts;
    expect(payouts).toHaveLength(beforePayouts + 1);
    expect(payouts.at(-1)).toEqual(expect.objectContaining({ currency: 'BTC', address: 'bcrt1qfixturebuyerrefund00000000', amount: 100_000 }));
    await buyer.goto(productURL);
    await expect(stock.locator('dd')).toHaveText(String(before));
    await vendor.reload();
    await expect(vendor.getByRole('button', { name: 'Cancel order', exact: true })).toHaveCount(0);
  } finally {
    await Promise.allSettled([vendor.context().close(), buyer.context().close()]);
  }
});

test('automatic digital delivery and administrator recovery of a rejected payout', async ({ browser, baseURL, request }) => {
  const admin = await signedIn(browser, baseURL, 'wallet_admin', password, false);
  const buyer = await signedIn(browser, baseURL, 'wallet_buyer', 'wallet-browser-buyer-password-123', false);
  try {
    await admin.goto('/vendor-dashboard');
    await admin.getByRole('link', { name: 'Edit Wallet XMR journey', exact: true }).click();
    const content = 'Private automatically delivered fixture download token';
    await admin.getByLabel('Automatic delivery content').fill(content);
    await admin.getByRole('button', { name: 'Save changes' }).click();
    await buyer.goto('/?q=Wallet%20XMR%20journey');
    await buyer.getByRole('link', { name: 'Wallet XMR journey', exact: true }).click();
    await expect(buyer.locator('main')).not.toContainText(content);
    await buyer.getByRole('link', { name: 'Review order draft' }).click();
    await buyer.getByLabel('Reference currency').selectOption('XMR');
    await buyer.getByRole('button', { name: 'Create unfunded draft' }).click();
    await buyer.getByRole('button', { name: 'Request payment address' }).click();
    await buyer.waitForURL(/&saved=1$/);
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
    await buyer.getByRole('button', { name: 'Confirm receipt and complete' }).click();
    await reloadUntil(buyer, async () => { await expect(buyer.locator('.payment-figures div', { has: buyer.getByText('Payout status', { exact: true }) })).toContainText('Failed', { timeout: 500 }); });
    await admin.goto('/admin');
    let payout = admin.locator('tr', { has: admin.getByText(orderID, { exact: true }) });
    await expect(payout).toContainText('Fixture rejected send before broadcast');
    const checks = (await (await request.get(`${rpc}/test/state`)).json()).checks;
    await expect.poll(async () => (await (await request.get(`${rpc}/test/state`)).json()).checks).toBeGreaterThan(checks + 2);
    expect((await (await request.get(`${rpc}/test/state`)).json()).payouts).toHaveLength(before);
    await payout.locator('summary').click();
    let requeue = payout.locator('form', { has: admin.getByRole('button', { name: 'Requeue payout', exact: true }) });
    await requeue.getByLabel('Current password', { exact: true }).fill('incorrect-password');
    await requeue.getByRole('button').click();
    await expect(admin.locator('body')).toContainText('Password incorrect');
    await admin.goto('/admin');
    payout = admin.locator('tr', { has: admin.getByText(orderID, { exact: true }) });
    await payout.locator('summary').click();
    requeue = payout.locator('form', { has: admin.getByRole('button', { name: 'Requeue payout', exact: true }) });
    await requeue.getByLabel('Current password', { exact: true }).fill(password);
    await submit(admin, requeue.getByRole('button'));
    await reloadUntil(buyer, async () => { await expect(buyer.locator('.payment-figures div', { has: buyer.getByText('Payout status', { exact: true }) })).toContainText('Sent', { timeout: 500 }); });
    expect((await (await request.get(`${rpc}/test/state`)).json()).payouts).toHaveLength(before + 1);
    await admin.reload();
    await expect(admin.locator('tr', { has: admin.getByText(orderID, { exact: true }) })).not.toContainText('Requeue payout');
    await expect(admin.locator('main')).toContainText('Requeued payout');
  } finally {
    await Promise.allSettled([admin.context().close(), buyer.context().close()]);
  }
});
