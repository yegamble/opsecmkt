import { execFileSync } from 'node:child_process';
import { randomBytes } from 'node:crypto';
import { expect, test } from '@playwright/test';
import { uniqueHandle } from './db-fixtures';
import { signedIn } from './db-helpers';
import { waitForStyles } from './styles';

// Seeded, fully drained history after migration107 tests readers and removed routes.
// The actual pre-migration refusal and atomic rollback are PostgreSQL Go/upgrade checks.
for (const width of [320, 1280]) {
  test(`pooled history remains readable and immutable at ${width}px`, async ({ browser, baseURL }) => {
    const database = process.env.E2E_DATABASE_URL!;
    const sql = (query: string) => execFileSync('psql', [database, '-v', 'ON_ERROR_STOP=1', '-qtAc', query], { encoding: 'utf8' }).trim();
    const hex = (bytes: number) => randomBytes(bytes).toString('hex');
    const adminHandle = uniqueHandle('history_admin'), buyerHandle = uniqueHandle('history_buyer'), vendorHandle = uniqueHandle('history_vendor');
    const password = 'browser-history-password-123';
    const admin = await signedIn(browser, baseURL, adminHandle, password, true, width);
    const buyer = await signedIn(browser, baseURL, buyerHandle, password, true, width);
    const vendor = await signedIn(browser, baseURL, vendorHandle, password, true, width);
    const product = hex(16), order = hex(32), txid = hex(32), payoutTxid = hex(32), address = `tb1qhistory${hex(20)}`, payoutAddress = `tb1qpayout${hex(20)}`;
    const fingerprint = () => sql(`SELECT md5(row_to_json(o)::text||row_to_json(pa)::text||row_to_json(pm)::text||row_to_json(p)::text)
      FROM orders o JOIN payment_addresses pa ON pa.order_id=o.id JOIN payments pm ON pm.order_id=o.id JOIN payouts p ON p.order_id=o.id WHERE o.id='${order}'`);
    try {
      sql(`UPDATE users SET role='admin' WHERE handle='${adminHandle}';
        UPDATE users SET role='vendor',payout_btc='${payoutAddress}' WHERE handle='${vendorHandle}';
        INSERT INTO products(id,vendor_id,title,description,category,region,kind,btc,xmr,stock,archived)
          SELECT '${product}',id,'Historical pooled order','Seeded legacy history','Other','Worldwide','physical',100000,500000000000,0,true FROM users WHERE handle='${vendorHandle}';
        INSERT INTO orders(id,buyer_id,product_id,currency,amount,state,updated)
          SELECT '${order}',id,'${product}','BTC',100000,'completed',now()-interval '40 days' FROM users WHERE handle='${buyerHandle}';
        INSERT INTO payment_addresses(order_id,currency,address,provider,created) VALUES('${order}','BTC','${address}','btc-regtest',now()-interval '40 days');
        INSERT INTO payments(order_id,currency,txid,idx,address,amount,confirmations,credited) VALUES('${order}','BTC','${txid}',0,'${address}',100000,3,true);
        INSERT INTO payouts(order_id,kind,user_id,currency,amount,address,state,txid)
          SELECT '${order}','release',id,'BTC',100000,'${payoutAddress}','sent','${payoutTxid}' FROM users WHERE handle='${vendorHandle}';`);
      const before = fingerprint();
      await buyer.goto(`/order?id=${order}`);
      await expect(buyer.getByRole('heading', { name: 'Historical payment record' })).toBeVisible();
      await expect(buyer.locator('main')).toContainText(txid);
      await expect(buyer.locator('main')).toContainText('do not send funds to an old address');
      await expect(buyer.locator('form[action="/orders/pay"]')).toHaveCount(0);
      await vendor.goto(`/order?id=${order}`);
      await expect(vendor.getByRole('heading', { name: 'Historical payment record' })).toBeVisible();
      await expect(vendor.locator('main')).not.toContainText(txid);
      await expect(vendor.locator('main')).not.toContainText(address);
      await vendor.goto('/account');
      await expect(vendor.locator('#payout')).toContainText(payoutAddress);
      await expect(vendor.locator('form[action="/account/payout"]')).toHaveCount(0);
      await admin.goto('/admin');
      await expect(admin.locator('.admin-payments')).toContainText('Historical pooled payouts');
      await expect(admin.locator('.admin-payments')).toContainText(payoutTxid);
      await expect(admin.locator('form[action="/admin/payout"]')).toHaveCount(0);
      const csrf = await admin.locator('input[name="csrf"]').first().inputValue();
      const refused = await admin.request.post('/admin/payout', { form: { csrf, payout_id: sql(`SELECT id FROM payouts WHERE order_id='${order}'`), op: 'requeue', password }, maxRedirects: 0 });
      expect(refused.status()).toBe(404);
      expect(fingerprint()).toBe(before);
      for (const page of [buyer, vendor, admin]) {
        await waitForStyles(page);
        expect(await page.evaluate(() => document.documentElement.scrollWidth)).toBeLessThanOrEqual(width);
        await expect(page.locator('script')).toHaveCount(0);
      }
    } finally {
      sql(`DELETE FROM payouts WHERE order_id='${order}'; DELETE FROM payments WHERE order_id='${order}';
        DELETE FROM payment_addresses WHERE order_id='${order}'; DELETE FROM orders WHERE id='${order}'; DELETE FROM products WHERE id='${product}';
        UPDATE users SET role='buyer',payout_btc='' WHERE handle IN ('${adminHandle}','${vendorHandle}');`);
      await Promise.all([admin.context().close(), buyer.context().close(), vendor.context().close()]);
    }
  });
}
