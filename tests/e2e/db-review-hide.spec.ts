import { execFileSync } from 'node:child_process';
import { randomBytes } from 'node:crypto';
import { expect, test } from '@playwright/test';
import { uniqueHandle } from './db-fixtures';
import { expectNotice, signedIn, submitAndLoad, submitStatus } from './db-helpers';

// A-125: an administrator hides a review from the listing and vendor pages with the form under it, and restores it.
// No wallet runs in this project, so the completed orders and their reviews are seeded in SQL (as in db-admin.spec.ts).
// A fresh account is made administrator for this test only, so the shared administrator's budgets are untouched.
test('administrator hides a review from the public pages and restores it', async ({ browser, baseURL }) => {
  const database = process.env.E2E_DATABASE_URL!;
  const sql = (query: string) => execFileSync('psql', [database, '-v', 'ON_ERROR_STOP=1', '-qtAc', query], { encoding: 'utf8' }).trim();
  const hex = (bytes: number) => randomBytes(bytes).toString('hex');
  const password = 'browser-review-admin-password-123';
  const adminHandle = uniqueHandle('review_admin');
  const vendor = uniqueHandle('reviewed_vendor');
  const buyer = uniqueHandle('reviewer');
  const doxx = `The seller is Jane Example of 12 Harbour Street ${hex(4)}`;
  const kept = 'Arrived as described.';
  const [buyerID, vendorID, productID, badOrder, goodOrder] = [hex(16), hex(16), hex(16), hex(32), hex(32)];
  const admin = await signedIn(browser, baseURL, adminHandle, password, true);
  const visitorContext = await browser.newContext({ baseURL, javaScriptEnabled: false, viewport: { width: 390, height: 844 } });
  const visitor = await visitorContext.newPage();
  try {
    sql(`UPDATE users SET role='admin' WHERE handle='${adminHandle}';
      INSERT INTO users(id,handle,password_hash,role) VALUES ('${buyerID}','${buyer}','!','buyer'),('${vendorID}','${vendor}','!','vendor');
      INSERT INTO products(id,vendor_id,title,description,category,region,kind,btc,xmr,stock)
        VALUES ('${productID}','${vendorID}','Review moderation fixture','Seeded by db-review-hide.spec.ts','Other','Worldwide','physical',100000,500000000000,1);
      INSERT INTO orders(id,buyer_id,product_id,currency,amount,state)
        VALUES ('${badOrder}','${buyerID}','${productID}','XMR',100000,'completed'),('${goodOrder}','${buyerID}','${productID}','XMR',100000,'completed');
      INSERT INTO reviews(id,order_id,product_id,buyer_id,rating,body)
        VALUES ('${hex(32)}','${badOrder}','${productID}','${buyerID}',1,'${doxx}'),('${hex(32)}','${goodOrder}','${productID}','${buyerID}',5,'${kept}');`);
    const productURL = `/product?id=${productID}`;
    const vendorURL = `/vendor?id=${vendorID}`;
    const reviews = visitor.getByRole('region', { name: 'Verified reviews' });
    await visitor.goto(productURL);
    await expect(reviews).toContainText(doxx);
    await expect(reviews.locator('.review-summary')).toHaveText('3.0 / 5 · 2 reviews');
    await expect(visitor.getByText('Hide this review')).toHaveCount(0);

    // The administrator opens the form under the review, confirms with their password and returns to the page.
    await admin.goto(productURL);
    const review = admin.locator('article.review', { hasText: doxx });
    // The summary names the review for screen readers (rating and month).
    await review.locator('summary', { hasText: 'Hide this review: rated 1 out of 5, ' }).click();
    await review.getByLabel('Current password').fill(password);
    for (const width of [390, 320]) {
      await admin.setViewportSize({ width, height: 900 });
      expect(await admin.evaluate(() => document.documentElement.scrollWidth), `open hide form at ${width}px`).toBeLessThanOrEqual(width);
    }
    await submitAndLoad(admin, review.getByRole('button', { name: 'Hide review' }));
    await expect(admin).toHaveURL(new RegExp(`/product\\?id=${productID}#reviews-title$`));
    await expect(admin.getByRole('region', { name: 'Verified reviews' }).getByRole('status')).toHaveText('Review hidden.');
    await admin.reload();
    const hidden = admin.locator('article.review', { hasText: doxx });
    await expect(hidden.locator('.badge')).toHaveText(['Completed order (test network)', 'Hidden by an administrator']);
    await expect(admin.getByText('1 hidden review is not shown to others or counted.')).toBeVisible();
    await expect(admin.locator('.review-summary')).toHaveText('5.0 / 5 · 1 review');

    // Gone publicly from both pages, rating included.
    for (const url of [productURL, vendorURL]) {
      await visitor.goto(url);
      const main = visitor.locator('main');
      await expect(main).toContainText(kept);
      await expect(main).not.toContainText(doxx);
      await expect(main).not.toContainText('Hidden by an administrator');
      await expect(main.locator('.review-summary')).toHaveText('5.0 / 5 · 1 review');
    }

    // Restored from the vendor page.
    await admin.goto(vendorURL);
    const restore = admin.locator('article.review', { hasText: doxx });
    await restore.locator('summary', { hasText: 'Restore this review: rated 1 out of 5, ' }).click();
    await restore.getByLabel('Current password').fill(password);
    await submitAndLoad(admin, restore.getByRole('button', { name: 'Restore review' }));
    await expect(admin).toHaveURL(new RegExp(`/vendor\\?id=${vendorID}#reviews-title$`));
    await expect(admin.getByRole('region', { name: 'Verified reviews for this vendor' }).getByRole('status')).toHaveText('Review restored.');
    await visitor.goto(vendorURL);
    await expect(visitor.locator('main')).toContainText(doxx);
    await expect(visitor.locator('main .review-summary')).toHaveText('3.0 / 5 · 2 reviews');

    expect(sql(`SELECT count(*) FROM notifications WHERE user_id='${buyerID}' AND body NOT LIKE '%http%'`)).toBe('2');
    expect(sql(`SELECT count(*) FROM audit_events WHERE action LIKE '%Jane Example%'`)).toBe('0');
    expect(sql(`SELECT count(*) FROM audit_events a JOIN users u ON u.id=a.user_id WHERE u.handle='${adminHandle}' AND a.action LIKE '% review of order ${badOrder.slice(0, 8)} (confirmed with password)'`)).toBe('2');
  } finally {
    // Leave no public fixture listing and no extra administrator for the other specs.
    sql(`UPDATE products SET archived=true,archived_at=now() WHERE id='${productID}';
      DELETE FROM sessions WHERE user_id=(SELECT id FROM users WHERE handle='${adminHandle}');
      UPDATE users SET role='buyer' WHERE handle='${adminHandle}';`);
    await Promise.allSettled([admin.context(), visitorContext].map(context => context.close()));
  }
});

