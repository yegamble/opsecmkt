import { execFileSync } from 'node:child_process';
import { randomBytes } from 'node:crypto';
import { expect, test, type Page } from '@playwright/test';
import { uniqueHandle } from './db-fixtures';
import { expectNotice, pgpFixture, seedRecordedMoneroEscrow, signedIn, submitAndLoad, submitStatus } from './db-helpers';
import { waitForStyles } from './styles';

// A-124 and A-162 against a real database, JavaScript disabled, at 320 px, with fresh accounts: the party who did
// not open a dispute puts one reply on record from the order page; the moderation desk shows the reason and the
// reply with their roles and line breaks, and which staff member each party messaged since the dispute opened
// (from message metadata only, never a message body).

async function fitsAndNoScripts(page: Page, what: string) {
  await expect(page.locator('script')).toHaveCount(0);
  await waitForStyles(page);
  expect(await page.evaluate(() => document.documentElement.scrollWidth), what).toBeLessThanOrEqual(page.viewportSize()!.width);
}

test('the other party replies once and the desk names who holds encrypted evidence', async ({ browser, baseURL }) => {
  test.setTimeout(120_000);
  const database = process.env.E2E_DATABASE_URL!;
  const sql = (query: string) => execFileSync('psql', [database, '-v', 'ON_ERROR_STOP=1', '-qtAc', query], { encoding: 'utf8' }).trim();
  const [buyerHandle, vendorHandle, holderHandle, resolverHandle] = ['dsp_buyer', 'dsp_vendor', 'dsp_holder', 'dsp_resolver'].map(uniqueHandle);
  const pages = await Promise.all([
    signedIn(browser, baseURL, buyerHandle, 'browser-dispute-buyer-123', true, 320),
    signedIn(browser, baseURL, vendorHandle, 'browser-dispute-vendor-123', true, 320),
    signedIn(browser, baseURL, holderHandle, 'browser-dispute-holder-123', true, 320),
    signedIn(browser, baseURL, resolverHandle, 'browser-dispute-resolver-123', true, 320),
  ]);
  const [buyer, vendor, , resolver] = pages;
  const productID = randomBytes(16).toString('hex');
  const orderID = randomBytes(32).toString('hex');
  try {
    // A shipped escrow order: this suite seeds its funding and shipping, then exercises live dispute forms.
    sql(`UPDATE users SET role='vendor' WHERE handle='${vendorHandle}';
      UPDATE users SET role='moderator' WHERE handle='${holderHandle}';
      UPDATE users SET role='admin' WHERE handle='${resolverHandle}';
      INSERT INTO products(id,vendor_id,title,description,category,region,kind,btc,xmr,stock)
        SELECT '${productID}',id,'Dispute record fixture','Seeded by db-disputes.spec.ts','Hardware','Worldwide','physical',100000,500000000000,1 FROM users WHERE handle='${vendorHandle}';
      INSERT INTO orders(id,buyer_id,product_id,currency,amount,state)
        SELECT '${orderID}',id,'${productID}','XMR',100000,'shipped' FROM users WHERE handle='${buyerHandle}';`);

    seedRecordedMoneroEscrow(sql, orderID);

    // The buyer opens the dispute from the order page (a <details> form, no script), with a two-line reason.
    expect((await buyer.goto(`/order?id=${orderID}`))?.status()).toBe(200);
    await buyer.locator('summary', { hasText: 'Open a dispute' }).click();
    await buyer.getByLabel('Describe the issue').fill('The parcel arrived empty.\nThe seal on the box was broken.');
    await submitAndLoad(buyer, buyer.getByRole('button', { name: 'Submit dispute' }));
    await expect(buyer).toHaveURL(/\/disputes$/);
    await expectNotice(buyer, 'Dispute opened.');
    // Evidence to one staff member, encrypted (the fixture ciphertext; the holder has no key, so it is "recipient unknown").
    await buyer.goto(`/messages?to=${holderHandle}`);
    await buyer.getByLabel('PGP encrypted message').fill(pgpFixture('message-to-recipient.asc'));
    await submitAndLoad(buyer, buyer.getByRole('button', { name: 'Send encrypted text ↗' }));
    await expect(buyer).toHaveURL(/\/messages$/);
    await expectNotice(buyer, 'Message sent.');
    await buyer.goto(`/order?id=${orderID}`);
    await expect(buyer.locator('form[action="/disputes/reply"]')).toHaveCount(0);
    await expect(buyer.locator('#order-dispute').locator('..')).toContainText('The vendor may put one reply on record');
    await expect(buyer.locator('#dispute-contacts').locator('..')).toContainText('Other moderators and administrators can see which staff member you messaged and when, never what.');

    // A-280: the resolver opens the desk before the vendor replies and fills in the resolve form, which says the
    // vendor has not replied; it is submitted after the reply (below).
    await resolver.goto('/moderator');
    const staleForm = resolver.locator('article.message', { hasText: orderID }).locator('form[action="/resolve"]');
    await expect(staleForm).toContainText('The vendor has not replied; only the buyer’s account is on record.');
    const decision = 'Refund: the courier has no record of delivery at the agreed place.';
    await staleForm.locator('input[name="outcome"][value="refund"]').check();
    await staleForm.getByLabel('Decision').fill(decision);
    await staleForm.locator('input[name="payout_confirmed"]').check();
    await staleForm.getByLabel('Current password').fill('browser-dispute-resolver-123');

    // The vendor is told a reply exists, and puts one on record.
    await vendor.goto('/notifications');
    await expect(vendor.locator('article.message', { hasText: `Order ${orderID.slice(0, 8)} is now Disputed` })).toContainText('You may put one reply to the dispute on record from the order page before a moderator decides.');
    await vendor.goto(`/order?id=${orderID}`);
    const reason = vendor.locator('#order-dispute').locator('..').locator('.dispute-text').first();
    await expect(reason).toHaveText('The parcel arrived empty.\nThe seal on the box was broken.');
    await expect(reason).toHaveCSS('white-space', 'pre-wrap');
    const field = vendor.getByLabel('Your reply to the dispute');
    await expect(field).toHaveAccessibleDescription(/No independent moderator or administrator has a usable PGP key yet.*Stored unencrypted\. The buyer reads it on this order.*Never include an address, real name or tracking number/);
    await fitsAndNoScripts(vendor, 'vendor order page with the reply form at 320 px');
    await field.fill('The parcel left sealed.\nThe courier photo shows the box intact.');
    await submitAndLoad(vendor, vendor.getByRole('button', { name: 'Put reply on record' }));
    await expectNotice(vendor, 'Reply added to the dispute.');
    await expect(vendor.locator('form[action="/disputes/reply"]')).toHaveCount(0);

    // Both parties see the reply after reload, with its role and line breaks; the history only says a reply was made.
    for (const page of [vendor, buyer]) {
      await page.goto(`/order?id=${orderID}`);
      const dispute = page.locator('#order-dispute').locator('..');
      await expect(dispute.getByRole('heading', { name: 'Reply from the vendor' })).toBeVisible();
      const reply = dispute.locator('.dispute-text').nth(1);
      await expect(reply).toHaveText('The parcel left sealed.\nThe courier photo shows the box intact.');
      await expect(reply).toHaveCSS('white-space', 'pre-wrap');
      await expect(page.locator('.order-timeline')).toContainText('Replied to the dispute; the reply is shown under Dispute.');
      await expect(page.locator('.order-timeline')).not.toContainText('courier photo');
      await fitsAndNoScripts(page, 'order page with the reply at 320 px');
    }

    // A-280: the form loaded before the reply is refused with 409 and the desk again: the reason in the alert, the
    // reply shown, the decision kept; nothing is resolved.
    expect(await submitStatus(resolver, () => staleForm.getByRole('button', { name: 'Resolve dispute' }).click())).toBe(409);
    await expect(resolver.getByRole('alert')).toHaveText('The vendor replied to this dispute after the form was loaded. Read the reply below and decide again. Nothing was changed.');
    await expect(resolver).toHaveTitle(/^Error: /);
    const refreshed = resolver.locator('article.message', { hasText: orderID });
    await expect(refreshed).toContainText('Reply from the vendor');
    await expect(refreshed.locator('form[action="/resolve"]')).toContainText('The vendor replied; the reply is shown above.');
    await expect(refreshed.getByLabel('Decision')).toHaveValue(decision);
    await fitsAndNoScripts(resolver, 'refused resolve at 320 px');
    expect(sql(`SELECT state FROM orders WHERE id='${orderID}'`)).toBe('disputed');

    // The resolver (who holds nothing) sees both sides and that the buyer messaged the holder once; no ciphertext.
    await resolver.goto('/moderator');
    const entry = resolver.locator('article.message', { hasText: orderID });
    await expect(entry.locator('.dispute-text').first()).toHaveText('The parcel arrived empty.\nThe seal on the box was broken.');
    await expect(entry).toContainText('by the buyer');
    await expect(entry).toContainText('Reply from the vendor');
    await expect(entry.locator('.dispute-evidence li').first()).toHaveText(new RegExp(`^Buyer → ${holderHandle} \\(1, latest \\d{4}-\\d{2}-\\d{2} \\d{2}:\\d{2} UTC\\)$`));
    await expect(entry.locator('.dispute-evidence li').nth(1)).toHaveText('Vendor → none');
    const form = entry.locator('form[action="/resolve"]');
    await expect(form).toContainText('The vendor replied; the reply is shown above.');
    await expect(form).toContainText('Before deciding, ask each staff member listed under Encrypted evidence holders');
    await expect(resolver.locator('main')).not.toContainText('BEGIN PGP MESSAGE');
    await fitsAndNoScripts(resolver, 'moderation desk at 320 px');
  } finally {
    sql(`UPDATE users SET role='buyer' WHERE handle='${resolverHandle}'`);
    for (const page of pages) await page.context().close();
  }
});
