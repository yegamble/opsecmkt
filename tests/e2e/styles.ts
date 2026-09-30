import { expect, type Page } from '@playwright/test';

// A-228, A-260: a click (or key press) that navigates returns once the new page commits, and an assertion on its
// text can pass while the stylesheets are still loading. Until they apply, the page lays out unstyled: it can be
// wider than the viewport, and both search forms (header and catalog) and the mobile filter panel are visible,
// which no media query allows. Every width, box or visibility measurement that can follow a navigation waits here
// first: the document has loaded (the stylesheets in <head> hold back `load`) and every linked stylesheet is
// parsed. page.goto already waits for `load`, so after one this returns at once.
export async function waitForStyles(page: Page): Promise<void> {
  await page.waitForLoadState('load');
  await expect.poll(() => page.evaluate(() => document.readyState === 'complete' &&
    [...document.querySelectorAll<HTMLLinkElement>('link[rel="stylesheet"]')].every(link => link.sheet !== null)),
  { message: 'every stylesheet of the page applies' }).toBe(true);
}
