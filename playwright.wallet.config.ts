import { defineConfig } from '@playwright/test';

const databaseURL = process.env.E2E_WALLET_DATABASE_URL;
if (!databaseURL) throw new Error('E2E_WALLET_DATABASE_URL must point to a fresh dedicated test database');
export default defineConfig({
  testDir: './tests/e2e', testMatch: /wallet-journey\.spec\.ts$/, workers: 1, retries: 0,
  forbidOnly: !!process.env.CI,
  outputDir: 'test-results-wallet',
  reporter: [['list'], ['html', { outputFolder: 'playwright-wallet-report', open: 'never' }]],
  timeout: 90_000, expect: { timeout: 15_000 },
  use: { browserName: 'chromium', baseURL: 'http://127.0.0.1:18082', javaScriptEnabled: false,
    viewport: { width: 390, height: 844 }, trace: 'retain-on-failure', screenshot: 'only-on-failure' },
  webServer: [
    { command: 'python3 tests/e2e/fixtures/wallet-rpc.py', url: 'http://127.0.0.1:18083/health', reuseExistingServer: false },
    { command: 'go run ./cmd/server', url: 'http://127.0.0.1:18082/healthz', reuseExistingServer: false, timeout: 120_000,
      env: { ADDR: '127.0.0.1:18082', DATABASE_URL: databaseURL, COOKIE_SECURE: 'false', APP_MODE: 'clearnet',
        SETUP_TOKEN: 'e2e-wallet-local-setup-token-at-least-32-characters',
        BITCOIN_RPC_URL: 'http://127.0.0.1:18083', BITCOIN_CHAIN: 'regtest', BITCOIN_WALLET: 'opsecmkt',
        MONERO_RPC_URL: 'http://127.0.0.1:18083', MONERO_WALLET_RPC_URL: 'http://127.0.0.1:18083', MONERO_NETWORK: 'stagenet',
        PAYMENT_CONFIRMATIONS_BTC: '3', PAYMENT_CONFIRMATIONS_XMR: '3', PAYMENT_POLL_INTERVAL: '1s',
        PAYMENT_EXPIRY: '24h', AUDIT_SIGNING_KEY: '' } },
  ],
});
