// Interaction benchmark on the throttled mobile profile (150 ms RTT,
// 1.6 Mbps down, 750 kbps up, 4x CPU), as in the case-study rules. The
// network part comes from cmd/netem in front of the app (Chrome's own
// emulation doesn't delay data on an open stream or WebSocket); the CPU
// slowdown from Chrome.
//
//   go run ./cmd/netem -listen 127.0.0.1:9000 -to 127.0.0.1:8080 &
//   node interaction-bench.mjs http://127.0.0.1:9000 [moves] > result.json
//
// Measures, for the board page: load (bytes, LCP, CLS); moving a card by
// keyboard from Alt release to the server-confirmed DOM (card in its new
// lane, pending look gone); the same move as seen by a second person;
// opening a card until its dialog shows; adding a comment until it is in
// the dialog; INP over the session; JS heap and DOM nodes after 50 moves.
import { chromium } from 'playwright-core'

const base = process.argv[2] || 'http://127.0.0.1:8080'
const moves = +(process.argv[3] || 20)
const board = +(process.env.BOARD || 2)
const sleep = (ms) => new Promise((r) => setTimeout(r, ms))
const pct = (xs, p) => {
	const s = [...xs].sort((a, b) => a - b)
	return s.length ? Math.round(s[Math.min(s.length - 1, Math.floor(s.length * p))]) : null
}
const summary = (xs) => ({ n: xs.length, p50: pct(xs, 0.5), p95: pct(xs, 0.95), max: pct(xs, 1) })

async function person(browser, email, throttled) {
	const ctx = await browser.newContext({ viewport: { width: 412, height: 915 }, deviceScaleFactor: 2.625, isMobile: false })
	const page = await ctx.newPage()
	await page.goto(base + '/login')
	await page.fill('input[name=email]', email)
	await page.fill('input[name=password]', 'demo')
	await Promise.all([page.waitForURL(base + '/'), page.click('button[type=submit]')])
	const cdp = await ctx.newCDPSession(page)
	if (throttled) await cdp.send('Emulation.setCPUThrottlingRate', { rate: 4 })
	await page.addInitScript(() => {
		window.__perf = { cls: 0, lcp: 0, inp: 0, slow: [] }
		new PerformanceObserver((l) => l.getEntries().forEach((e) => !e.hadRecentInput && (window.__perf.cls += e.value))).observe({ type: 'layout-shift', buffered: true })
		new PerformanceObserver((l) => l.getEntries().forEach((e) => (window.__perf.lcp = e.startTime))).observe({ type: 'largest-contentful-paint', buffered: true })
		new PerformanceObserver((l) => l.getEntries().forEach((e) => {
			if (!e.interactionId) return
			window.__perf.inp = Math.max(window.__perf.inp, e.duration)
			if (e.duration >= 64) window.__perf.slow.push({ name: e.name, ms: e.duration, input: Math.round(e.processingStart - e.startTime), processing: Math.round(e.processingEnd - e.processingStart), target: e.target?.id || e.target?.className || e.target?.tagName })
		})).observe({ type: 'event', durationThreshold: 16, buffered: true })
	})
	return { ctx, page, cdp }
}

const browser = await chromium.launch({ executablePath: process.env.CHROME || '/usr/bin/chromium' })
const a = await person(browser, 'anna@example.com', true)
const b = await person(browser, 'ben@example.com', false)

// Load
let bytes = 0
a.page.on('response', async (r) => {
	try {
		const s = await r.request().sizes()
		bytes += s.responseBodySize + s.responseHeadersSize
	} catch {}
})
const t0 = Date.now()
await a.page.goto(`${base}/boards/${board}`, { waitUntil: 'load' })
const loadMs = Date.now() - t0
await sleep(3000) // the stream's first frame, LCP settles
const load = { ms_to_load_event: loadMs, transferred_bytes: bytes, ...(await a.page.evaluate(() => ({ lcp_ms: Math.round(window.__perf.lcp), cls: +window.__perf.cls.toFixed(4) }))) }
await b.page.goto(`${base}/boards/${board}`)
await sleep(1500)

