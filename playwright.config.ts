import { defineConfig } from '@playwright/test';

const previewURL = 'http://127.0.0.1:18080';
const databaseURL = process.env.E2E_DATABASE_URL;
const databaseUse = { browserName: 'chromium' as const, baseURL: 'http://127.0.0.1:18081', viewport: { width: 390, height: 844 } };
if (process.env.CI && !databaseURL) {
  throw new Error('CI requires E2E_DATABASE_URL pointing to a fresh, dedicated test database');
}

export default defineConfig({
  testDir: './tests/e2e',
  fullyParallel: true,
  forbidOnly: !!process.env.CI,
  retries: process.env.CI ? 1 : 0,
  workers: 2,
  reporter: [['list'], ['html', { open: 'never' }]],
  use: {
    baseURL: previewURL,
    javaScriptEnabled: false,
    trace: 'retain-on-failure',
    screenshot: 'only-on-failure',
  },
  projects: [
    ...[
      { name: 'chromium-mobile-320', browserName: 'chromium' as const, width: 320 },
      { name: 'chromium-mobile-390', browserName: 'chromium' as const, width: 390 },
      { name: 'chromium-tablet', browserName: 'chromium' as const, width: 768 },
      { name: 'chromium-desktop', browserName: 'chromium' as const, width: 1280 },
      { name: 'firefox-mobile', browserName: 'firefox' as const, width: 390 },
      { name: 'webkit-mobile', browserName: 'webkit' as const, width: 390 },
    ].map(({ name, browserName, width }) => ({
      name,
      testMatch: /(^|\/)preview[^/]*\.spec\.ts$/,
      use: { browserName, viewport: { width, height: 900 } },
    })),
    // db-setup initializes the fresh database exactly once; every other db-*.spec.ts depends on it and
    // creates its own uniquely named users/listings (tests/e2e/db-fixtures.ts). The db files still share one
    // admin account, the global CAPTCHA setting and the per-handle login limit, so each database project runs
    // on a single worker (per-project workers, Playwright >= 1.52); preview projects keep the global pool.
    ...(databaseURL ? [{
      name: 'db-setup',
      retries: 0,
      workers: 1,
      testMatch: /(^|\/)db-setup\.spec\.ts$/,
      use: databaseUse,
    }, {
      name: 'database-chromium',
      retries: 0,
      workers: 1,
      fullyParallel: false,
      dependencies: ['db-setup'],
      testMatch: /(^|\/)db-(?!setup)[^/]*\.spec\.ts$/,
      use: databaseUse,
    }] : []),
  ],
  webServer: [
    {
      command: 'go run ./cmd/server -preview',
      env: { PREVIEW_ADDR: '127.0.0.1:18080' },
      url: previewURL,
      reuseExistingServer: false,
      timeout: 120_000,
    },
    ...(databaseURL ? [{
      command: 'go run ./cmd/server',
      env: {
        ADDR: '127.0.0.1:18081', DATABASE_URL: databaseURL,
        COOKIE_SECURE: 'false', APP_MODE: 'clearnet',
        SETUP_TOKEN: 'e2e-local-only-setup-token-at-least-32-characters',
        // Never connect disposable browser fixtures to wallets inherited from the developer's shell.
        BITCOIN_RPC_URL: '', MONERO_RPC_URL: '', MONERO_WALLET_RPC_URL: '',
      },
      url: 'http://127.0.0.1:18081/healthz',
      reuseExistingServer: false,
      timeout: 120_000,
    }] : []),
  ],
});
