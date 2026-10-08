import { execFileSync } from 'node:child_process';
import { randomBytes } from 'node:crypto';
import { expect, test, type Page } from '@playwright/test';
import { uniqueHandle } from './db-fixtures';
import { expectNotice, signedIn, submitStatus, withinViewport } from './db-helpers';
import { waitForStyles } from './styles';

// A-445 (P4-R9, ARCH-12) and A-486: the pooled wallet's drain panel and the hand settlement of a blocked payout (hold,
// then record), with JavaScript disabled at 320 px and 1280 px. No wallet runs here, so the blockers are seeded in SQL: an open pooled order and a
// completed one whose release is blocked (its vendor saved no payout address) and whose deposit address is still
// watched. The panel's counts are compared with pooled_drain_blockers() itself, since other specs leave fixtures in this
// database. A fresh account is made administrator for this test only; everything seeded is removed at the end
// (db-payments.spec.ts expects no payouts).

const kinds = ['open_order', 'unsent_payout', 'flagged_payment', 'watched_address', 'recovery_required', 'address_collision', 'mainnet_history'];

async function fits(page: Page, width: number, label: string) {
  await waitForStyles(page);
  expect(await page.evaluate(() => document.documentElement.scrollWidth), `${label} at ${width}px`).toBeLessThanOrEqual(width);
}

