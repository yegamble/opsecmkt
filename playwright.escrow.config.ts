import { defineConfig } from '@playwright/test';

// The Bitcoin escrow no-JavaScript paste journeys (WR-escrow Phase 1, A-246, tests/e2e/escrow-journey.spec.ts; Phase 2,
// A-247, tests/e2e/escrow-physical-journey.spec.ts, run after it on the same database): the real server and PostgreSQL
// against the SIMULATED escrow node (tests/e2e/fixtures/escrow_rpc.py, served by wallet-rpc.py --escrow-port). Chromium
// only, 320 px, JavaScript disabled. Simulated RPC: not chain evidence.
const databaseURL = process.env.E2E_ESCROW_DATABASE_URL;
if (!databaseURL) throw new Error('E2E_ESCROW_DATABASE_URL must point to a fresh dedicated test database');
const app = 18090, pooled = 18091, escrow = 18092;
process.env.E2E_ESCROW_RPC = `http://127.0.0.1:${escrow}`;
export default defineConfig({
  testDir: './tests/e2e', testMatch: /escrow(-physical)?-journey\.spec\.ts$/, workers: 1, retries: 0,
  forbidOnly: !!process.env.CI,
  outputDir: 'test-results-escrow',
  reporter: [['list'], ['html', { outputFolder: 'playwright-escrow-report', open: 'never' }]],
  timeout: 240_000, expect: { timeout: 15_000 },
  use: { browserName: 'chromium', baseURL: `http://127.0.0.1:${app}`, javaScriptEnabled: false,
    viewport: { width: 320, height: 800 }, trace: 'retain-on-failure', screenshot: 'only-on-failure' },
  webServer: [
    { command: `python3 tests/e2e/fixtures/wallet-rpc.py --port ${pooled} --escrow-port ${escrow}`, url: `http://127.0.0.1:${escrow}/health`, reuseExistingServer: false },
    { command: 'go run ./cmd/server', url: `http://127.0.0.1:${app}/healthz`, reuseExistingServer: false, timeout: 120_000,
      env: { ADDR: `127.0.0.1:${app}`, DATABASE_URL: databaseURL, COOKIE_SECURE: 'false', APP_MODE: 'clearnet',
        SETUP_TOKEN: 'e2e-escrow-local-setup-token-at-least-32-characters',
        BITCOIN_RPC_URL: `http://127.0.0.1:${pooled}`, BITCOIN_CHAIN: 'regtest', BITCOIN_WALLET: 'opsecmkt',
        ESCROW_BTC: 'on', ESCROW_BTC_RPC_URL: `http://escrow:simulated-escrow-node@127.0.0.1:${escrow}`,
        ESCROW_BTC_ARBITER_WALLET: 'opsecmkt-arbiter', ESCROW_BTC_WATCH_WALLET: 'opsecmkt-escrow-watch',
        ESCROW_BTC_FEE_BPS: '', ESCROW_BTC_FEE_ADDRESS: '', ESCROW_BTC_FEE_RATE: '',
        MONERO_RPC_URL: `http://127.0.0.1:${pooled}`, MONERO_WALLET_RPC_URL: `http://127.0.0.1:${pooled}`, MONERO_NETWORK: 'stagenet',
        PAYMENT_CONFIRMATIONS_BTC: '3', PAYMENT_CONFIRMATIONS_XMR: '3', PAYMENT_POLL_INTERVAL: '1s',
        PAYMENT_EXPIRY: '24h', AUDIT_SIGNING_KEY: '' } },
  ],
});
