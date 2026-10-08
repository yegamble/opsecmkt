import { execFileSync } from 'node:child_process';
import { randomBytes } from 'node:crypto';
import { expect, test, type Page } from '@playwright/test';
import { uniqueHandle } from './db-fixtures';
import { expectNotice, signedIn, withinViewport } from './db-helpers';
import { waitForStyles } from './styles';

// A-444 (P4-R8): the cutover lever and the escrow-only readiness panel, with JavaScript disabled at 320 px. This
// database server runs no pooled wallet and no escrow, so the intake form offers only escrow only and paused, and an
// escrow-only coin has every listing "not available for purchase" in it (its escrow is off). Fresh accounts only: an
// administrator promoted in SQL and a vendor whose listing is seeded in SQL. The intake settings are put back to their
// default ('true', which the form cannot set without a pooled wallet) in SQL at the end, so later specs are unaffected.

// goSpaces is the SQL literal of every rune Go's unicode.IsSpace reports (escrow_readiness.go's goSpaces): ASCII white
// space, U+0085, U+00A0 and Unicode's other White_Space runes. goBlank is delivery content of only such runes.
const spaceRunes = [0x09, 0x0a, 0x0b, 0x0c, 0x0d, 0x20, 0x85, 0xa0, 0x1680, 0x2000, 0x2001, 0x2002, 0x2003, 0x2004, 0x2005, 0x2006,
  0x2007, 0x2008, 0x2009, 0x200a, 0x2028, 0x2029, 0x202f, 0x205f, 0x3000];
const sqlRunes = (runes: number[]) => `U&'${runes.map((r) => '\\' + r.toString(16).padStart(4, '0')).join('')}'`;
const goSpaces = sqlRunes(spaceRunes);
const goBlank = sqlRunes([0x3000, 0x85, 0xa0, 0x2028]);

async function fits(page: Page, label: string) {
  await waitForStyles(page);
  expect(await page.evaluate(() => document.documentElement.scrollWidth), `${label} at 320px`).toBeLessThanOrEqual(320);
}

