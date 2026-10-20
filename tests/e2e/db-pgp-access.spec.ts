import { execFileSync } from 'node:child_process';
import { randomBytes } from 'node:crypto';
import { expect, test, type Page } from '@playwright/test';
import { uniqueHandle } from './db-fixtures';
import { pgpFixture, seedPGPAccess, signedIn, submitStatus } from './db-helpers';

// This journey checks the browse-only boundary using real HTTP requests. SQL supplies proof only for the later
// authorized commerce fixture; real challenge decryption and replay rejection are covered by Go integration tests.
test('browse-only sessions cannot purchase or message, even after saving a public key', async ({ browser, baseURL }) => {
  const sql = (query: string) => execFileSync('psql', [process.env.E2E_DATABASE_URL!, '-v', 'ON_ERROR_STOP=1', '-qtAc', query], { encoding: 'utf8' }).trim();
  const buyerHandle = uniqueHandle('pgp_access_buyer');
  const vendorHandle = uniqueHandle('pgp_access_vendor');
  const password = 'browser-pgp-access-password-123';
  const buyer = await signedIn(browser, baseURL, buyerHandle, password, true);
  const vendor = await signedIn(browser, baseURL, vendorHandle, password, true);
  const product = randomBytes(16).toString('hex');
  const refusal = 'This browser has not completed PGP authentication.';
  let other: Page | undefined;
  try {
    await seedPGPAccess(vendor);
    sql(`UPDATE users SET role='vendor' WHERE handle='${vendorHandle}';
      INSERT INTO products(id,vendor_id,title,description,category,region,kind,xmr,stock)
      SELECT '${product}',id,'PGP access fixture','Disposable browser fixture','Digital','Worldwide','digital',500000000000,5
      FROM users WHERE handle='${vendorHandle}';`);

    const post = async (page: Page, path: string, fields: Record<string, string>) => {
      await page.goto('/account');
      const csrf = await page.locator('input[name="csrf"]').first().inputValue();
      return page.request.post(path, { form: { csrf, form_nonce: randomBytes(16).toString('hex'), ...fields }, maxRedirects: 0 });
    };
    const refuseBoth = async (page: Page) => {
      const order = await post(page, '/orders', { product_id: product, currency: 'XMR' });
      expect(order.status()).toBe(403);
      expect(await order.text()).toContain(refusal);
      const message = await post(page, '/messages', { recipient: vendorHandle, body: pgpFixture('message-to-recipient.asc') });
      expect(message.status()).toBe(403);
      expect(await message.text()).toContain(refusal);
    };

    expect((await buyer.goto(`/product?id=${product}`))?.status()).toBe(200);
    expect((await buyer.goto(`/checkout?id=${product}`))?.status()).toBe(200);
    await expect(buyer.getByRole('button', { name: 'Create unfunded draft' })).toHaveCount(0);
    await refuseBoth(buyer);

    // Saving a key alone gives no possession proof for this browser.
    await buyer.goto('/account');
    const profile = buyer.locator('form', { has: buyer.getByRole('button', { name: 'Save profile' }) });
    await profile.getByLabel('PGP public key', { exact: true }).fill(pgpFixture('recipient.pub.asc'));
    await profile.getByLabel('Current password', { exact: true }).fill(password);
    expect(await submitStatus(buyer, () => profile.getByRole('button', { name: 'Save profile' }).click())).toBe(303);
    await refuseBoth(buyer);
    expect(sql(`SELECT count(*) FROM orders WHERE product_id='${product}'`)).toBe('0');
    expect(sql(`SELECT count(*) FROM messages m JOIN users u ON u.id=m.sender_id WHERE u.handle='${buyerHandle}'`)).toBe('0');

    // Existing PGP authentication is scoped to one session, rather than the whole account.
    await seedPGPAccess(buyer);
    other = await signedIn(browser, baseURL, buyerHandle, password, false);
    await refuseBoth(other);
    const order = await post(buyer, '/orders', { product_id: product, currency: 'XMR' });
    expect(order.status()).toBe(303);
    const message = await post(buyer, '/messages', { recipient: vendorHandle, body: pgpFixture('message-to-recipient.asc') });
    expect(message.status()).toBe(303);
    expect(sql(`SELECT count(*) FROM orders WHERE product_id='${product}'`)).toBe('1');
    expect(sql(`SELECT count(*) FROM messages m JOIN users u ON u.id=m.sender_id WHERE u.handle='${buyerHandle}'`)).toBe('1');
  } finally {
    await buyer.context().close();
    await vendor.context().close();
    await other?.context().close();
  }
});
