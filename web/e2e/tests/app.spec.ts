import { expect, expectNoErrors, test } from './fixtures';

/**
 * A visitor opening the app landing page gets the keeper pitch, not a
 * blank shell. The suite fails the test on any page or console error,
 * so a bundle that never hydrates reads red.
 */
test('app landing renders without errors', async ({ page, pageErrors }) => {
  const resp = await page.goto('/');
  expect(resp?.ok(), 'app root unhealthy').toBeTruthy();
  await expect(page).toHaveTitle(/kpr/);
  await expect(page.getByRole('heading', { name: 'kpr' })).toBeVisible();
  expectNoErrors(pageErrors);
});

/**
 * The landing page polls /api/status for the keeper state it badges.
 * A missing or misshapen body leaves the badge stuck on loading, so
 * the test pins the shape, not just the status code.
 */
test('app status answers JSON', async ({ request }) => {
  const resp = await request.get('/api/status');
  expect(resp.ok(), 'status unhealthy').toBeTruthy();
  const body = await resp.json();
  expect(body, 'status shape').toHaveProperty('status');
  expect(body, 'status shape').toHaveProperty('store');
});
