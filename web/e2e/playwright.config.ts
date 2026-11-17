import { defineConfig, devices } from '@playwright/test';

// Address book, shared with tests that need an absolute URL
// (the console has no baseURL). Defaults keep the suite off
// the dev ports (8080/9300/9000); override with E2E_HOST,
// E2E_APP_PORT, E2E_CONSOLE_PORT, E2E_RECEIVER_PORT.
export const e2eHost = process.env.E2E_HOST ?? '127.0.0.1';
export const e2eAppPort = process.env.E2E_APP_PORT ?? '18092';
export const e2eConsolePort = process.env.E2E_CONSOLE_PORT ?? '19312';
export const e2eReceiverPort = process.env.E2E_RECEIVER_PORT ?? '19012';
export const consoleURL = `http://${e2eHost}:${e2eConsolePort}`;

const host = e2eHost;
const appPort = e2eAppPort;
const consolePort = e2eConsolePort;
const receiverPort = e2eReceiverPort;

export default defineConfig({
  testDir: './tests',
  // One worker, serial: suites share one server and one store.
  fullyParallel: false,
  workers: 1,
  reporter: [['list']],
  use: {
    baseURL: `http://${host}:${appPort}`,
    screenshot: 'only-on-failure',
  },
  webServer: {
    command: 'go run ./cmd/app serve',
    cwd: '../..',
    url: `http://${host}:${consolePort}/health`,
    reuseExistingServer: !process.env.CI,
    timeout: 180_000,
    env: {
      KPR_APP_HOST: host,
      KPR_APP_PORT: appPort,
      KPR_MANAGEMENT_HOST: host,
      KPR_MANAGEMENT_PORT: consolePort,
      KPR_RECEIVER_HOST: host,
      KPR_RECEIVER_PORT: receiverPort,
      KPR_STORE_DIR: 'web/e2e/.data',
      // No registry alongside the suite: the edge stays shut
      // and the keeper cards render their degraded standings.
      KPR_EDGE: 'false',
    },
  },
  projects: [{ name: 'chromium', use: { ...devices['Desktop Chrome'] } }],
});
