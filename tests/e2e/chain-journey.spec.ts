import { expect, test, type Page } from '@playwright/test';
import { execFile } from 'node:child_process';
import { promisify } from 'node:util';
import { signedIn, setRole } from './db-helpers';

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
async function verifyReceipt(currency: string, address: string, total: number) {
  const receipt = await chain('received', currency, address);
  expect(receipt.transactions).toBe(1);
  expect(receipt.amount).toBeGreaterThan(0);
  if (currency === 'BTC') {
    expect(receipt.fee).toBeGreaterThan(0);
    expect(receipt.amount + receipt.fee).toBe(total);
  } else expect(receipt.amount).toBe(total);
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
    await buyer.getByRole('button', { name: 'Confirm receipt and complete' }).click();
    await expect(buyer.locator('.page-head .badge')).toHaveText('Completed');
    await refreshUntil(buyer, async () => {
      await expect(buyer.locator('.payment-figures div').filter({ has: buyer.getByText('Payout status', { exact: true }) })).toContainText('Sent', { timeout: 1000 });
    });
    await expect(buyer.locator('main')).toContainText(currency === 'BTC' ? 'Bitcoin network fee is deducted' : 'market wallet pays the Monero network fee separately');
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
    await resolution.getByRole('radio', { name: 'Refund to buyer' }).check();
    await resolution.getByLabel('Decision', { exact: true }).fill('Return this isolated test payment to the buyer.');
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
