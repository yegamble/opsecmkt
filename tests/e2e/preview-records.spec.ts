import { expect, test, type Page } from '@playwright/test';

// A-81 records disclosure in the read-only preview (JavaScript disabled, every viewport project): /canary#records
// says what the server keeps, who can read it and that nothing is deletable; the pages where users create
// records link to it; the audit export is framed as private operator evidence.

async function noScriptsNoOverflow(page: Page) {
  await expect(page.locator('script')).toHaveCount(0);
  expect(await page.evaluate(() => document.documentElement.scrollWidth)).toBeLessThanOrEqual(page.viewportSize()!.width);
}

test('/canary#records lists what the server keeps, who reads it and that nothing is deletable', async ({ page }) => {
  expect((await page.goto('/canary#records'))?.status()).toBe(200);
  const records = page.locator('section#records');
  await expect(records).toHaveCount(1);
  const heading = records.getByRole('heading', { level: 2, name: 'What this server keeps' });
  await expect(heading).toBeVisible();
  await expect(heading).toBeInViewport();
  await expect(records.locator('table')).toHaveCount(0);
  await expect(records).toContainText('kept until the operator removes it; there is no account or message deletion');
  await expect(records).toContainText('records no IP address or browser details');
  await expect(records).toContainText('reverse proxy');
  for (const item of ['Handle', 'Password', 'Second factors', 'PGP public key', 'Messages', 'Notifications',
    'Orders and drafts', 'Disputes', 'Reviews', 'Digital deliveries', 'Payments', 'Payout addresses', 'Activity log']) {
    await expect(records.locator('li > strong', { hasText: new RegExp(`^${item}`) }), item).toHaveCount(1);
  }
  await expect(records).toContainText('never to be published');
  // Visitors are not told to "check exports": the key lets the operator prove an export they hold.
  await expect(page.locator('main')).not.toContainText('check exports');
  await expect(page.locator('section.audit-key')).toContainText('private operator evidence');
  await noScriptsNoOverflow(page);
});

for (const [path, where] of [['/register', 'main'], ['/account', 'main .page-head'], ['/messages', 'main .notice']]) {
  test(`${path} links to the records disclosure`, async ({ page }) => {
    expect((await page.goto(path))?.status()).toBe(200);
    const link = page.locator(where).getByRole('link', { name: 'What this server keeps' });
    await expect(link).toHaveCount(1);
    await expect(link).toBeVisible();
    await expect(link).toHaveAttribute('href', '/canary#records');
    await noScriptsNoOverflow(page);
    await link.click();
    await expect(page.getByRole('heading', { level: 2, name: 'What this server keeps' })).toBeInViewport();
  });
}

for (const path of ['/register', '/setup']) {
  test(`${path} says the handle is permanent and visible`, async ({ page }) => {
    await page.goto(path);
    const handle = page.getByRole('textbox', { name: 'Handle' });
    await expect(handle).toHaveAccessibleDescription(/cannot be changed.*trading partners and market staff.*Do not reuse/);
    await noScriptsNoOverflow(page);
  });
}

test('catalog replaces the privacy slogan with one factual line linking the records', async ({ page }) => {
  await page.goto('/');
  await expect(page.locator('body')).not.toContainText(/YOUR CHOICE/i);
  // Rendered once in the page, visible at every width without opening the filters.
  const link = page.locator('a[href="/canary#records"]');
  await expect(link).toHaveCount(1);
  await expect(link).toBeVisible();
  await expect(link).toHaveText('What this server keeps');
  await expect(page.locator('.sidebar-note')).toHaveCount(1);
  await expect(page.locator('.sidebar-note')).toContainText('No third-party resources');
  await noScriptsNoOverflow(page);
});

test('vendor page says the market keeps the encrypted shipping message and its metadata', async ({ page }) => {
  await page.goto('/vendor?id=ghost');
  const key = page.getByRole('region', { name: 'PGP public key' });
  await expect(key).toContainText('keeps the encrypted message and who sent it to whom and when');
  await expect(key).not.toContainText('never stores');
  await noScriptsNoOverflow(page);
});

test('admin export panel frames the export as private operator evidence', async ({ page }) => {
  await page.goto('/admin');
  const exportPanel = page.locator('section', { has: page.getByRole('heading', { name: 'Signed audit export' }) });
  await expect(exportPanel).toContainText('private operator evidence');
  await expect(exportPanel).toContainText('never publish');
  await noScriptsNoOverflow(page);
});
