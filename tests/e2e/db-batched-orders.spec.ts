import { execFileSync } from 'node:child_process';
import { randomBytes } from 'node:crypto';
import { expect, test, type Page } from '@playwright/test';
import { ADMIN, uniqueHandle } from './db-fixtures';
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

// Funding is seeded, not simulated by submitting plaintext or connecting a
// wallet. Fulfillment itself uses the vendor's real single-submission form.
test('a vendor delivers all manual digital units once on a scrolling order page', async ({ browser, baseURL }) => {
  test.setTimeout(90_000);
  const vendorHandle = uniqueHandle('manual_vendor');
  const buyerHandle = uniqueHandle('manual_buyer');
  const vendor = await signedIn(browser, baseURL, vendorHandle, 'browser-manual-vendor-123', true, 320);
  const buyer = await signedIn(browser, baseURL, buyerHandle, 'browser-manual-buyer-123', true, 320);
  const product = id(), order = id();
  const content = ['Unit 1: https://example.invalid/download/first', 'Unit 2: a distinct manual text key', ...Array.from({ length: 160 }, (_, index) => `Batch instructions ${index + 1}: retain this text with the order.`)].join('\n');
  try {
    sql(`UPDATE users SET role='vendor' WHERE handle='${vendorHandle}';
      INSERT INTO products(id,vendor_id,title,description,category,region,kind,btc,xmr,stock,digital_delivery_mode)
      SELECT '${product}',id,'Manual digital batch','Disposable manual delivery browser fixture','Digital','Worldwide','digital',0,250000000000,0,'manual' FROM users WHERE handle='${vendorHandle}';
      INSERT INTO orders(id,buyer_id,product_id,currency,amount,quantity,state,listing_title,listing_description,listing_kind)
      SELECT '${order}',id,'${product}','XMR',500000000000,2,'paid','Manual digital batch','Disposable manual delivery browser fixture','digital' FROM users WHERE handle='${buyerHandle}';`);
    await vendor.goto(`/order?id=${order}`);
    const form = vendor.locator('form[action="/orders/deliver"]');
    await expect(form).toHaveCount(1);
    await form.getByRole('textbox', { name: 'Delivery content', exact: true }).fill(content);
    await submitAndLoad(vendor, form.getByRole('button', { name: 'Deliver digital content', exact: true }));
    await expect(vendor.locator('form[action="/orders/deliver"]')).toHaveCount(0);
    expect(sql(`SELECT count(*) FROM deliveries WHERE order_id='${order}'`)).toBe('1');
    expect(sql(`SELECT state FROM orders WHERE id='${order}'`)).toBe('delivered');
    await buyer.goto(`/order?id=${order}`);
    const delivered = buyer.getByRole('textbox', { name: 'Digital delivery', exact: true });
    await expect(delivered).toHaveCount(1);
    await expect(delivered).toHaveValue(content);
    await expect(delivered).toHaveAttribute('readonly', '');
    await waitForStyles(buyer);
    const box = await delivered.evaluate(node => ({ height: node.clientHeight, content: node.scrollHeight }));
    expect(box.height).toBeLessThanOrEqual(420);
    expect(box.content).toBeGreaterThan(box.height);
    await responsive(buyer);
    await vendor.reload();
    await expect(vendor.getByRole('textbox', { name: 'Digital delivery', exact: true })).toHaveValue(content);
    expect(sql(`SELECT count(*) FROM messages WHERE body LIKE '%Unit 1: https://example.invalid/download/first%'`)).toBe('0');
    expect(sql(`SELECT count(*) FROM notifications WHERE order_id='${order}' AND body LIKE '%distinct manual text key%'`)).toBe('0');
    await buyer.goto('/messages');
    await expect(buyer.locator('.messages-list article.message')).toHaveCount(0);
  } finally {
    await Promise.allSettled([buyer.context().close(), vendor.context().close()]);
  }
});

test('automatic digital stock is imported as distinct private units through vendor forms', async ({ browser, baseURL }) => {
  test.setTimeout(90_000);
  const vendorHandle = uniqueHandle('unit_vendor');
  const vendor = await signedIn(browser, baseURL, vendorHandle, 'browser-unit-vendor-123', true, 320);
  const admin = await signedIn(browser, baseURL, ADMIN.handle, ADMIN.password, false, 320);
  const product = id();
  const title = `Inventory ${uniqueHandle('digital')}`;
  const first = 'Private multiline unit\nhttps://example.invalid/private-unit-one';
  const second = `private-key-${uniqueHandle('second')}`;
  const third = `https://example.invalid/private-unit-${uniqueHandle('third')}`;
  try {
    sql(`UPDATE users SET role='vendor' WHERE handle='${vendorHandle}';
      INSERT INTO products(id,vendor_id,title,description,category,region,kind,btc,xmr,stock,digital_delivery_mode)
      SELECT '${product}',id,'${title}','Disposable private unit inventory browser fixture','Digital','Worldwide','digital',0,250000000000,8,'manual' FROM users WHERE handle='${vendorHandle}';`);
    await vendor.goto(`/listing-edit?id=${product}`);
    const mode = vendor.locator('form[action="/listings/digital-mode"]');
    await mode.locator('select[name="mode"]').selectOption('inventory');
    await submitAndLoad(vendor, mode.getByRole('button'));
    expect(sql(`SELECT stock FROM products WHERE id='${product}'`)).toBe('0');
    const entry = vendor.locator('form[action="/listings/digital-units"]', { has: vendor.getByRole('button', { name: 'Add digital unit', exact: true }) });
    await entry.getByRole('textbox', { name: 'Digital item text or link', exact: true }).fill(first);
    await submitAndLoad(vendor, entry.getByRole('button', { name: 'Add digital unit', exact: true }));
    await vendor.getByText('Bulk import one unit per line', { exact: true }).click();
    const bulk = vendor.locator('form[action="/listings/digital-units"]', { has: vendor.getByRole('button', { name: 'Import digital units', exact: true }) });
    await bulk.getByRole('textbox', { name: 'Digital units (one per line)', exact: true }).fill(`${second}\n${third}`);
    await submitAndLoad(vendor, bulk.getByRole('button', { name: 'Import digital units', exact: true }));
    expect(sql(`SELECT stock FROM products WHERE id='${product}'`)).toBe('3');
    expect(sql(`SELECT count(*) FROM digital_units WHERE product_id='${product}' AND order_id IS NULL`)).toBe('3');
    expect(sql(`SELECT content FROM digital_units WHERE product_id='${product}' AND content LIKE 'Private multiline unit%'`)).toBe(first);
    await expect(vendor.getByRole('heading', { name: 'Digital unit inventory', exact: true })).toBeVisible();
    await responsive(vendor);
    await admin.goto(`/listing-edit?id=${product}`);
    await expect(admin.locator('form[action="/listings/digital-units"]')).toHaveCount(0);
    await expect(admin.locator('main')).not.toContainText(first);
    await expect(admin.locator('main')).not.toContainText(second);
    await expect(admin.locator('main')).not.toContainText(third);
    await admin.goto(`/product?id=${product}`);
    await expect(admin.locator('main')).not.toContainText(second);
    expect(sql(`SELECT count(*) FROM audit_events WHERE action LIKE '%${second}%'`)).toBe('0');
  } finally {
    await Promise.allSettled([vendor.context().close(), admin.context().close()]);
  }
});
