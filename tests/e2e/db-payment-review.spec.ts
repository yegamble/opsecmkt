import { execFileSync } from 'node:child_process';
import { randomBytes } from 'node:crypto';
import { expect, test, type Locator } from '@playwright/test';
import { uniqueHandle } from './db-fixtures';
import { signedIn, submitAndLoad } from './db-helpers';

// A-253 (A-31): the moderator's payment-review disposition forms, submitted with JavaScript disabled against a real
// database. No wallet runs here, so the flags are seeded in SQL (as db-admin.spec.ts seeds payouts): two deposits
// the watcher would have flagged after the order closed. A fresh moderator records "no action" and then "refunded by
// hand" at 320 px; both parties read the outcome in the order history after a reload, the refund's transaction ID
// only the buyer. Every account, listing, order and address is unique to the run, and a flag this test leaves open
// is unflagged at the end, so the desk count stays right on a rerun against the same database.
test('a moderator records payment reviews at 320 px; buyer and vendor read the outcome after reload', async ({ browser, baseURL }) => {
  const database = process.env.E2E_DATABASE_URL!;
  const sql = (query: string) => execFileSync('psql', [database, '-v', 'ON_ERROR_STOP=1', '-qtAc', query], { encoding: 'utf8' }).trim();
  const hex = (bytes: number) => randomBytes(bytes).toString('hex');
  const [modHandle, buyerHandle, vendorHandle] = [uniqueHandle('review_mod'), uniqueHandle('review_buyer'), uniqueHandle('review_vendor')];
  const password = 'browser-payment-review-password-123';
  const [productID, order, noActionTx, refundedTx, refundTx] = [hex(16), hex(32), hex(32), hex(32), hex(32)];
  // Unique per run (payment_addresses.address is unique); only bech32 characters.
  const address = `tb1q${hex(19).replace(/[1b]/g, 'q')}`;
  const reason = 'The buyer confirmed this was a test transfer to keep.';
  const mod = await signedIn(browser, baseURL, modHandle, password, true, 320);
  const buyer = await signedIn(browser, baseURL, buyerHandle, password, true);
  const vendor = await signedIn(browser, baseURL, vendorHandle, password, true);
  try {
    sql(`UPDATE users SET role='moderator' WHERE handle='${modHandle}'; UPDATE users SET role='vendor' WHERE handle='${vendorHandle}';
      INSERT INTO products(id,vendor_id,title,description,category,region,kind,btc,xmr,stock,archived)
        VALUES ('${productID}',(SELECT id FROM users WHERE handle='${vendorHandle}'),'Payment review fixture','Seeded by db-payment-review.spec.ts','Other','Worldwide','digital',100000,500000000000,0,true);
      INSERT INTO orders(id,buyer_id,product_id,currency,amount,state)
        VALUES ('${order}',(SELECT id FROM users WHERE handle='${buyerHandle}'),'${productID}','BTC',100000,'cancelled');
      INSERT INTO payment_addresses(order_id,currency,address,provider) VALUES ('${order}','BTC','${address}','bitcoin');
      INSERT INTO payments(order_id,currency,txid,idx,address,amount,confirmations,flagged,after_close) VALUES
        ('${order}','BTC','${noActionTx}',0,'${address}',20000,3,true,true),('${order}','BTC','${refundedTx}',0,'${address}',30000,3,true,true);`);

    await mod.goto('/moderator');
    const desk = mod.getByRole('region', { name: 'Payment review' });
    const openCount = desk.locator('p > strong');
    await expect(openCount).toHaveText('2 open flags');
    // A-275: a flag's review forms are not in its table row (a cell widened the scrolling table to ~1560 px at
    // 320 px): the row's "Record the review" link goes to its <details> under the table. After the stylesheet
    // applies (A-228), the open form, its labels, fields and submit buttons lie within the 320 px viewport.
    await mod.waitForLoadState('load');
    await expect(desk.locator('.table-wrap')).toHaveCSS('overflow-x', 'auto');
    const openReview = async (tx: string) => {
      const id = sql(`SELECT id FROM payments WHERE order_id='${order}' AND txid='${tx}'`);
      const details = desk.locator(`details.payment-review-form#payment-review-${id}`);
      await desk.locator('.payment-review-row', { hasText: `${tx}:0` }).getByRole('link', { name: 'Record the review' }).click();
      await expect(mod).toHaveURL(new RegExp(`/moderator(\\?saved=1)?#payment-review-${id}$`));
      await expect(details.locator(`input[name="payment_id"][value="${id}"]`)).toHaveCount(3);
      const summary = details.locator('summary');
      await expect(summary).toHaveText(`Record the review of ${tx}:0`);
      await summary.click();
      await expect(details).toHaveAttribute('open', '');
      await withinViewport(details, 320);
      expect(await mod.evaluate(() => document.documentElement.scrollWidth), '/moderator at 320 px with the review form open').toBeLessThanOrEqual(320);
      return details;
    };
    const noActionReview = await openReview(noActionTx);
    const noAction = noActionReview.locator('form', { has: mod.getByRole('button', { name: 'Record: no action' }) });
    await noAction.getByLabel('Reason for taking no action').fill(reason);
    await noAction.getByLabel('Current password').fill(password);
    await submitAndLoad(mod, noAction.getByRole('button', { name: 'Record: no action' }));
    await expect(mod).toHaveURL(/\/moderator\?saved=1#payment-review$/);
    await expect(openCount).toHaveText('1 open flag');
    await expect(desk.locator('.payment-review-row', { hasText: `${noActionTx}:0` })).toHaveCount(0);

    const refundedReview = await openReview(refundedTx);
    const refundForm = refundedReview.locator('form', { has: mod.getByRole('button', { name: 'Record: refunded by hand' }) });
    await refundForm.getByLabel('Refund transaction ID').fill(refundTx);
    await refundForm.getByLabel('Amount refunded (BTC, at most 0.0003)').fill('0.0003');
    await refundForm.getByLabel('Current password').fill(password);
    await submitAndLoad(mod, refundForm.getByRole('button', { name: 'Record: refunded by hand' }));
    await expect(mod).toHaveURL(/\/moderator\?saved=1#payment-review$/);
    await expect(desk).toContainText('No payments are flagged for review.');
    await expect(desk.locator('.payment-review-row')).toHaveCount(0);
    // Nothing is sent: a hand refund writes no payouts row.
    expect(sql(`SELECT string_agg(disposition, ',' ORDER BY txid='${refundedTx}') FROM payments WHERE order_id='${order}'`)).toBe('no_action,refunded');
    expect(sql(`SELECT count(*) FROM payouts WHERE order_id='${order}'`)).toBe('0');

    // No wallet is configured here, so the order page lists no deposits; both parties read the outcome in the order
    // history, the refund's transaction ID only the buyer (the vendor sees it replaced).
    const refunded = "refunded to the buyer by hand, outside the market's automatic payouts: 0.0003 BTC, transaction";
    await buyer.goto(`/order?id=${order}`);
    await buyer.reload();
    const buyerPage = buyer.locator('main');
    await expect(buyerPage).toContainText(`no action. Reason: ${reason}`);
    await expect(buyerPage).toContainText(`${refunded} ${refundTx}.`);
    await vendor.goto(`/order?id=${order}`);
    await vendor.reload();
    const vendorPage = vendor.locator('main');
    await expect(vendorPage).toContainText(`no action. Reason: ${reason}`);
    await expect(vendorPage).toContainText(`${refunded} (transaction ID shown only to its recipient).`);
    await expect(vendor.locator('body')).not.toContainText(refundTx);
  } finally {
    // A failed run leaves no open flag on the shared moderation desk.
    sql(`UPDATE payments SET flagged=false WHERE order_id='${order}' AND disposition=''`);
    for (const page of [mod, buyer, vendor]) await page.context().close();
  }
});

// A-275: an open review <details>, each of its forms, labels, fields and submit buttons lie horizontally within the
// viewport: a form clipped inside the table's scrolling .table-wrap does not widen the document, so scrollWidth alone
// cannot see it.
async function withinViewport(review: Locator, width: number) {
  const parts = review.locator('summary, form, label, input:not([type="hidden"]), textarea, button');
  expect(await parts.count()).toBeGreaterThan(4);
  const outside: string[] = [];
  for (const part of await parts.all()) {
    const box = (await part.boundingBox())!;
    if (box.x < 0 || box.x + box.width > width) outside.push(`${await part.evaluate(e => e.outerHTML.slice(0, 60))}: x ${Math.round(box.x)} to ${Math.round(box.x + box.width)}`);
  }
  expect(outside, `review form parts outside 0..${width} px`).toEqual([]);
}
