import { expect, test } from '@playwright/test';
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
  await expect(page.getByRole('button', { name: 'Send order message' })).toBeVisible();
  await expect(page.locator('script')).toHaveCount(0);
  expect(await page.evaluate(() => document.documentElement.scrollWidth)).toBeLessThanOrEqual(page.viewportSize()!.width);
});

// A-521 (UI60-3): each order table scrolls sideways inside a named region that takes keyboard focus, so keyboard
// and screen-reader users reach its status and later columns while the page itself never scrolls sideways. A
// 64-hex order ID wraps inside its cell on a phone instead of widening the table, and copies as one unbroken ID.
for (const [path, name] of [['/orders', 'Your orders'], ['/vendor-dashboard', 'Incoming orders'], ['/moderator', 'Payment review flags']] as const) {
  test(`${path}: the order table is a named, focusable scroll region and long IDs wrap`, async ({ page }) => {
    await page.goto(path);
    await waitForStyles(page);
    const width = page.viewportSize()!.width;
    expect(await page.evaluate(() => document.documentElement.scrollWidth), `${path} page reflow`).toBeLessThanOrEqual(width);
    const region = page.getByRole('region', { name, exact: true });
    await expect(region).toHaveClass(/\btable-wrap\b/);
    await expect(region).toHaveAttribute('tabindex', '0');
    await expect(region.locator('table')).toHaveCount(1);
    await region.focus();
    await expect(region).toBeFocused();
    await expect(region).toHaveCSS('outline-style', 'solid');
    await expect(region).toHaveCSS('outline-width', '3px');
    const box = (await region.boundingBox())!;
    expect(box.x).toBeGreaterThanOrEqual(0);
    expect(box.x + box.width).toBeLessThanOrEqual(width);
    if (await region.evaluate(element => element.scrollWidth > element.clientWidth)) {
      // The focused region scrolls from the keyboard. WebKit can drop the first key press right after load, so the
      // press is retried until the region moves.
      await expect.poll(async () => {
        await page.keyboard.press('ArrowRight');
        return region.evaluate(element => element.scrollLeft);
      }).toBeGreaterThan(0);
      await expect(region).toBeFocused();
      await region.evaluate(element => { element.scrollLeft = 0; });
    }

    const id = region.locator('.mono').filter({ hasText: /^[0-9a-f]{64}$/ }).first();
    const text = (await id.textContent())!;
    expect(text).toMatch(/^[0-9a-f]{64}$/);
    const idBox = (await id.boundingBox())!;
    const cell = (await id.locator('xpath=ancestor::td[1]').boundingBox())!;
    expect(idBox.x + idBox.width, 'the ID stays inside its cell').toBeLessThanOrEqual(cell.x + cell.width + 0.5);
    if (width < 641) {
      const line = parseFloat(await id.evaluate(element => getComputedStyle(element).lineHeight)) || 20;
      expect(idBox.height, 'the ID wraps onto more than one line').toBeGreaterThan(line * 1.5);
      expect(idBox.width, 'the wrapped ID is narrower than the phone').toBeLessThan(width - 32);
    }
    // Soft wraps add no characters: selecting the ID copies exactly the 64 hex digits.
    expect(await id.evaluate(element => {
      const selection = getSelection()!;
      const range = document.createRange();
      range.selectNodeContents(element);
      selection.removeAllRanges();
      selection.addRange(range);
      return selection.toString();
    })).toBe(text);
  });
}
