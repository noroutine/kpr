import { CONSOLE_URL, expect, expectNoErrors, test } from './fixtures';

/**
 * An operator opening the management console gets the live dashboard.
 * The page titles itself and answers its JSON endpoints; the suite
 * fails the test on any page or console error, so a silently dead
 * dashboard reads red.
 */
test('console dashboard renders without errors', async ({ page, pageErrors }) => {
  const resp = await page.goto(CONSOLE_URL + '/');
  expect(resp?.ok(), 'console root unhealthy').toBeTruthy();
  await expect(page).toHaveTitle(/Management console/);
  expectNoErrors(pageErrors);
});

/**
 * Scrapers and the SPA poll /metrics for the request count and the
 * build identity. A missing or misshapen body breaks dashboards and
 * the console's own SERVERS cards.
 */
test('console metrics answer JSON', async ({ request }) => {
  const resp = await request.get(CONSOLE_URL + '/metrics');
  expect(resp.ok(), 'metrics unhealthy').toBeTruthy();
  const body = await resp.json();
  expect(body, 'metrics shape').toHaveProperty('requests');
});

/**
 * The dashboard's keeper cards come from /api/overview: the registry
 * and store standings the operator reads first. With no registry
 * alongside the suite they render degraded, but the endpoint still
 * answers and still names both cards.
 */
test('console overview names the keeper cards', async ({ request }) => {
  const resp = await request.get(CONSOLE_URL + '/api/overview');
  expect(resp.ok(), 'overview unhealthy').toBeTruthy();
  const body = await resp.json();
  expect(body, 'overview shape').toHaveProperty('registry');
  expect(body, 'overview shape').toHaveProperty('store');
});