test('intake form sets escrow only and paused with no pooled wallet; readiness panel counts what SQL counts', async ({ browser, baseURL }) => {
  const database = process.env.E2E_DATABASE_URL!;
  const sql = (query: string) => execFileSync('psql', [database, '-v', 'ON_ERROR_STOP=1', '-qtAc', query], { encoding: 'utf8' }).trim();
  const adminPassword = 'browser-cutover-admin-password-123';
  const adminHandle = uniqueHandle('cutover_admin');
  const vendorHandle = uniqueHandle('cutover_vendor_long_handle');
  const productID = randomBytes(16).toString('hex');
  const blankID = randomBytes(16).toString('hex');
  const title = `Cutover ${uniqueHandle('item')}`;
  const admin = await signedIn(browser, baseURL, adminHandle, adminPassword, true, 320);
  const vendor = await signedIn(browser, baseURL, vendorHandle, 'browser-cutover-vendor-123', true, 320);
  const buyer = await signedIn(browser, baseURL, uniqueHandle('cutover_buyer'), 'browser-cutover-buyer-123', true, 320);
  const visitorContext = await browser.newContext({ baseURL, javaScriptEnabled: false, viewport: { width: 320, height: 900 } });
  const visitor = await visitorContext.newPage();
  try {
    sql(`UPDATE users SET role='admin' WHERE handle='${adminHandle}';
      UPDATE users SET role='vendor' WHERE handle='${vendorHandle}';
      INSERT INTO products(id,vendor_id,title,description,category,region,kind,btc,xmr,stock)
        SELECT '${productID}',id,'${title}','Seeded by db-escrow-cutover.spec.ts','Digital','Worldwide','digital',100000,500000000000,3 FROM users WHERE handle='${vendorHandle}';
      INSERT INTO products(id,vendor_id,title,description,category,region,kind,btc,xmr,stock,delivery_content)
        SELECT '${blankID}',id,'${title} blank','Seeded by db-escrow-cutover.spec.ts','Digital','Worldwide','digital',100000,500000000000,3,${goBlank} FROM users WHERE handle='${vendorHandle}';`);

    await admin.goto('/admin');
    // No pooled wallet: On is not offered for either coin; escrow only and paused are.
    for (const cur of ['BTC', 'XMR']) {
      const form = admin.locator('form', { has: admin.getByRole('button', { name: `Save new ${cur} payments` }) });
      await expect(form.getByRole('radio')).toHaveCount(2);
      await expect(form.getByRole('radio', { name: /^Escrow only/ })).toBeVisible();
      await expect(form.getByRole('radio', { name: /^Paused/ })).toBeVisible();
      await expect(form.getByText(`On is not offered: configure the ${cur} wallet RPC on the server first.`)).toBeVisible();
      await withinViewport(form, 320);
    }

    // The readiness panel's counts are SQL's: active listings that cannot use escrow, and vendors with active
    // listings and no active Bitcoin escrow key. The seeded listing is listed with its reason.
    // btrim with every rune Go's strings.TrimSpace removes, as escrowEligibleSQL does (goSpaces): the second seeded listing's
    // delivery content is only such white space, which plain btrim (spaces only) would count as content.
    const listings = Number(sql(`SELECT count(*) FROM products p WHERE NOT p.archived AND NOT (p.kind='digital' AND btrim(p.delivery_content,${goSpaces})<>'' AND char_length(p.delivery_content)<=32000 OR p.kind='physical' AND p.dispatch_days IS NOT NULL)`));
    const vendors = Number(sql(`SELECT count(DISTINCT p.vendor_id) FROM products p WHERE NOT p.archived AND NOT EXISTS(SELECT 1 FROM user_escrow_keys k WHERE k.user_id=p.vendor_id AND k.active_at IS NOT NULL)`));
    expect(listings).toBeGreaterThan(0);
    await expect(admin.locator('#escrow-readiness-listings strong')).toHaveText(String(listings));
    await expect(admin.locator('#escrow-readiness-vendors strong')).toHaveText(String(vendors));
    const table = admin.getByRole('region', { name: /active listings? cannot use escrow/ });
    const row = table.locator('tr', { has: admin.getByRole('link', { name: title }) });
    if (listings <= 100) {
      await expect(row.locator('td').nth(2)).toHaveText('This digital listing has no automatic delivery content, which escrow needs.');
    }
    await expect(admin.getByText('BTC escrow: off', { exact: false })).toBeVisible();
    await expect(admin.getByText('XMR escrow: off', { exact: false })).toBeVisible();
    await fits(admin, 'admin intake and readiness');

    // Escrow only for BTC, with the administrator's password.
    const btc = admin.locator('form', { has: admin.getByRole('button', { name: 'Save new BTC payments' }) });
    await btc.getByRole('radio', { name: /^Escrow only/ }).check();
    await btc.getByLabel('Current password', { exact: true }).fill(adminPassword);
    await btc.getByRole('button', { name: 'Save new BTC payments' }).click();
    await expectNotice(admin, 'New payments set to escrow only.');
    await expect(admin.getByRole('heading', { name: 'BTC intake: Escrow only' })).toBeVisible();
    await expect(admin.getByText('Nothing can be bought in BTC: its escrow is off.')).toBeVisible();
    expect(sql(`SELECT value FROM settings WHERE key='payment_intake_BTC'`)).toBe('escrow');
    expect(sql(`SELECT count(*) FROM audit_events a JOIN users u ON u.id=a.user_id WHERE u.handle='${adminHandle}' AND a.action LIKE 'Set new BTC payments to escrow only%'`)).toBe('1');
    await fits(admin, 'admin after escrow only');

    // XMR is still on: the listing can be bought in XMR, with the BTC reason shown.
    await visitor.goto(`/product?id=${productID}`);
    await expect(visitor.getByText('Not available for purchase in BTC.')).toBeVisible();
    await expect(visitor.getByRole('link', { name: 'Review order draft' })).toBeVisible();
    await buyer.goto(`/checkout?id=${productID}`);
    await expect(buyer.getByLabel('Pay with').locator('option')).toHaveText([/XMR/]);

    // XMR escrow only as well: the listing is not available for purchase at all; no buy form, no draft form.
    await admin.goto('/admin');
    const xmr = admin.locator('form', { has: admin.getByRole('button', { name: 'Save new XMR payments' }) });
    await xmr.getByRole('radio', { name: /^Escrow only/ }).check();
    await xmr.getByLabel('Current password', { exact: true }).fill(adminPassword);
    await xmr.getByRole('button', { name: 'Save new XMR payments' }).click();
    await expectNotice(admin, 'New payments set to escrow only.');
    await visitor.goto(`/product?id=${productID}`);
    await expect(visitor.getByText('Not available for purchase in BTC.')).toBeVisible();
    await expect(visitor.getByText('Not available for purchase in XMR.')).toBeVisible();
    await expect(visitor.getByText('Escrow for BTC is not enabled on this market.')).toBeVisible();
    await expect(visitor.getByRole('link', { name: 'Review order draft' })).toHaveCount(0);
    await fits(visitor, 'listing not available for purchase');
    await buyer.goto(`/checkout?id=${productID}`);
    await expect(buyer.locator('form[action="/orders"]')).toHaveCount(0);
    await fits(buyer, 'checkout not available for purchase');
    await expect(buyer.getByText('Not available for purchase in XMR.')).toBeVisible();    // Its vendor reads the same, with what only the operator can change.
    await vendor.goto(`/product?id=${productID}`);
    await expect(vendor.getByText('Only the market\'s operator can enable it.').first()).toBeVisible();

    // Paused, then back to escrow only: reversible from the form.
    await admin.goto('/admin');
    await btc.getByRole('radio', { name: /^Paused/ }).check();
    await btc.getByLabel('Current password', { exact: true }).fill(adminPassword);
    await btc.getByRole('button', { name: 'Save new BTC payments' }).click();
    await expectNotice(admin, 'New payment addresses paused.');
    await expect(admin.getByRole('heading', { name: 'BTC intake: Paused' })).toBeVisible();
    expect(sql(`SELECT value FROM settings WHERE key='payment_intake_BTC'`)).toBe('false');
  } finally {
    sql(`UPDATE settings SET value='true' WHERE key IN ('payment_intake_BTC','payment_intake_XMR')`);
    await visitorContext.close();
  }
});
