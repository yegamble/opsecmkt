import { expect, test } from '@playwright/test';
import { waitForStyles } from './styles';

// A-545: the user directory on /admin/users and an account's page (/admin/users?handle=), JavaScript off. The
// read-only preview proves layout and semantics only; the database journey is in db-admin.spec.ts.
for (const width of [320, 1280]) {
  test(`user directory and account page at ${width}px: search form, table and actions without sideways scroll`, async ({ page }) => {
    await page.setViewportSize({ width, height: 900 });
    expect((await page.goto('/admin/users'))?.status()).toBe(200);
    await waitForStyles(page);
    const directory = page.getByRole('region', { name: 'User directory' }).first();
    const search = page.getByRole('search', { name: 'Search and filter accounts' });
    await expect(search.getByLabel('Handle contains')).toBeVisible();
    for (const [name, values] of [['role', ['', 'buyer', 'vendor', 'moderator', 'admin']], ['status', ['', 'active', 'suspended', 'banned', 'closed']],
      ['vendor', ['', 'awaiting_bond', 'awaiting_approval', 'approved', 'verified']]] as const) {
      const select = search.locator(`select[name="${name}"]`);
      await expect(select).toHaveAccessibleName(new RegExp(`^${name}`, 'i')); // its wrapping label names it
      expect(await select.locator('option').evaluateAll(options => options.map(o => (o as HTMLOptionElement).value))).toEqual(values);
    }
    await expect(search.getByRole('button', { name: 'Search' })).toBeVisible();
    await expect(directory.getByRole('link', { name: 'ghost_circuit' })).toHaveAttribute('href', '/admin/users?handle=ghost_circuit');
    await expect(directory.locator('tbody tr').first()).toContainText('Approved vendor, verified');
    expect(await page.evaluate(() => document.documentElement.scrollWidth), `directory at ${width}px`).toBeLessThanOrEqual(width);
    for (const field of await search.locator('input, select, button').all()) {
      const box = (await field.boundingBox())!;
      expect(box.x + box.width, `${await field.getAttribute('name')} at ${width}px`).toBeLessThanOrEqual(width);
      // A 44px target for the text field and button; WebKit draws every native select of the site at its own height.
      if (await field.evaluate(element => element.tagName !== 'SELECT')) expect(box.height).toBeGreaterThanOrEqual(43.9);
    }

    // The account page: details, counts and the existing actions, each a plain form.
    await directory.getByRole('link', { name: 'ghost_circuit' }).click();
    await expect(page).toHaveURL(/\/admin\/users\?handle=ghost_circuit$/);
    await waitForStyles(page);
    await expect(page.getByRole('heading', { level: 2, name: 'ghost_circuit' })).toBeVisible();
    await expect(page.getByRole('link', { name: '← All users' })).toHaveAttribute('href', '/admin/users');
    await expect(page.getByRole('link', { name: /Message user/ })).toHaveAttribute('href', '/messages?to=ghost_circuit');
    await expect(page.getByRole('table', { name: 'Orders by state' }).locator('tbody tr').last()).toHaveText(/Total\s*1\s*17/);
    await expect(page.getByRole('table', { name: "Disputes on this account's orders" })).toContainText('Opened against this account');
    for (const button of ['Suspend ghost_circuit', 'Ban ghost_circuit', 'Update role of ghost_circuit', 'Remove the verified mark']) {
      await expect(page.getByRole('button', { name: button })).toBeVisible();
    }
    await expect(page.locator('main form[method="post"] input[name="from"][value="user"]')).toHaveCount(await page.locator('main form[method="post"]').count());
    expect(await page.evaluate(() => document.documentElement.scrollWidth), `account page at ${width}px`).toBeLessThanOrEqual(width);
    await expect(page.locator('script')).toHaveCount(0);
  });
}

test('an unknown account page is not found', async ({ page }) => {
  expect((await page.goto('/admin/users?handle=nobody_here'))?.status()).toBe(404);
});
