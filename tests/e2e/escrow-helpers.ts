import { expect, type Browser, type Locator, type Page, type TestInfo } from '@playwright/test';
import { waitForStyles } from './styles';

// Shared keyboard, viewport and session helpers for the Monero escrow journey.
export const password = 'escrow-browser-admin-password-123';
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
// Types text into a field reached with Tab; long content is inserted as one paste.
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
export const settlementStatus = (page: Page) => page.locator('.escrow-settlement [role="status"]').first();
