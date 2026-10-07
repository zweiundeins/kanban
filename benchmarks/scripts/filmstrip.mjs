// A cold load of the board as a filmstrip: the frames Chrome painted, on the
// same slow phone as compare-bench.mjs (behind cmd/netem, CPU 4x slower).
//
//   node filmstrip.mjs ours   http://127.0.0.1:19000 out/ [seconds]
//   node filmstrip.mjs planka http://127.0.0.1:19001 out/ [seconds]
//
// Writes out/<app>-<ms>.jpg for 0.5, 1, 2, 4, 8 and 16 s after navigation
// start (the last frame painted by then) and out/<app>-frames.json.
import { mkdirSync, writeFileSync } from 'node:fs'
import { chromium } from 'playwright-core'

const [app, base, out = 'filmstrip/', secondsArg] = process.argv.slice(2)
const marks = [500, 1000, 2000, 4000, 8000, 16000]
const seconds = +(secondsArg || (app === 'planka' ? 18 : 5))
mkdirSync(out, { recursive: true })

const browser = await chromium.launch({ executablePath: process.env.CHROME || '/usr/bin/chromium' })
const login = await browser.newContext({ viewport: { width: 1280, height: 860 } })
const page = await login.newPage()
let board = '/boards/2'
await page.goto(base + '/login')
if (app === 'planka') {
	await page.fill('input[name=emailOrUsername]', 'anna@example.com')
	await page.fill('input[name=password]', 'kanban-demo-2026')
	await Promise.all([page.waitForURL(base + '/'), page.keyboard.press('Enter')])
	await page.goto(base + (await page.locator('a[href^="/boards/"]').first().getAttribute('href')))
	await page.waitForSelector('a[href^="/boards/"][title="Release 2.0"]', { timeout: 30000 })
	board = await page.getAttribute('a[href^="/boards/"][title="Release 2.0"]', 'href')
} else {
	await page.fill('input[name=email]', 'anna@example.com')
	await page.fill('input[name=password]', 'demo')
	await Promise.all([page.waitForURL(base + '/'), page.click('button[type=submit]')])
}
const state = await login.storageState()
await login.close()

// The cold load: a new context with only the session.
const ctx = await browser.newContext({ viewport: { width: 1280, height: 860 }, storageState: state })
const cold = await ctx.newPage()
const cdp = await ctx.newCDPSession(cold)
await cdp.send('Emulation.setCPUThrottlingRate', { rate: 4 })
await browser.startTracing(cold, { categories: ['disabled-by-default-devtools.screenshot', 'blink.user_timing', 'loading', 'devtools.timeline'] })
cold.goto(base + board).catch(() => {})
await new Promise((r) => setTimeout(r, seconds * 1000))
const trace = JSON.parse((await browser.stopTracing()).toString())
await browser.close()

const events = trace.traceEvents
const nav = events.find((e) => e.name === 'navigationStart' && e.args?.data?.isLoadingMainFrame && e.args.data.documentLoaderURL?.includes('/boards/'))
	?? events.find((e) => e.name === 'navigationStart')
const shots = events.filter((e) => e.name === 'Screenshot' && e.args?.snapshot).sort((a, b) => a.ts - b.ts)
const frames = []
for (const ms of marks) {
	const at = nav.ts + ms * 1000
	const shot = shots.filter((s) => s.ts <= at).at(-1)
	if (!shot) continue
	const file = `${out}/${app}-${ms}.jpg`
	writeFileSync(file, Buffer.from(shot.args.snapshot, 'base64'))
	frames.push({ ms, painted_ms: Math.round((shot.ts - nav.ts) / 1000), file })
}
writeFileSync(`${out}/${app}-frames.json`, JSON.stringify({ app, base, board, frames, screenshots: shots.length }, null, 2))
console.log(app, frames.map((f) => `${f.ms}: painted ${f.painted_ms} ms`).join(', '))
