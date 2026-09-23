import { defineConfig } from '@playwright/test';
import { execFileSync } from 'node:child_process';

const databaseURL = process.env.E2E_CHAIN_DATABASE_URL;
if (!databaseURL) throw new Error('E2E_CHAIN_DATABASE_URL must point to a fresh dedicated local-chain test database');
// The helper refuses public/non-loopback nodes and verifies the isolated chain before exposing configuration.
const chainEnv = JSON.parse(execFileSync('python3', ['tests/e2e/fixtures/local-chain.py', 'env'], { encoding: 'utf8' }));
export default defineConfig({
  testDir: './tests/e2e', testMatch: /chain-journey\.spec\.ts$/, workers: 1, retries: 0,
  forbidOnly: !!process.env.CI, timeout: 300_000, expect: { timeout: 30_000 },
  outputDir: 'artifacts/real-chain/browser-results',
  reporter: [['list'], ['html', { outputFolder: 'artifacts/real-chain/browser-report', open: 'never' }]],
  use: { browserName: 'chromium', baseURL: 'http://127.0.0.1:18122', javaScriptEnabled: false,
    viewport: { width: 390, height: 844 }, actionTimeout: 30_000, navigationTimeout: 30_000, trace: 'retain-on-failure', screenshot: 'only-on-failure' },
  webServer: { command: 'go run ./cmd/server', url: 'http://127.0.0.1:18122/healthz', reuseExistingServer: false,
    timeout: 120_000, env: { ...chainEnv, ADDR: '127.0.0.1:18122', DATABASE_URL: databaseURL,
      COOKIE_SECURE: 'false', APP_MODE: 'clearnet', SETUP_TOKEN: 'local-chain-browser-test-setup-token-at-least-32',
      PAYMENT_CONFIRMATIONS_BTC: '3', PAYMENT_CONFIRMATIONS_XMR: '3', PAYMENT_POLL_INTERVAL: '1s',
      PAYMENT_EXPIRY: '24h', AUDIT_SIGNING_KEY: '' } },
});
