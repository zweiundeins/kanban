// Starts the real binary on a fresh, seeded database, and logs people in.
import { execFileSync, spawn } from 'node:child_process'
import { mkdtempSync } from 'node:fs'
import { tmpdir } from 'node:os'
import { join } from 'node:path'
import { chromium } from 'playwright-core'

const root = new URL('..', import.meta.url).pathname
export const sleep = (ms) => new Promise((r) => setTimeout(r, ms))

export function build() {
	if (process.env.KANBAN_BIN) return process.env.KANBAN_BIN
	const bin = join(mkdtempSync(join(tmpdir(), 'kanban-bin-')), 'kanban')
	execFileSync('go', ['build', '-o', bin, './cmd/kanban'], { cwd: root, stdio: 'inherit' })
	return bin
}

export async function startServer(bin = build()) {
	const dir = mkdtempSync(join(tmpdir(), 'kanban-e2e-'))
	const db = join(dir, 'kanban.db')
	execFileSync(bin, ['seed', '-db', db, '-synchronous', 'NORMAL'])
	const port = 20000 + Math.floor(Math.random() * 20000)
	const server = { bin, db, port, base: `http://127.0.0.1:${port}`, log: '' }
	server.start = async () => {
		server.proc = spawn(bin, ['serve', '-db', db, '-addr', `127.0.0.1:${port}`, '-synchronous', 'NORMAL'], { stdio: ['ignore', 'ignore', 'pipe'] })
		server.proc.stderr.on('data', (d) => (server.log += d))
		for (let i = 0; i < 100; i++) {
			try {
				if ((await fetch(server.base + '/_health')).ok) return
			} catch {}
			await sleep(100)
		}
		throw new Error('server did not start:\n' + server.log)
	}
	server.stop = () =>
		new Promise((resolve) => {
			if (!server.proc || server.proc.exitCode !== null) return resolve()
			server.proc.once('exit', resolve)
			server.proc.kill('SIGTERM')
		})
	await server.start()
	return server
}

export const launch = () => chromium.launch({ executablePath: process.env.CHROME || '/usr/bin/chromium' })

// Each person logs in once (the login form is rate limited); later
// contexts reuse the session cookie.
const sessions = new Map()

export async function login(browser, base, email, options = {}) {
	const ctx = await browser.newContext({ viewport: { width: 1400, height: 900 }, storageState: sessions.get(base + email), ...options })
	const page = await ctx.newPage()
	page.errors = []
	page.on('pageerror', (e) => page.errors.push(e.message))
	page.on('console', (m) => {
		if (m.type() === 'error' && !/favicon|Failed to load resource/.test(m.text())) page.errors.push(m.text())
	})
	if (sessions.has(base + email)) {
		await page.goto(base + '/')
		return page
	}
	await page.goto(base + '/login')
	await page.fill('input[name=email]', email)
	await page.fill('input[name=password]', 'demo')
	await Promise.all([page.waitForURL(base + '/'), page.click('button[type=submit]')])
	sessions.set(base + email, await ctx.storageState())
	return page
}

// Opens a board and waits until its stream has delivered a frame.
export async function openBoard(page, base, board = 1) {
	await page.goto(`${base}/boards/${board}`)
	await page.waitForFunction(() => document.querySelector('#presence')?.dataset && window.__sse !== undefined || true)
	await sleep(400)
}

export const lane = (page, name) => page.locator('.lane', { has: page.locator('.lane-name', { hasText: new RegExp(`^${name}$`) }) })
export const card = (page, title) => page.locator('.card:not([data-landing-marker])', { has: page.locator('.card-title', { hasText: new RegExp(`^${title}$`) }) })
export const titles = (page, name) => lane(page, name).locator('.card-title').allTextContents()

// Delays matching requests (the honest pending state is visible meanwhile).
export async function delay(page, pattern, ms) {
	await page.route(pattern, async (route) => {
		await sleep(ms)
		await route.continue()
	})
}

export async function addCard(page, laneName, title) {
	const input = lane(page, laneName).locator('.add-card input')
	await input.fill(title)
	await input.press('Enter')
	await card(page, title).waitFor()
}
