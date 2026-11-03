import { test, expect, type Page } from '@playwright/test'

const unique = (p: string) => `${p}-${Date.now().toString(36)}-${Math.floor(Math.random() * 1e4)}`

async function createCommandJob(page: Page, name: string, command: string, env: Record<string, string> = {}) {
  await page.goto('#/new')
  await page.getByLabel('Name').fill(name)
  await page.getByLabel('Cron expression').fill('0 0 1 1 *')
  await page.getByRole('group', { name: 'Executor' }).getByRole('button', { name: 'Command' }).click()
  await page.getByLabel('Command', { exact: true }).fill(command)
  for (const [k, v] of Object.entries(env)) {
    const box = page.locator('div.grid', { has: page.getByText('Environment', { exact: true }) }).last()
    await box.getByRole('button', { name: 'Add' }).click()
    await box.getByPlaceholder('NAME').last().fill(k)
    await box.getByPlaceholder('Value').last().fill(v)
  }
  await page.getByRole('button', { name: 'Create job' }).click()
  await expect(page.getByRole('heading', { name, level: 1 })).toBeVisible()
}

test('create a job, run it with params and follow the output live', async ({ page }) => {
  const name = unique('e2e-params')
  await createCommandJob(page, name, 'echo "hello $KRONIK_PARAM_WHO"')

  await page.getByRole('button', { name: 'Run with params' }).click()
  const params = page.locator('section', { hasText: 'Run once with params' })
  await params.getByRole('button', { name: 'Add' }).click()
  await params.getByPlaceholder('name').first().fill('who')
  await params.getByPlaceholder('Value').first().fill('world')
  await page.getByRole('button', { name: 'Run', exact: true }).click()
  await page.getByRole('link', { name: 'View run' }).click()

  await expect(page.locator('pre.term')).toHaveText('hello world', { timeout: 15_000 })
  await expect(page.getByText('succeeded').first()).toBeVisible()
  await expect(page.getByText('who=world')).toBeVisible()
})

test('a secret is used by a job and hidden in its output', async ({ page, request }) => {
  const secret = unique('E2E').replace(/-/g, '_').toUpperCase()
  await page.goto('#/settings/secrets')
  await page.getByRole('button', { name: 'Add a secret' }).click()
  await page.getByLabel('Name').fill(secret)
  await page.getByLabel('Value').fill('very-secret-value')
  await page.getByRole('button', { name: 'Save' }).click()
  await expect(page.getByRole('cell', { name: secret }).first()).toBeVisible()
  await expect(page.getByText('very-secret-value')).toHaveCount(0)

  const name = unique('e2e-secret')
  await createCommandJob(page, name, 'echo "token=$T"', { T: `{{secret.${secret}}}` })
  await page.getByRole('button', { name: 'Run now' }).click()
  await page.getByRole('link', { name: 'View run' }).click()
  await expect(page.locator('pre.term')).toHaveText('token=[secret]', { timeout: 15_000 })

  // Clean up the secret.
  await request.delete(`api/v1/secrets/${secret}`)
})

test('select jobs and pause them together', async ({ page }) => {
  const a = unique('e2e-bulk-a')
  const b = unique('e2e-bulk-b')
  await createCommandJob(page, a, 'true')
  await createCommandJob(page, b, 'true')

  await page.goto(`#/jobs?search=e2e-bulk`)
  await page.getByLabel(`Select ${a}`).check()
  await page.getByLabel(`Select ${b}`).check()
  await expect(page.getByText('2 selected')).toBeVisible()
  await page.getByRole('button', { name: 'Pause', exact: true }).first().click()
  await expect(page.getByText('2 paused')).toBeVisible()
})

test('the calendar lists a job at its time', async ({ page }) => {
  const name = unique('e2e-cal')
  await page.goto('#/new')
  await page.getByLabel('Name').fill(name)
  await page.getByLabel('Cron expression').fill('17 * * * *')
  await page.getByRole('group', { name: 'Executor' }).getByRole('button', { name: 'Command' }).click()
  await page.getByLabel('Command', { exact: true }).fill('true')
  await page.getByRole('button', { name: 'Create job' }).click()
  await expect(page.getByRole('heading', { name, level: 1 })).toBeVisible()

  await page.goto('#/calendar')
  await expect(page.getByRole('link', { name: new RegExp(name) }).first()).toBeVisible()
})

test('settings pages load', async ({ page }) => {
  for (const tab of ['configuration', 'secrets', 'notifications', 'replicas', 'audit', 'mcp', 'about']) {
    await page.goto(`#/settings/${tab}`)
    await expect(page.getByRole('heading', { name: 'Settings', level: 1 })).toBeVisible()
    await expect(page.locator('.alert')).toHaveCount(0)
  }
})
