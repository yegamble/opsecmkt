import { readFileSync } from 'node:fs';
import { expect, type APIRequestContext, type Browser, type Locator, type Page, type TestInfo } from '@playwright/test';
import { waitForStyles } from './styles';

// Shared helpers of the Bitcoin escrow no-JavaScript journeys (tests/e2e/escrow-journey.spec.ts, Phase 1, and
// tests/e2e/escrow-physical-journey.spec.ts, Phase 2): every form is driven by the keyboard alone at 320 px against the
// real server and PostgreSQL, and the escrow node is SIMULATED (tests/e2e/fixtures/escrow_rpc.py): not chain evidence.
export const password = 'escrow-browser-admin-password-123';
export const buyerPassword = 'escrow-browser-buyer-password-123';
export const vendorPassword = 'escrow-browser-vendor-password-123';
export const sim = process.env.E2E_ESCROW_RPC ?? 'http://127.0.0.1:18092';
export const width = 320;

// Presses Tab until target has the focus (a keyboard user reaching a field or button).
export async function tabTo(page: Page, target: Locator) {
  await expect(target).toHaveCount(1);
  for (let i = 0; i < 400; i++) {
    if (await target.evaluate(el => el === document.activeElement)) return;
    await page.keyboard.press('Tab');
  }
  throw new Error('not reachable with the Tab key');
}
// Types text into a field reached with Tab. A PSBT is inserted as one paste (keyboard.insertText).
export async function typeInto(page: Page, field: Locator, text: string) {
  await tabTo(page, field);
  if (text.length > 120) await page.keyboard.insertText(text);
  else await page.keyboard.type(text);
}
// Reaches a submit button with Tab and presses Enter; resolves with the HTTP status of the page it loads.
export async function pressSubmit(page: Page, button: Locator): Promise<number> {
  await tabTo(page, button);
  const [response] = await Promise.all([
    page.waitForResponse(r => r.request().method() === 'POST' && r.request().isNavigationRequest()),
    page.waitForEvent('load'),
    page.keyboard.press('Enter'),
  ]);
  return response.status();
}
export async function check(page: Page, box: Locator) {
  await tabTo(page, box);
  await page.keyboard.press('Space');
  await expect(box).toBeChecked();
}
export async function simPost(request: APIRequestContext, path: string, data: object): Promise<any> {
  const r = await request.post(`${sim}${path}`, { data });
  expect(r.ok(), `${path}: ${await r.text()}`).toBeTruthy();
  return r.json();
}
export async function reloadUntil(page: Page, assertion: () => Promise<void>) {
  await expect(async () => { await page.reload(); await assertion(); }).toPass({ timeout: 30_000, intervals: [300, 700, 1000] });
}
// The page at 320 px: no horizontal scrolling, and a screenshot for the record.
export async function shot(page: Page, info: TestInfo, name: string) {
  await waitForStyles(page);
  expect(await page.evaluate(() => document.documentElement.scrollWidth), `${name} at ${width}px`).toBeLessThanOrEqual(width);
  await page.screenshot({ path: info.outputPath(`${name}.png`), fullPage: true });
}
export async function context(browser: Browser, baseURL: string | undefined): Promise<Page> {
  const ctx = await browser.newContext({ baseURL, javaScriptEnabled: false, viewport: { width, height: 800 } });
  return ctx.newPage();
}
export async function register(page: Page, handle: string, pass: string) {
  await page.goto('/register');
  await typeInto(page, page.getByLabel('Handle', { exact: true }), handle);
  await typeInto(page, page.getByLabel('Password', { exact: true }), pass);
  await pressSubmit(page, page.getByRole('button', { name: 'Create account' }));
  await expect(page.locator('.account-name')).toHaveText(handle);
}
// Registers the party's escrow key and activates it by signing the practice transaction (in the simulated wallet).
export async function activateKey(page: Page, request: APIRequestContext, name: string, pass: string, info: TestInfo) {
  const { key } = await simPost(request, '/test/escrow/key', { name });
  await page.goto('/escrow-key');
  await typeInto(page, page.getByLabel('Extended public key with its origin'), key);
  await typeInto(page, page.getByLabel('Current password'), pass);
  expect(await pressSubmit(page, page.getByRole('button', { name: 'Register key' }))).toBe(303);
  await expect(page.locator('.page-head .badge')).toHaveText('Inactive');
  await pressSubmitGet(page, page.getByRole('button', { name: 'Show the practice transaction' }));
  const practice = (await page.locator('#practice textarea[readonly]').inputValue()).trim();
  expect(practice).toMatch(/^cHNidP8/);
  await shot(page, info, `${name}-01-practice`);
  const { psbt } = await simPost(request, '/test/escrow/sign', { psbt: practice, fingerprint: key.slice(1, 9), sighash: 1 });
  await typeInto(page, page.getByLabel('Signed practice PSBT'), psbt);
  expect(await pressSubmit(page, page.getByRole('button', { name: 'Check signature' }))).toBe(303);
  await expect(page.locator('.page-head .badge')).toHaveText('Active');
  await shot(page, info, `${name}-02-key-active`);
  return key;
}
// A GET form submitted from the keyboard.
export async function pressSubmitGet(page: Page, button: Locator) {
  await tabTo(page, button);
  await Promise.all([page.waitForEvent('load'), page.keyboard.press('Enter')]);
}
// Downloads the reader's copy of the settlement with the keyboard and returns it.
export async function downloadCopy(page: Page): Promise<string> {
  await tabTo(page, page.getByRole('link', { name: 'Download your copy' }));
  const [download] = await Promise.all([page.waitForEvent('download'), page.keyboard.press('Enter')]);
  return readFileSync((await download.path())!, 'utf8').trim();
}
export const settlementStatus = (page: Page) => page.locator('.escrow-settlement [role="status"]').first();
