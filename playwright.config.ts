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
  // Database projects share one administrator and global market settings. A
  // per-project limit does not stop Chromium and Firefox from racing each other.
  // Serialize the shared database run; preview-only runs keep two workers.
  workers: databaseURL ? 1 : 2,
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
    // admin account, global settings and the per-handle login limit, so database
    // projects and the global pool use one worker when this shared database is enabled.
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
    }, {
      // A-192 (ruling C3): photo uploads in Firefox with JavaScript off, where an upload answered before its body
      // was read shows a connection reset instead of the page. Only the @firefox tests of db-listing-photos.spec.ts
      // and db-listing-form.spec.ts (A-59: a refused listing form keeps the typed text in Tor Browser's engine).
      // Alone (on a fresh database; db-setup runs first): npx playwright test --project=database-firefox
      name: 'database-firefox',
      retries: 0,
      workers: 1,
      fullyParallel: false,
      dependencies: ['db-setup'],
      testMatch: /(^|\/)db-listing-(photos|form)\.spec\.ts$/,
      grep: /@firefox/,
      use: { ...databaseUse, browserName: 'firefox' as const },
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
    // A loopback fake clamd (not a malware scanner) whose signatures are two days old, so the image scanner is
    // Ready; it starts before the app so the app's first poll finds it.
    ...(databaseURL ? [{
      command: 'python3 tests/e2e/fixtures/fake-clamd.py',
      env: { FAKE_CLAMD_PORT: '18086', FAKE_CLAMD_AGE_DAYS: '2' },
      port: 18086,
      reuseExistingServer: false,
    }, {
      command: 'go run ./cmd/server',
      env: {
        ADDR: '127.0.0.1:18081', DATABASE_URL: databaseURL,
        COOKIE_SECURE: 'false', APP_MODE: 'clearnet',
        RETENTION_ENABLED: process.env.E2E_RETENTION_STATE_DIR ? 'on' : 'off',
        RETENTION_STATE_DIR: process.env.E2E_RETENTION_STATE_DIR ?? '',
        SETUP_TOKEN: 'e2e-local-only-setup-token-at-least-32-characters',
        // Never connect disposable browser fixtures to wallets inherited from the developer's shell.
        BITCOIN_RPC_URL: '', BITCOIN_WALLET: '', MONERO_RPC_URL: '', MONERO_WALLET_RPC_URL: '',
        ESCROW_BTC: '', ESCROW_BTC_RPC_URL: '', ESCROW_BTC_ARBITER_WALLET: '', ESCROW_BTC_WATCH_WALLET: '',
        ESCROW_BTC_FEE_BPS: '', ESCROW_BTC_FEE_ADDRESS: '', ESCROW_BTC_FEE_RATE: '',
        ESCROW_XMR: '', ESCROW_XMR_DAEMON_URL: '', ESCROW_XMR_WALLET_RPC_URL: '',
        CLAMD_ADDRESS: '127.0.0.1:18086', SCANNER_MAX_SIGNATURE_AGE_DAYS: '30',
      },
      url: 'http://127.0.0.1:18081/healthz',
      reuseExistingServer: false,
      timeout: 120_000,
    }] : []),
  ],
});
