import { execFileSync } from 'node:child_process';
import { randomBytes } from 'node:crypto';
import { expect, test } from '@playwright/test';
import { uniqueHandle } from './db-fixtures';
import { signedIn, submitAndLoad, submitStatus } from './db-helpers';
import { waitForStyles } from './styles';

// Financial/dispute state is seeded, not payment evidence. Actual browser file
// forms run without scripts against PostgreSQL and the protocol-faithful fake
// clamd, whose marker response exercises the application's fail-closed behavior.
test('dispute images upload, remain private and reject documents and scanner findings', async ({ browser, baseURL }) => {
  test.setTimeout(90_000);
  const sql = (query: string) => execFileSync('psql', [process.env.E2E_DATABASE_URL!, '-v', 'ON_ERROR_STOP=1', '-qtAc', query], { encoding: 'utf8' }).trim();
  const handles = ['evidence_buyer', 'evidence_vendor', 'evidence_outside'].map(uniqueHandle);
  const pages = [];
  for (const handle of handles) pages.push(await signedIn(browser, baseURL, handle, 'browser-evidence-password-123', true, 320));
  const [buyer, vendor, outsider] = pages;
  const product = randomBytes(16).toString('hex');
  const order = randomBytes(32).toString('hex');
  const dispute = randomBytes(16).toString('hex');
  const png = Buffer.from('iVBORw0KGgoAAAANSUhEUgAAAAIAAAACCAIAAAD91JpzAAAAEklEQVR4nGP4z8DAAMIM/4EAAB/uBfsL2WiLAAAAAElFTkSuQmCC', 'base64');
  try {
    sql(`UPDATE users SET role='vendor' WHERE handle='${handles[1]}';
      INSERT INTO products(id,vendor_id,title,description,category,region,kind,btc,xmr,stock)
      SELECT '${product}',id,'Image evidence browser fixture','Synthetic fixture only','Hardware','Worldwide','physical',0,500000000000,1 FROM users WHERE handle='${handles[1]}';
      INSERT INTO orders(id,buyer_id,product_id,currency,amount,state)
      SELECT '${order}',id,'${product}','XMR',500000000000,'disputed' FROM users WHERE handle='${handles[0]}';
      INSERT INTO disputes(id,order_id,reason,status) VALUES('${dispute}','${order}','Synthetic browser dispute fixture','Open');`);
    await buyer.goto(`/order?id=${order}`);
    const form = buyer.locator('form[action="/orders/evidence"]');
    await form.getByLabel('Evidence description (optional)').fill('Synthetic case image');
    await form.getByLabel('Evidence image', { exact: true }).setInputFiles({ name: 'evidence.png', mimeType: 'image/png', buffer: Buffer.concat([png, Buffer.from('PRIVATE-FIXTURE-METADATA')]) });
    await submitAndLoad(buyer, form.getByRole('button', { name: 'Upload evidence', exact: true }));
    await expect(buyer).toHaveURL(new RegExp(`/order\\?id=${order}#dispute-evidence$`));
    const evidence = buyer.getByRole('region', { name: 'Dispute image evidence' });
    await expect(evidence).toContainText('Synthetic case image');
    const imageURL = await evidence.locator('.photo-thumb').getAttribute('href');
    expect(imageURL).toMatch(/^\/desk\/evidence\/[0-9a-f]{32}-f$/);
    const response = await buyer.request.get(imageURL!);
    expect(response.status()).toBe(200);
    expect(response.headers()['content-type']).toBe('image/jpeg');
    expect(response.headers()['cache-control']).toBe('no-store');
    const normalized = await response.body();
    expect(normalized.subarray(0, 3).toString('hex')).toBe('ffd8ff');
    expect(normalized.includes(Buffer.from('PRIVATE-FIXTURE-METADATA'))).toBe(false);
    await vendor.goto(`/order?id=${order}`);
    await expect(vendor.getByRole('region', { name: 'Dispute image evidence' })).toContainText('Synthetic case image');
    expect((await vendor.request.get(imageURL!)).status()).toBe(200);
    expect((await outsider.request.get(imageURL!)).status()).toBe(404);
    expect((await buyer.request.get(imageURL!.replace('/desk/evidence/', '/img/'))).status()).toBe(404);
    for (const [body, expected] of [[Buffer.from('%PDF-1.7 renamed document'), 415], [Buffer.concat([png, Buffer.from('OPSECMKT-CLAMD-TEST-MARKER')]), 422]] as const) {
      await buyer.goto(`/order?id=${order}`);
      const upload = buyer.locator('form[action="/orders/evidence"]');
      await upload.getByLabel('Evidence image', { exact: true }).setInputFiles({ name: 'renamed.png', mimeType: 'image/png', buffer: body });
      expect(await submitStatus(buyer, () => upload.getByRole('button', { name: 'Upload evidence', exact: true }).click())).toBe(expected);
      expect(sql(`SELECT count(*) FROM images WHERE order_id='${order}'`)).toBe('1');
    }
    await buyer.goto(`/order?id=${order}`);
    for (const width of [320, 390, 768, 1280]) {
      await buyer.setViewportSize({ width, height: 900 });
      await waitForStyles(buyer);
      expect(await buyer.evaluate(() => document.documentElement.scrollWidth)).toBeLessThanOrEqual(width);
    }
    await buyer.goto('/messages');
    await expect(buyer.locator('main')).not.toContainText('Synthetic case image');
    await expect(buyer.locator('script')).toHaveCount(0);
  } finally {
    for (const page of pages) await page.context().close();
  }
});
