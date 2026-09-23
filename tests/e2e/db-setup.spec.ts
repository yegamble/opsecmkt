import { expect, test } from '@playwright/test';
import { ADMIN, FIXTURE_LISTING, SETUP_TOKEN } from './db-fixtures';

// Runs once against the fresh database before every other db-*.spec.ts. Never point E2E_DATABASE_URL at production.
test('initialize the market and publish the fixture listing', async ({ page }) => {
  await page.goto('/setup');
  await page.getByLabel('Setup token').fill(SETUP_TOKEN);
  await page.getByLabel('Handle', { exact: true }).fill(ADMIN.handle);
  await page.getByLabel('Password', { exact: true }).fill(ADMIN.password);
  await page.getByRole('button', { name: 'Initialize marketplace' }).click();
  await expect(page.locator('.account-name')).toHaveText(ADMIN.handle);
  await page.goto('/vendor-dashboard');
  await page.getByText('+ Publish a new listing', { exact: true }).click();
  await page.getByLabel('Listing title').fill(FIXTURE_LISTING);
  await page.getByLabel('Description', { exact: true }).fill('Deterministic browser regression fixture. Unfunded drafts only.');
  await page.getByLabel('Bitcoin reference price').fill('0.00100000');
  await page.getByLabel('Monero reference price').fill('0.500000000000');
  await page.getByRole('button', { name: 'Publish listing' }).click();
  await expect(page.locator('.listings-grid')).toContainText(FIXTURE_LISTING);
});
