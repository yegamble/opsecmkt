import { execFileSync } from 'node:child_process';
import { randomBytes } from 'node:crypto';
import { expect, test } from '@playwright/test';
import { uniqueHandle } from './db-fixtures';
import { signedIn, submitAndLoad, withinViewport } from './db-helpers';

// A-480: a Monero escrow deposit refused as another order's escrow account, recorded at 320 px with JavaScript disabled
// against a real database. No wallet runs here, so the record the escrow watcher writes (xmr_escrow_collisions) is
// seeded in SQL, as db-payment-review.spec.ts seeds flags. A fresh moderator who is not a party opens the order from the
// moderation desk's Escrow alerts, reads the record, and records what was done with the order page's form; the alert
// clears, the order is no longer readable to them after a reload, and the buyer never sees the record. Every account,
// listing and order is unique to the run; a record this test leaves open is closed at the end.
test('a moderator records what was done about a refused Monero deposit at 320 px; the alert clears', async ({ browser, baseURL }) => {
  const database = process.env.E2E_DATABASE_URL!;
  const sql = (query: string) => execFileSync('psql', [database, '-v', 'ON_ERROR_STOP=1', '-qtAc', query], { encoding: 'utf8' }).trim();
  const hex = (bytes: number) => randomBytes(bytes).toString('hex');
  const [modHandle, buyerHandle, vendorHandle] = [uniqueHandle('refused_mod'), uniqueHandle('refused_buyer'), uniqueHandle('refused_vendor')];
  const password = 'browser-refused-deposit-password-123';
  const [productID, order, txid] = [hex(16), hex(32), hex(32)];
  const reason = `Checked the escrow wallet: refunded to the buyer by hand in ${hex(32)}.`;
  const mod = await signedIn(browser, baseURL, modHandle, password, true, 320);
  const buyer = await signedIn(browser, baseURL, buyerHandle, password, true);
  try {
    sql(`UPDATE users SET role='moderator',pgp_2fa=true WHERE handle='${modHandle}';
      INSERT INTO users(id,handle,password_hash,role) VALUES ('${hex(16)}','${vendorHandle}','x','vendor');
      INSERT INTO products(id,vendor_id,title,description,category,region,kind,btc,xmr,stock,archived)
        VALUES ('${productID}',(SELECT id FROM users WHERE handle='${vendorHandle}'),'Refused deposit fixture','Seeded by db-xmr-collision.spec.ts','Other','Worldwide','digital',100000,500000000000,0,true);
      INSERT INTO orders(id,buyer_id,product_id,currency,amount,state)
        VALUES ('${order}',(SELECT id FROM users WHERE handle='${buyerHandle}'),'${productID}','XMR',500000000000,'awaiting_payment');
      INSERT INTO xmr_escrow_collisions(order_id,wallet_name,account,txid) VALUES ('${order}','escrow-202610-01',7,'${txid}');`);

    await mod.goto('/moderator');
    const alerts = mod.getByRole('region', { name: 'Escrow alerts' });
    const entry = alerts.locator('li', { hasText: order });
    await expect(entry).toContainText("a Monero deposit was refused as another order's escrow account");
    await submitAndLoad(mod, entry.getByRole('link', { name: order }));
    await expect(mod).toHaveURL(new RegExp(`/order\\?id=${order}$`));
    await expect(mod.locator('main')).toContainText('because its escrow has an open staff alert');
    const section = mod.getByRole('region', { name: 'Refused Monero deposits' });
    await expect(section).toContainText(txid);
    await expect(section).toContainText('escrow-202610-01');
    await expect(section).toContainText('Open');
    await withinViewport(section, 320);
    expect(await mod.evaluate(() => document.documentElement.scrollWidth), '/order at 320 px with the refused-deposit form').toBeLessThanOrEqual(320);

    const form = section.locator('form');
    await form.getByLabel('What was done').fill(reason);
    await form.getByLabel('Current password').fill(password);
    await submitAndLoad(mod, form.getByRole('button', { name: 'Record what was done' }));
    await expect(mod).toHaveURL(/\/moderator$/);
    await expect(mod.getByRole('status')).toHaveText('Recorded what was done about the refused Monero deposit.');
    await expect(mod.locator(`a[href="/order?id=${order}"]`)).toHaveCount(0);
    expect(sql(`SELECT disposition_note FROM xmr_escrow_collisions WHERE order_id='${order}'`)).toBe(reason);

    // After a reload the order is the parties' alone again: the moderator is refused it.
    await mod.reload();
    const refused = await mod.goto(`/order?id=${order}`);
    expect(refused?.status()).toBe(404);

    // The buyer reads the order as before, with no record of the refused deposit or its disposition.
    await buyer.goto(`/order?id=${order}`);
    await buyer.reload();
    await expect(buyer.getByRole('region', { name: 'Refused Monero deposits' })).toHaveCount(0);
    await expect(buyer.locator('body')).not.toContainText(reason);
  } finally {
    // A failed run leaves no open alert on the shared moderation desk.
    sql(`UPDATE xmr_escrow_collisions SET disposition_note='Closed by db-xmr-collision.spec.ts cleanup.',disposed_at=now(),
      disposed_by=(SELECT id FROM users WHERE handle='${modHandle}') WHERE order_id='${order}' AND disposed_at IS NULL`);
    for (const page of [mod, buyer]) await page.context().close();
  }
});
