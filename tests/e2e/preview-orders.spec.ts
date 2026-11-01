import { expect, test, type Locator } from '@playwright/test';
import { PREVIEW_DRAFT_ID } from './preview-routes';
import { waitForStyles } from './styles';

// P3 order views in the read-only preview (sample data, JavaScript disabled).
test('order page shows history, unavailable payment and the buyer actions', async ({ page }) => {
  await page.goto(`/order?id=${PREVIEW_DRAFT_ID}`);
  await expect(page.getByRole('heading', { name: 'History' })).toBeVisible();
  await expect(page.locator('.order-timeline li')).toHaveCount(1);
  await expect(page.locator('.key-values')).toContainText('Draft — unfunded');
  await expect(page.locator('form[action="/orders/pay"]')).toHaveCount(0);
  await expect(page.getByRole('button', { name: 'Request payment address' })).toHaveCount(0);
  await expect(page.getByRole('button', { name: 'Cancel draft' })).toBeVisible();
  // A-164: the order names its listing and links to it as it is now.
  const listing = page.locator('.key-values > div', { has: page.getByText('Listing', { exact: true }) }).locator('dd');
  await expect(listing.getByRole('link', { name: 'Encrypted USB Drive — 256GB', exact: true })).toHaveAttribute('href', '/product?id=encrypted-drive');
  await expect(listing).toContainText('Listing as it is now; it may have changed since purchase.');
  await expect(page.locator('script')).toHaveCount(0);
  expect(await page.evaluate(() => document.documentElement.scrollWidth)).toBeLessThanOrEqual(page.viewportSize()!.width);
});

test('plaintext order notes say who reads them and to send addresses encrypted', async ({ page }) => {
  // A-80: the cancel reason is stored unencrypted; its warning is the field's accessible description. The
  // preview draft carries only this note; the ship, dispute and review warnings are checked by the Go
  // database tests and the wallet journey.
  await page.goto(`/order?id=${PREVIEW_DRAFT_ID}`);
  await expect(page.getByLabel('Reason (optional)')).toHaveAccessibleDescription(
    /^Stored unencrypted\. The vendor reads it in the order history, .*moderators and administrators if the order is disputed .*the operator and anyone with a backup\. Never include an address, real name or tracking number\. Order correspondence becomes available after checkout\.$/);
  await expect(page.locator('#note-help-cancelled').getByRole('link', { name: 'Stored unencrypted.', exact: true })).toHaveAttribute('href', '/canary#records');
});

test('reviews are labelled verified purchases without reviewer handles', async ({ page }) => {
  await page.goto('/product?id=encrypted-drive');
  const reviews = page.locator('.order-reviews');
  await expect(reviews.getByRole('heading', { name: 'Verified reviews' })).toBeVisible();
  await expect(reviews.locator('.badge')).toHaveText('Completed order (test network)');
  await expect(reviews).toContainText('Rated 4 out of 5');
  await expect(reviews).not.toContainText('preview_user');
  // A-649: the policy is public; how many reviews it leaves out is not.
  await expect(reviews).toContainText('Reviews by banned accounts are not shown or counted.');
  await expect(reviews).not.toContainText('by a banned account is');
});

test('moderator desk offers no outcome for a dispute without escrow', async ({ page }) => {
  await page.goto('/moderator');
  await expect(page.locator('form[action="/resolve"]')).toHaveCount(0);
  await expect(page.getByRole('group', { name: 'Outcome' })).toHaveCount(0);
  await expect(page.locator('main')).toContainText('This order has no escrow, so no outcome can be recorded.');
  // A-124 and A-162 (sample data): the reply with its role and line breaks, whom each party messaged, and the
  // resolve help that says whether the other side replied and to ask the evidence holders first.
  await expect(page.locator('.dispute-text').nth(1)).toHaveText('Sample reply from the vendor.\nNothing was stored.');
  await expect(page.locator('.dispute-text').first()).toHaveCSS('white-space', 'pre-wrap');
  await expect(page.locator('.dispute-evidence li')).toHaveText(['Buyer → sample_moderator (1, latest Sample)', 'Vendor → none']);
  expect(await page.evaluate(() => document.documentElement.scrollWidth)).toBeLessThanOrEqual(page.viewportSize()!.width);
  await page.goto('/disputes');
  await expect(page.locator('.order-unavailable')).toContainText('Only paid, shipped, or delivered orders qualify');
  expect(await page.evaluate(() => document.documentElement.scrollWidth)).toBeLessThanOrEqual(page.viewportSize()!.width);
});

test('preview rejects order actions', async ({ page }) => {
  await page.goto(`/order?id=${PREVIEW_DRAFT_ID}`);
  await page.getByRole('button', { name: 'Cancel draft' }).click();
  await expect(page.locator('body')).toContainText('Read-only preview');
});


