import { expect, test as base } from '@playwright/test';

// Single-sourced from the Playwright config: the app side rides
// baseURL, the console needs an absolute origin.
import { consoleURL as CONSOLE_URL } from '../playwright.config';

export { CONSOLE_URL };

// Every spec fails its test on any page or console error, even
// when the visible assertions passed: a silently dead page
// (no hydration, swallowed handler) must read red, never green.
export const test = base.extend<{ pageErrors: string[] }>({
  pageErrors: async ({ page }, use) => {
    const errors: string[] = [];
    page.on('pageerror', (err) => errors.push(`pageerror: ${err.message}`));
    page.on('console', (msg) => {
      if (msg.type() === 'error') errors.push(`console: ${msg.text()}`);
    });
    await use(errors);
  },
});

export function expectNoErrors(errors: string[]) {
  expect(errors, 'unexpected page/console errors').toEqual([]);
}

export { expect };