// Moves: Alt+Right on a card in the first lane, until it is in the second lane and no longer pending.
const confirmed = []
const seenByOther = []
for (let i = 0; i < moves; i++) {
	// A card from the first lane that has one, one lane to the right.
	const { id, from } = await a.page.evaluate(() => {
		const lanes = [...document.querySelectorAll('.lane')]
		const from = lanes.findIndex((l, i) => i < lanes.length - 1 && l.querySelector('.card'))
		return { id: lanes[from]?.querySelector('.card')?.id, from }
	})
	if (!id) break
	await a.page.focus('#' + id)
	await a.page.keyboard.down('Alt')
	await a.page.keyboard.press('ArrowRight')
	const watchA = a.page.evaluate(
		({ id, to }) =>
			new Promise((resolve) => {
				const start = performance.now()
				const check = () => {
					const lane = document.getElementById(id)?.closest('.lane')
					const pending = document.querySelector('#pending-style').textContent.includes('#' + id + '{opacity')
					if (lane === document.querySelectorAll('.lane')[to] && !pending) {
						resolve(performance.now() - start)
						return true
					}
				}
				if (check()) return
				const mo = new MutationObserver(() => check() && mo.disconnect())
				mo.observe(document.body, { subtree: true, childList: true, characterData: true, attributes: true })
			}),
		{ id, to: from + 1 },
	)
	const watchB = b.page.evaluate(
		({ id, to }) =>
			new Promise((resolve) => {
				const start = performance.now()
				const mo = new MutationObserver(() => {
					if (document.getElementById(id)?.closest('.lane') === document.querySelectorAll('.lane')[to]) {
						mo.disconnect()
						resolve(performance.now() - start)
					}
				})
				mo.observe(document.body, { subtree: true, childList: true })
			}),
		{ id, to: from + 1 },
	)
	await a.page.keyboard.up('Alt')
	confirmed.push(await watchA)
	seenByOther.push(await watchB)
	await sleep(300)
}

// Open a card, until the dialog shows it.
const opened = []
for (let i = 0; i < 5; i++) {
	const title = a.page.locator('.lane').nth(2).locator('.card-title').nth(i)
	const ms = a.page.evaluate(() => new Promise((resolve) => {
		const start = performance.now()
		const t = setInterval(() => document.querySelector('#detail')?.isOpen && document.querySelector('#detail .detail-title') && (clearInterval(t), resolve(performance.now() - start)), 5)
	}))
	await title.click()
	opened.push(await ms)
	await a.page.keyboard.press('Escape')
	await a.page.waitForFunction(() => !document.querySelector('#detail')?.isOpen)
	await sleep(300)
}

// Add a comment, until it is in the dialog.
await a.page.locator('.lane').nth(2).locator('.card-title').first().click()
await a.page.waitForFunction(() => document.querySelector('#detail')?.isOpen)
const commented = []
for (let i = 0; i < 5; i++) {
	const text = `Benchmark comment ${Date.now()} ${i}`
	await a.page.fill('#detail .comment-new textarea', text)
	const ms = a.page.evaluate((text) => new Promise((resolve) => {
		const start = performance.now()
		const mo = new MutationObserver(() => [...document.querySelectorAll('#detail .comment-text')].some((p) => p.textContent === text) && (mo.disconnect(), resolve(performance.now() - start)))
		mo.observe(document.body, { subtree: true, childList: true, characterData: true })
	}), text)
	await a.page.click('#detail .comment-new button')
	commented.push(await ms)
	await sleep(300)
}
await a.page.keyboard.press('Escape')

// 50 more moves (unmeasured), then the page's weight.
for (let i = 0; i < 50; i++) {
	const id = await a.page.evaluate(() => {
		const lanes = [...document.querySelectorAll('.lane')]
		return lanes.find((l, i) => i < lanes.length - 1 && l.querySelector('.card'))?.querySelector('.card')?.id
	})
	if (!id) break
	await a.page.focus('#' + id)
	await a.page.keyboard.down('Alt')
	await a.page.keyboard.press('ArrowRight')
	await a.page.keyboard.up('Alt')
	await sleep(150)
}
await sleep(2000)
await a.cdp.send('HeapProfiler.collectGarbage')
const heap = await a.cdp.send('Runtime.getHeapUsage')
const after = await a.page.evaluate(() => ({ dom_nodes: document.getElementsByTagName('*').length, inp_ms: Math.round(window.__perf.inp), slow_interactions: window.__perf.slow, cls_session: +window.__perf.cls.toFixed(4) }))

console.log(JSON.stringify({
	app: 'kanban (Go)',
	base,
	board,
	date: new Date().toISOString(),
	profile: 'cmd/netem 150 ms RTT, 1.6 Mbps down, 750 kbps up; Chrome 4x CPU; 412x915',
	load,
	move_to_confirmed_ms: summary(confirmed),
	move_seen_by_second_person_ms: summary(seenByOther),
	open_card_ms: summary(opened),
	comment_to_visible_ms: summary(commented),
	after_session: { ...after, js_heap_used_bytes: heap.usedSize },
}, null, 2))
await browser.close()
