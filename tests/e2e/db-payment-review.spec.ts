import { execFileSync } from 'node:child_process';
import { randomBytes } from 'node:crypto';
import { expect, test } from '@playwright/test';
import { uniqueHandle } from './db-fixtures';
import { seedRecordedMoneroEscrow, signedIn, submitAndLoad, withinViewport } from './db-helpers';

// A-253 (A-31): the moderator's payment-review disposition forms, submitted with JavaScript disabled against a real
// database. No wallet runs here, so the flags are seeded in SQL: two escrow deposits
// the watcher would have flagged after the order closed. A moderator records no action, then escalates another
// flag after a forged hand-refund request is refused; an administrator records its final review. Both parties
// read the outcome after a reload, with no payout or refund transaction created. Every account, listing, order and address is unique to the run, and a flag this test leaves open
// is unflagged at the end, so the desk count stays right on a rerun against the same database.
test('a moderator reviews escrow flags at 320 px; a hand refund is refused and an administrator closes escalation', async ({ browser, baseURL }) => {
  const database = process.env.E2E_DATABASE_URL!;
  const sql = (query: string) => execFileSync('psql', [database, '-v', 'ON_ERROR_STOP=1', '-qtAc', query], { encoding: 'utf8' }).trim();
  const hex = (bytes: number) => randomBytes(bytes).toString('hex');
  const [modHandle, buyerHandle, vendorHandle, adminHandle] = [uniqueHandle('review_mod'), uniqueHandle('review_buyer'), uniqueHandle('review_vendor'), uniqueHandle('review_admin')];
  const password = 'browser-payment-review-password-123';
  const [productID, order, noActionTx, refundedTx, refundTx] = [hex(16), hex(32), hex(32), hex(32), hex(32)];
  // Unique per run (payment_addresses.address is unique); only bech32 characters.
  const address = `tb1q${hex(19).replace(/[1b]/g, 'q')}`;
  const reason = 'The buyer confirmed this was a test transfer to keep.';
  const mod = await signedIn(browser, baseURL, modHandle, password, true, 320);
  const buyer = await signedIn(browser, baseURL, buyerHandle, password, true);
  const vendor = await signedIn(browser, baseURL, vendorHandle, password, true);
  const admin = await signedIn(browser, baseURL, adminHandle, password, true, 320);
  try {
    sql(`UPDATE users SET role='moderator',pgp_2fa=true WHERE handle='${modHandle}'; UPDATE users SET role='vendor' WHERE handle='${vendorHandle}'; UPDATE users SET role='admin',pgp_2fa=true WHERE handle='${adminHandle}';
      INSERT INTO products(id,vendor_id,title,description,category,region,kind,btc,xmr,stock,archived)
        VALUES ('${productID}',(SELECT id FROM users WHERE handle='${vendorHandle}'),'Payment review fixture','Seeded by db-payment-review.spec.ts','Other','Worldwide','digital',100000,500000000000,0,true);
      INSERT INTO orders(id,buyer_id,product_id,currency,amount,state)
        VALUES ('${order}',(SELECT id FROM users WHERE handle='${buyerHandle}'),'${productID}','XMR',100000,'cancelled');
      INSERT INTO payments(order_id,currency,txid,idx,address,amount,confirmations,flagged,after_close) VALUES
        ('${order}','XMR','${noActionTx}',0,'${address}',20000,3,true,true),('${order}','XMR','${refundedTx}',0,'${address}',30000,3,true,true);`);

    seedRecordedMoneroEscrow(sql, order);

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
      await expect(mod).toHaveURL(new RegExp(`/moderator#payment-review-${id}$`));
      await expect(details.locator(`input[name="payment_id"][value="${id}"]`)).toHaveCount(2);
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
    await expect(mod).toHaveURL(/\/moderator#payment-review$/);
    await expect(desk.getByRole('status')).toHaveText('Payment review recorded.');
    await expect(openCount).toHaveText('1 open flag');
    await expect(desk.locator('.payment-review-row', { hasText: `${noActionTx}:0` })).toHaveCount(0);

    const escalatedReview = await openReview(refundedTx);
    await expect(escalatedReview.getByRole('button', { name: 'Record: refunded by hand' })).toHaveCount(0);
    const values = Object.fromEntries(await escalatedReview.locator('form').first().locator('input[type="hidden"]').evaluateAll(inputs =>
      inputs.map(input => [(input as HTMLInputElement).name, (input as HTMLInputElement).value])));
    const forgedRefund = await mod.request.post('/moderator/payment-review', {
      form: { ...values, outcome: 'refunded', txid: refundTx, amount: '0.0003', password }, maxRedirects: 0,
    });
    expect(forgedRefund.status()).toBe(409);
    expect(await forgedRefund.text()).toContain('never refunded by hand');
    expect(sql(`SELECT disposition FROM payments WHERE order_id='${order}' AND txid='${refundedTx}'`)).toBe('');

    const escalate = escalatedReview.locator('form', { has: mod.getByRole('button', { name: 'Escalate to an administrator' }) });
    await escalate.getByLabel('Current password').fill(password);
    await submitAndLoad(mod, escalate.getByRole('button', { name: 'Escalate to an administrator' }));
    await expect(desk).toContainText('An administrator records the outcome.');
    await expect(desk.locator('.payment-review-form')).toHaveCount(0);
    expect(sql(`SELECT disposition FROM payments WHERE order_id='${order}' AND txid='${refundedTx}'`)).toBe('escalated');

    await admin.goto('/moderator');
    const id = sql(`SELECT id FROM payments WHERE order_id='${order}' AND txid='${refundedTx}'`);
    const adminReview = admin.locator(`details.payment-review-form#payment-review-${id}`);
    await adminReview.locator('summary').click();
    await withinViewport(adminReview, 320);
    const finalReason = 'The stray refund remains the responsibility of this escrow.';
    await adminReview.getByLabel('Reason for taking no action').fill(finalReason);
    await adminReview.getByLabel('Current password').fill(password);
    await submitAndLoad(admin, adminReview.getByRole('button', { name: 'Record: no action' }));
    await expect(admin.getByRole('region', { name: 'Payment review' })).toContainText('No payments are flagged for review.');
    expect(sql(`SELECT string_agg(disposition, ',' ORDER BY txid='${refundedTx}') FROM payments WHERE order_id='${order}'`)).toBe('no_action,no_action');
    expect(sql(`SELECT count(*) FROM payouts WHERE order_id='${order}'`)).toBe('0');

    for (const party of [buyer, vendor]) {
      await party.goto(`/order?id=${order}`);
      await party.reload();
      await expect(party.locator('main')).toContainText(`no action. Reason: ${reason}`);
      await expect(party.locator('main')).toContainText(`no action. Reason: ${finalReason}`);
      await expect(party.locator('main')).not.toContainText(refundTx);
    }
  } finally {
    // A failed run leaves no open flag on the shared moderation desk.
    sql(`UPDATE payments SET flagged=false WHERE order_id='${order}' AND disposition=''`);
    sql(`UPDATE users SET role='buyer' WHERE handle='${adminHandle}'`);
    for (const page of [mod, buyer, vendor, admin]) await page.context().close();
  }
});