for (const width of [320, 1280]) {
  test(`drain panel and hand settlement of a blocked payout at ${width} px`, async ({ browser, baseURL }) => {
    const database = process.env.E2E_DATABASE_URL!;
    const sql = (query: string) => execFileSync('psql', [database, '-v', 'ON_ERROR_STOP=1', '-qtAc', query], { encoding: 'utf8' }).trim();
    const hex = (bytes: number) => randomBytes(bytes).toString('hex');
    const adminPassword = 'browser-drain-admin-password-123';
    const adminHandle = uniqueHandle(`drain_admin_${width}`);
    const vendor = uniqueHandle('drain_recipient_long_handle');
    const [buyerID, vendorID, productID, openOrder, settledOrder] = [hex(16), hex(16), hex(16), hex(32), hex(32)];
    const address = `tb1qdrain${hex(20)}`;
    const admin = await signedIn(browser, baseURL, adminHandle, adminPassword, true, width);
    const other = await signedIn(browser, baseURL, uniqueHandle('drain_buyer'), 'browser-drain-buyer-123', true, width);
    try {
      sql(`UPDATE users SET role='admin' WHERE handle='${adminHandle}';
        INSERT INTO users(id,handle,password_hash,role) VALUES ('${buyerID}','${uniqueHandle('drain_payer')}','!','buyer'),('${vendorID}','${vendor}','!','vendor');
        INSERT INTO products(id,vendor_id,title,description,category,region,kind,btc,xmr,stock,archived)
          VALUES ('${productID}','${vendorID}','Drain fixture','Seeded by db-pooled-drain.spec.ts','Other','Worldwide','physical',100000,500000000000,0,true);
        INSERT INTO orders(id,buyer_id,product_id,currency,amount,state)
          VALUES ('${openOrder}','${buyerID}','${productID}','BTC',100000,'paid'),('${settledOrder}','${buyerID}','${productID}','BTC',100000,'completed');
        INSERT INTO payment_addresses(order_id,currency,address,provider) VALUES ('${settledOrder}','BTC','${address}','btc-regtest');
        INSERT INTO payouts(order_id,kind,user_id,currency,amount,address,state) VALUES ('${settledOrder}','release','${vendorID}','BTC',100000,'','blocked');`);
      const payout = sql(`SELECT id FROM payouts WHERE order_id='${settledOrder}'`);
      const state = () => sql(`SELECT state||':'||txid FROM payouts WHERE id=${payout}`);

      // Denied: a buyer gets no admin page and no panel, and a hand settlement posted anyway is refused (403).
      const denied = await other.goto('/admin');
      expect([403, 404]).toContain(denied!.status());
      await expect(other.locator('#pooled-drain')).toHaveCount(0);
      await other.goto('/account');
      const csrf = await other.locator('input[name="csrf"]').first().getAttribute('value');
      const refused = await other.request.post('/admin/payout', { form: { csrf: csrf!, payout_id: payout, op: 'sent', txid: 'ab'.repeat(32), password: 'browser-drain-buyer-123' }, maxRedirects: 0 });
      expect(refused.status()).toBe(403);
      expect(state()).toBe('blocked:');

      // The panel: each kind's count is the function's, the payout links to its resolve form, the balance is advice.
      await admin.goto('/admin#pooled-drain');
      await admin.waitForLoadState('load');
      const counts = Object.fromEntries(sql(`SELECT kind||'='||count(*) FROM pooled_drain_blockers() GROUP BY kind`).split('\n').filter(Boolean).map((l) => l.split('=')));
      for (const kind of kinds) {
        await expect(admin.locator(`#pooled-drain-${kind} .pooled-drain-count`)).toHaveText(String(counts[kind] ?? 0));
      }
      const total = kinds.reduce((n, k) => n + Number(counts[k] ?? 0), 0);
      await expect(admin.locator('#pooled-drain-total strong')).toHaveText(String(total));
      await expect(admin.locator('#pooled-drain-advice')).toContainText('The list decides, not the balance.');
      await expect(admin.locator('#pooled-drain-balance')).toContainText('BTC pooled wallet: no pooled wallet is configured.');
      await expect(admin.locator('#pooled-drain-sweep')).toContainText('The market never sweeps the pooled wallet. Once this list is empty, move any balance left in it yourself');
      if (Number(counts.watched_address) <= 100) await expect(admin.locator('#pooled-drain-watched_address')).toContainText(address);
      await expect(admin.locator('form[action^="/admin/pooled"]')).toHaveCount(0); // read-only
      await fits(admin, width, 'drain panel');

      const link = admin.locator('#pooled-drain-unsent_payout').getByRole('link', { name: `Payout ${payout}`, exact: true });
      await link.click();
      await expect(admin).toHaveURL(new RegExp(`/admin#resolve-payout-${payout}$`));
      const details = admin.locator(`details.payout-actions#resolve-payout-${payout}`);
      await expect(details, 'the blocked payout has a resolve form').toHaveCount(1);
      if ((await details.getAttribute('open')) === null) await details.locator('summary').click();
      await expect(details).toContainText('The recipient has saved no BTC payout address');
      await expect(details).toContainText('first hold it here, then pay it from the pooled wallet');
      await expect(details).toContainText('The market does not check the transaction ID against the chain');
      await withinViewport(details, width);

      // A-486 (R2): hold first, with a staff reason and the password; the order history says so, the reason stays staff only.
      const holdForm = details.locator('form', { has: admin.getByRole('button', { name: 'Hold to pay by hand' }) });
      const reason = 'Recipient gave an address by PGP mail; paying by hand.';
      await holdForm.getByLabel('Reason for holding it (staff only)').fill(reason);
      await holdForm.getByLabel('Current password', { exact: true }).fill(adminPassword);
      await holdForm.getByRole('button', { name: 'Hold to pay by hand' }).click();
      await expectNotice(admin, 'Payout held to be paid by hand.');
      expect(state()).toBe('held:');
      expect(sql(`SELECT error FROM payouts WHERE id=${payout}`)).toBe(`Held to be paid by hand: ${reason}`);
      expect(sql(`SELECT count(*) FROM order_events WHERE order_id='${settledOrder}' AND note LIKE '%held by an administrator to be paid by hand%' AND note NOT LIKE '%PGP%'`)).toBe('1');
      await admin.goto(`/admin#resolve-payout-${payout}`);
      await admin.waitForLoadState('load');
      await expect(admin.locator('.admin-payments tbody tr', { has: admin.getByRole('link', { name: `Resolve payout ${payout}`, exact: true }) }))
        .toContainText('Held to be paid by hand — record it sent once paid, or release it');
      if ((await details.getAttribute('open')) === null) await details.locator('summary').click();
      await expect(details).toContainText('An administrator held this payout to pay it by hand.');
      await expect(details.getByLabel('Nobody paid this payout by hand: I checked the pooled wallet')).toBeVisible();
      await withinViewport(details, width);
      await fits(admin, width, 'admin with a payout held to be paid by hand');

      // A stale copy of the form, loaded before the record, for the repeat after it.
      const stale = await admin.context().newPage();
      await stale.goto(`/admin#resolve-payout-${payout}`);
      await stale.waitForLoadState('load');

      // A bad transaction ID: the browser's own check stops a non-hex one, and the server refuses one sent anyway (400).
      const sentForm = details.locator('form', { has: admin.getByRole('button', { name: 'Record sent by hand' }) });
      const txid = sentForm.getByLabel('Wallet transaction ID');
      await txid.fill('g'.repeat(64));
      await sentForm.getByLabel('Current password', { exact: true }).fill(adminPassword);
      expect(await txid.evaluate((e: HTMLInputElement) => e.validity.patternMismatch)).toBe(true);
      const adminCSRF = await sentForm.locator('input[name="csrf"]').getAttribute('value');
      const bad = await admin.request.post('/admin/payout', { form: { csrf: adminCSRF!, payout_id: payout, op: 'sent', txid: 'g'.repeat(64), password: adminPassword }, maxRedirects: 0 });
      expect(bad.status()).toBe(400);
      expect(await bad.text()).toContain('64-character hexadecimal transaction ID');
      expect(state()).toBe('held:');

      // Success: an upper-case ID is stored in lower case, audited with the payout and the ID.
      const id = 'AB'.repeat(32);
      await txid.fill(id);
      await sentForm.getByLabel('Current password', { exact: true }).fill(adminPassword);
      await sentForm.getByRole('button', { name: 'Record sent by hand' }).click();
      await expectNotice(admin, 'Payout marked sent.');
      expect(state()).toBe(`sent:${id.toLowerCase()}`);
      expect(sql(`SELECT count(*) FROM audit_events a JOIN users u ON u.id=a.user_id WHERE u.handle='${adminHandle}' AND a.action LIKE 'Marked payout ${payout} %sent with transaction ${id.toLowerCase()}%by hand%'`)).toBe('1');
      await admin.reload();
      await admin.waitForLoadState('load');
      await expect(admin.locator(`#resolve-payout-${payout}`)).toHaveCount(0);
      await expect(admin.locator('#pooled-drain-unsent_payout').getByRole('link', { name: `Payout ${payout}`, exact: true })).toHaveCount(0);
      await fits(admin, width, 'admin after the hand settlement');

      // The repeat from the stale form answers 409, says the payout is already recorded as sent (a hand payment would
      // be a second one), and changes nothing.
      const staleDetails = stale.locator(`details.payout-actions#resolve-payout-${payout}`);
      if ((await staleDetails.getAttribute('open')) === null) await staleDetails.locator('summary').click();
      const staleSent = staleDetails.locator('form', { has: stale.getByRole('button', { name: 'Record sent by hand' }) });
      await staleSent.getByLabel('Wallet transaction ID').fill('cd'.repeat(32));
      await staleSent.getByLabel('Current password', { exact: true }).fill(adminPassword);
      const status = await submitStatus(stale, () => staleSent.getByRole('button', { name: 'Record sent by hand' }).click());
      expect(status).toBe(409);
      await expect(stale.locator('body')).toContainText('already recorded as sent');
      await expect(stale.locator('body')).toContainText('a payment you made by hand would be a second payment');
      await expect(stale.locator('body')).toContainText('Nothing was changed');
      expect(state()).toBe(`sent:${id.toLowerCase()}`);
      expect(sql(`SELECT count(*) FROM audit_events WHERE action LIKE 'Marked payout ${payout} %'`)).toBe('1');
      await stale.close();
    } finally {
      sql(`DELETE FROM payouts WHERE order_id IN ('${openOrder}','${settledOrder}');
        DELETE FROM payment_addresses WHERE order_id IN ('${openOrder}','${settledOrder}');
        DELETE FROM order_events WHERE order_id IN ('${openOrder}','${settledOrder}');
        DELETE FROM notifications WHERE user_id IN ('${buyerID}','${vendorID}');
        DELETE FROM orders WHERE id IN ('${openOrder}','${settledOrder}');
        DELETE FROM products WHERE id='${productID}';
        DELETE FROM users WHERE id IN ('${buyerID}','${vendorID}');
        DELETE FROM sessions WHERE user_id=(SELECT id FROM users WHERE handle='${adminHandle}');
        UPDATE users SET role='buyer' WHERE handle='${adminHandle}';`);
      await admin.context().close();
      await other.context().close();
    }
  });
}
