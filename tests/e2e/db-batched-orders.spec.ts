import { execFileSync } from 'node:child_process';
import { randomBytes } from 'node:crypto';
import { expect, test, type Page } from '@playwright/test';
import { uniqueHandle } from './db-fixtures';
import { signedIn, submitAndLoad } from './db-helpers';
import { waitForStyles } from './styles';

const sql = (query: string) => execFileSync('psql', [process.env.E2E_DATABASE_URL!, '-v', 'ON_ERROR_STOP=1', '-qtAc', query], { encoding: 'utf8' }).trim();
const id = () => randomBytes(32).toString('hex');

async function responsive(page: Page) {
  for (const scale of [100, 200]) {
    if (scale === 200) {
      await page.route('**/*.css*', async route => {
        const stylesheet = await route.fetch();
        await route.fulfill({ response: stylesheet, body: `${await stylesheet.text()}\nhtml { font-size: 200% !important; }` });
      });
      await page.reload();
    }
    for (const width of [320, 390, 768, 1280]) {
      await page.setViewportSize({ width, height: 900 });
      await waitForStyles(page);
      expect(await page.evaluate(() => document.documentElement.scrollWidth), `${width}px at ${scale}% text`).toBeLessThanOrEqual(width);
    }
  }
  await expect(page.locator('script')).toHaveCount(0);
}

// No payment provider runs here. These are real catalog/draft forms against
// PostgreSQL; reservation and settlement are covered by Go simulated-RPC tests.
test('published empty stock is optional in discovery and drafts record a bounded multi-unit total', async ({ browser, baseURL }) => {
  test.setTimeout(90_000);
  const vendorHandle = uniqueHandle('batch_vendor');
  const buyerHandle = uniqueHandle('batch_buyer');
  const vendor = await signedIn(browser, baseURL, vendorHandle, 'browser-batch-vendor-123', true);
  const buyer = await signedIn(browser, baseURL, buyerHandle, 'browser-batch-buyer-123', true, 320);
  const product = id(), empty = id(), search = uniqueHandle('stock_case');
  const title = `${search} available`, emptyTitle = `${search} empty`;
  try {
    sql(`UPDATE users SET role='vendor' WHERE handle='${vendorHandle}';
      INSERT INTO products(id,vendor_id,title,description,category,region,kind,btc,xmr,stock)
      SELECT '${product}',id,'${title}','Disposable quantity browser fixture','Hardware','Worldwide','physical',0,250000000000,3 FROM users WHERE handle='${vendorHandle}';
      INSERT INTO products(id,vendor_id,title,description,category,region,kind,btc,xmr,stock)
      SELECT '${empty}',id,'${emptyTitle}','Disposable published empty fixture','Hardware','Worldwide','physical',0,250000000000,0 FROM users WHERE handle='${vendorHandle}';`);
    await buyer.goto(`/?q=${search}`);
    await expect(buyer.locator('.listings-grid .product-card')).toHaveCount(2);
    await buyer.getByText('Browse & filter listings', { exact: true }).click();
    const filters = buyer.locator('.filter-details form');
    await filters.getByLabel('Hide out of stock', { exact: true }).check();
    await submitAndLoad(buyer, filters.getByRole('button', { name: 'Apply filters', exact: true }));
    await expect(buyer.locator('.listings-grid .product-card')).toHaveCount(1);
    await expect(buyer.locator('.listings-grid')).toContainText(title);
    await expect(buyer.locator('.listings-grid')).not.toContainText(emptyTitle);
    await expect(buyer).toHaveURL(/in_stock=1/);
    expect(sql(`SELECT count(*) FROM products WHERE id='${empty}' AND stock=0 AND NOT archived`)).toBe('1');
    await buyer.goto(`/product?id=${empty}`);
    await expect(buyer.getByText('Out of stock.', { exact: true })).toBeVisible();
    await expect(buyer.getByRole('link', { name: 'Review order draft' })).toHaveCount(0);
    await buyer.goto(`/checkout?id=${empty}`);
    await expect(buyer.locator('form[action="/orders"]')).toHaveCount(0);

    await buyer.goto(`/checkout?id=${product}`);
    const quantity = buyer.getByRole('spinbutton', { name: 'Quantity', exact: true });
    await expect(quantity).toHaveAttribute('min', '1');
    await expect(quantity).toHaveAttribute('max', '3');
    await quantity.fill('3');
    await submitAndLoad(buyer, buyer.getByRole('button', { name: 'Create unfunded draft' }));
    const orderURL = buyer.url();
    const order = new URL(orderURL).searchParams.get('id')!;
    await expect(buyer.locator('.key-values > div', { has: buyer.getByText('Quantity', { exact: true }) })).toContainText('3 units');
    expect(sql(`SELECT quantity||':'||amount FROM orders WHERE id='${order}'`)).toBe('3:750000000000');
    expect(sql(`SELECT stock FROM products WHERE id='${product}'`)).toBe('3');

    // Revisit checkout and save a different quantity into the same draft.
    await buyer.goto(`/checkout?id=${product}`);
    await buyer.getByRole('spinbutton', { name: 'Quantity', exact: true }).fill('2');
    await submitAndLoad(buyer, buyer.getByRole('button', { name: 'Create unfunded draft' }));
    expect(buyer.url()).toBe(orderURL);
    expect(sql(`SELECT quantity||':'||amount FROM orders WHERE id='${order}'`)).toBe('2:500000000000');
    expect(sql(`SELECT count(*) FROM orders WHERE buyer_id=(SELECT id FROM users WHERE handle='${buyerHandle}') AND product_id='${product}'`)).toBe('1');
    await responsive(buyer);
    await vendor.goto(orderURL);
    await expect(vendor.locator('.key-values > div', { has: vendor.getByText('Quantity', { exact: true }) })).toContainText('2 units');
    await responsive(vendor);
  } finally {
    await Promise.allSettled([buyer.context().close(), vendor.context().close()]);
  }
});