test('paid order keeps private correspondence and shipping links on the order page', async ({ page }) => {
  await page.goto('/order?id=8cf1aaf1bfa545a92504cdfe264306ee5f9d9ac801736734b173d8631a891cd9');
  await expect(page.getByRole('heading', { name: 'Order correspondence', exact: true })).toBeVisible();
  const form=page.locator('form[action="/orders/message"]');
  await expect(form.locator('input[name="order_id"]')).toHaveValue('8cf1aaf1bfa545a92504cdfe264306ee5f9d9ac801736734b173d8631a891cd9');
  await expect(form.locator('input[name="form_nonce"]')).toHaveValue(/^[0-9a-f]{32}$/);
  await expect(page.getByRole('link', { name: 'Send encrypted shipping details' })).toHaveAttribute('href','#order-messages');
  // A-716: the steps of an encryption that writes no plaintext file, and what the vendor can do with the address.
  const shipping=page.locator('.shipping-prompt');
  await expect(shipping.locator('ol li')).toHaveCount(4);
  await expect(shipping.locator('code').filter({ hasText: 'gpg --import vendor.asc' })).toBeVisible();
  await expect(shipping.locator('code').filter({ hasText: 'gpg --armor --encrypt --recipient <fingerprint>' })).toBeVisible();
  await expect(shipping).toContainText('no plaintext file is written');
  await expect(shipping).toContainText('the market cannot delete their copy.');
  await expect(page.getByRole('button', { name: 'Send order message' })).toBeVisible();
  await expect(page.locator('script')).toHaveCount(0);
  expect(await page.evaluate(() => document.documentElement.scrollWidth)).toBeLessThanOrEqual(page.viewportSize()!.width);
});

// A-721: the 64-hex id, not the status, used to fill a phone's width. Unscrolled, the next step and the status show;
// what still overflows (always at 200% text) scrolls by keyboard in a named wrapper, and the page never widens.
const insideWrapper = async (wrapper: Locator, item: Locator, label: string) => {
  const box = (await wrapper.boundingBox())!;
  const at = (await item.boundingBox())!;
  expect(at.x, label).toBeGreaterThanOrEqual(box.x);
  expect(at.x + at.width, label).toBeLessThanOrEqual(box.x + box.width);
};

for (const width of [320, 390, 768]) {
  test(`vendor desk at ${width}px: Waiting for you above the listings, its next step inside the wrapper`, async ({ page, browserName }) => {
    await page.setViewportSize({ width, height: 640 });
    await page.goto('/vendor-dashboard');
    await waitForStyles(page);
    const heading = page.getByRole('heading', { name: 'Waiting for you' });
    const y = (await heading.boundingBox())!.y;
    expect(y).toBeLessThanOrEqual(1280);
    expect(y).toBeLessThan((await page.locator('.stats').boundingBox())!.y);
    const wrapper = page.getByRole('region', { name: 'Orders waiting for you' });
    await expect(wrapper).toHaveAttribute('tabindex', '0');
    expect(await wrapper.evaluate(el => el.scrollLeft)).toBe(0);
    const row = wrapper.locator('tbody tr').first();
    await insideWrapper(wrapper, row.getByRole('link', { name: /^Mark shipped/ }), 'next-step link');
    await insideWrapper(wrapper, row.locator('td').nth(2).locator('small'), 'ship-by text');
    expect(await page.evaluate(() => document.documentElement.scrollWidth)).toBeLessThanOrEqual(width);

    const id = row.locator('small.long-id');
    await expect(id).toHaveText(/^[0-9a-f]{64}$/);
    if (browserName === 'chromium') {
      await id.dblclick();
      expect(await page.evaluate(() => getSelection()!.toString().trim())).toBe(await id.textContent());
    }

    const incoming = page.getByRole('region', { name: 'Incoming orders' });
    await expect(incoming).toHaveAttribute('tabindex', '0');
    const status = incoming.locator('tbody tr').first().locator('td').nth(1);
    await expect(status.locator('.badge')).toHaveText('Paid');
    await insideWrapper(incoming, status, 'desk status');
    if (width === 320) await insideWrapper(incoming, incoming.locator('tbody tr').first().locator('td').nth(2), 'desk payout');

    await page.locator('html').evaluate(element => { element.style.fontSize = '200%'; });
    await waitForStyles(page);
    expect(await page.evaluate(() => document.documentElement.scrollWidth), '200% text').toBeLessThanOrEqual(width);
  });

  test(`orders at ${width}px: status is visible unscrolled and the wrapper is a named Tab stop`, async ({ page }) => {
    await page.setViewportSize({ width, height: 640 });
    await page.goto('/orders');
    await waitForStyles(page);
    const wrapper = page.getByRole('region', { name: 'Your orders' });
    await expect(wrapper).toHaveAttribute('tabindex', '0');
    expect(await wrapper.evaluate(el => el.scrollLeft)).toBe(0);
    await expect(wrapper.locator('thead th').nth(1)).toHaveText('Status');
    for (const status of await wrapper.locator('tbody tr td:nth-child(2) .badge').all()) await insideWrapper(wrapper, status, 'order status');
    await page.locator('html').evaluate(element => { element.style.fontSize = '200%'; });
    await waitForStyles(page);
    expect(await page.evaluate(() => document.documentElement.scrollWidth), '200% text').toBeLessThanOrEqual(width);
  });
}