// A-649: once a buyer is banned, none of their reviews count. The administrator bans the reviewer from
// /admin/users; the review leaves both public pages and the rating, labelled only for administrators, and lifting the
// ban brings it back. Nothing is written to the review itself.
test('a banned buyer\'s review leaves the public pages and rating until the ban is lifted', async ({ browser, baseURL }) => {
  const database = process.env.E2E_DATABASE_URL!;
  const sql = (query: string) => execFileSync('psql', [database, '-v', 'ON_ERROR_STOP=1', '-qtAc', query], { encoding: 'utf8' }).trim();
  const hex = (bytes: number) => randomBytes(bytes).toString('hex');
  const password = 'browser-review-ban-password-123';
  const adminHandle = uniqueHandle('review_ban_admin');
  const vendor = uniqueHandle('ban_rev_vendor');
  const banned = uniqueHandle('banned_reviewer');
  const other = uniqueHandle('kept_reviewer');
  const bad = `Never arrived ${hex(4)}`;
  const kept = 'Arrived as described.';
  const [bannedID, otherID, vendorID, productID, badOrder, goodOrder] = [hex(16), hex(16), hex(16), hex(16), hex(32), hex(32)];
  const admin = await signedIn(browser, baseURL, adminHandle, password, true);
  const visitorContext = await browser.newContext({ baseURL, javaScriptEnabled: false, viewport: { width: 390, height: 844 } });
  const visitor = await visitorContext.newPage();
  try {
    sql(`UPDATE users SET role='admin' WHERE handle='${adminHandle}';
      INSERT INTO users(id,handle,password_hash,role) VALUES ('${bannedID}','${banned}','!','buyer'),('${otherID}','${other}','!','buyer'),('${vendorID}','${vendor}','!','vendor');
      INSERT INTO products(id,vendor_id,title,description,category,region,kind,btc,xmr,stock)
        VALUES ('${productID}','${vendorID}','Banned reviewer fixture','Seeded by db-review-hide.spec.ts','Other','Worldwide','physical',100000,500000000000,1);
      INSERT INTO orders(id,buyer_id,product_id,currency,amount,state)
        VALUES ('${badOrder}','${bannedID}','${productID}','XMR',100000,'completed'),('${goodOrder}','${otherID}','${productID}','XMR',100000,'completed');
      INSERT INTO reviews(id,order_id,product_id,buyer_id,rating,body)
        VALUES ('${hex(32)}','${badOrder}','${productID}','${bannedID}',1,'${bad}'),('${hex(32)}','${goodOrder}','${productID}','${otherID}',5,'${kept}');`);
    const productURL = `/product?id=${productID}`;
    const vendorURL = `/vendor?id=${vendorID}`;
    await visitor.goto(productURL);
    await expect(visitor.locator('main')).toContainText(bad);
    await expect(visitor.locator('main .review-summary')).toHaveText('3.0 / 5 · 2 reviews');

    const panel = admin.getByRole('region', { name: 'Ban or unban an account' });
    const submitBan = async (action: 'Ban' | 'Unban', why: string) => {
      await admin.goto('/admin/users');
      await panel.getByLabel('Account handle').fill(banned);
      await panel.getByRole('radio', { name: new RegExp(`^${action}:`) }).check();
      await panel.getByLabel('Reason').fill(why);
      await panel.getByLabel('Current password').fill(password);
      return submitStatus(admin, () => panel.getByRole('button', { name: 'Ban or unban' }).click());
    };
    expect(await submitBan('Ban', 'Posted reviews for orders that never existed.')).toBe(303);
    await expectNotice(admin, 'Account banned.');

    // Gone publicly from both pages, rating included, with the policy line and no count of what was left out.
    for (const url of [productURL, vendorURL]) {
      await visitor.goto(url);
      const main = visitor.locator('main');
      await expect(main).toContainText(kept);
      await expect(main).not.toContainText(bad);
      await expect(main).not.toContainText('banned account is');
      await expect(main).toContainText('Reviews by banned accounts are not shown or counted.');
      await expect(main.locator('.review-summary')).toHaveText('5.0 / 5 · 1 review');
    }
    // The administrator sees it labelled and counted apart, at phone widths without sideways scroll.
    await admin.goto(productURL);
    const labelled = admin.locator('article.review', { hasText: bad });
    await expect(labelled.locator('.badge')).toHaveText(['Completed order (test network)', 'Not counted: reviewer\'s account is banned']);
    await expect(admin.getByText('1 review by a banned account is not shown to others or counted.')).toBeVisible();
    await expect(admin.locator('.review-summary')).toHaveText('5.0 / 5 · 1 review');
    for (const width of [390, 320]) {
      await admin.setViewportSize({ width, height: 900 });
      expect(await admin.evaluate(() => document.documentElement.scrollWidth), `banned review label at ${width}px`).toBeLessThanOrEqual(width);
    }
    expect(sql(`SELECT count(*) FROM reviews WHERE order_id IN ('${badOrder}','${goodOrder}') AND hidden_at IS NOT NULL`)).toBe('0');

    // Lifting the ban counts it again.
    expect(await submitBan('Unban', 'Appeal upheld: the orders were real.')).toBe(303);
    await expectNotice(admin, 'Ban lifted.');
    await visitor.goto(vendorURL);
    await expect(visitor.locator('main')).toContainText(bad);
    await expect(visitor.locator('main .review-summary')).toHaveText('3.0 / 5 · 2 reviews');
  } finally {
    sql(`UPDATE products SET archived=true,archived_at=now() WHERE id='${productID}';
      DELETE FROM sessions WHERE user_id=(SELECT id FROM users WHERE handle='${adminHandle}');
      UPDATE users SET role='buyer' WHERE handle='${adminHandle}';`);
    await Promise.allSettled([admin.context(), visitorContext].map(context => context.close()));
  }
});
