import { expect, test } from '@playwright/test';
import { ADMIN, uniqueHandle } from './db-fixtures';
import { setRole, signedIn, submitStatus, withinViewport } from './db-helpers';

// A-59 (PA-6): a refused listing form comes back filled in, with JavaScript disabled, at 320 px. Before, the refusal
// was a separate page and Back in Firefox (Tor Browser's engine) restored an empty form, losing the typed title and
// description. Tagged @firefox, so it also runs in the database-firefox project (playwright.config.ts).
test('a refused listing keeps what the vendor typed, on create and edit @firefox', async ({ browser, baseURL }) => {
  const handle = uniqueHandle('lform');
  const vendor = await signedIn(browser, baseURL, handle, 'browser-lform-password-123', true, 320);
  const admin = await signedIn(browser, baseURL, ADMIN.handle, ADMIN.password, false);
  await setRole(admin, handle, 'vendor');
  await admin.context().close();

  const title = `Typed title ${handle}`;
  const description = 'A description the vendor typed by hand.\nIt has a second line & an ampersand.';
  let form = vendor.locator('form[action="/listings"]');
  await vendor.goto('/vendor-dashboard');
  await vendor.getByText('+ Publish a new listing', { exact: true }).click();
  await form.getByLabel('Listing title').fill(title);
  await form.getByLabel('Description', { exact: true }).fill(description);
  await form.locator('select[name="category"]').selectOption('Services');
  await form.locator('select[name="region"]').selectOption('Europe');
  await form.getByLabel('Stock', { exact: true }).fill('7');
  await form.getByLabel('Price (XMR)').fill('0.0009'); // below the 0.001 XMR floor: refused by the server only
  await vendor.getByText('Additional shipping regions').click();
  await form.getByRole('checkbox', { name: 'Asia', exact: true }).check();
  expect(await submitStatus(vendor, () => vendor.getByRole('button', { name: 'Publish listing ↗' }).click())).toBe(400);

  const priceError = 'Monero price: use at least 0.001 XMR. The network fee is deducted from every payout and would take too large a share of a smaller one.';
  await expect(vendor).toHaveTitle(/^Error: /);
  await expect(vendor.getByRole('alert')).toHaveText(`The listing was not published; nothing was saved. ${priceError}`);
  await expect(form.getByLabel('Listing title')).toHaveValue(title);
  await expect(form.getByLabel('Description', { exact: true })).toHaveValue(description);
  await expect(form.locator('select[name="category"]')).toHaveValue('Services');
  await expect(form.locator('select[name="region"]')).toHaveValue('Europe');
  await expect(form.getByLabel('Stock', { exact: true })).toHaveValue('7');
  await expect(form.getByRole('checkbox', { name: 'Asia', exact: true })).toBeChecked();
  const price = form.getByLabel('Price (XMR)');
  await expect(price).toHaveValue('0.0009');
  await expect(price).toHaveAttribute('aria-invalid', 'true');
  await expect(vendor.locator('#lf-price_xmr-error')).toHaveText(priceError);
  await expect(vendor.locator('.field-error')).toHaveCount(1);
  expect(await vendor.evaluate(() => document.documentElement.scrollWidth)).toBeLessThanOrEqual(320);
  await withinViewport(vendor.locator('#publish-listing'), 320);

  // Corrected and sent again from the re-rendered form (its own CSRF token and form nonce), it publishes.
  await price.fill('0.5');
  expect(await submitStatus(vendor, () => vendor.getByRole('button', { name: 'Publish listing ↗' }).click())).toBe(303);
  await expect(vendor).toHaveURL(/\/listing-edit\?id=[0-9a-f]{64}#photos$/);
  const editURL = vendor.url().replace(/#.*$/, '');
  form = vendor.locator('form[action="/listings/update"]');
  await expect(form.getByLabel('Listing title')).toHaveValue(title);

  // A refused edit keeps the edited values and changes nothing.
  await form.getByLabel('Listing title').fill(`${title} edited`);
  await form.getByLabel('Description', { exact: true }).fill('An edited description.');
  await form.getByLabel('Price (XMR)').fill('1.1234567890123');
  expect(await submitStatus(vendor, () => vendor.getByRole('button', { name: 'Save changes' }).click())).toBe(400);
  await expect(vendor.getByRole('alert')).toHaveText('The changes were not saved. Monero price: use a positive amount with at most 12 decimal places.');
  await expect(form.getByLabel('Listing title')).toHaveValue(`${title} edited`);
  await expect(form.getByLabel('Description', { exact: true })).toHaveValue('An edited description.');
  await expect(form.getByLabel('Price (XMR)')).toHaveValue('1.1234567890123');
  await expect(vendor.locator('#lf-price_xmr-error')).toHaveText('Monero price: use a positive amount with at most 12 decimal places.');
  expect(await vendor.evaluate(() => document.documentElement.scrollWidth)).toBeLessThanOrEqual(320);
  await vendor.goto(editURL);
  await expect(form.getByLabel('Listing title')).toHaveValue(title);
  await expect(form.getByLabel('Price (XMR)')).toHaveValue(/^0\.5/);
  await vendor.context().close();
});
