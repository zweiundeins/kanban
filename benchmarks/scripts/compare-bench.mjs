// The same interactions in ours and in PLANKA, on the same data, through
// the same slow network (cmd/netem: 150 ms RTT, 1.6 Mbps down, 750 kbps up)
// with the measured person's CPU slowed 4x, as on a mid-range phone.
//
//   node compare-bench.mjs ours   http://127.0.0.1:19000 [moves]
//   node compare-bench.mjs planka http://127.0.0.1:19001 [moves]
//
// Moves are pointer drags of a card one lane to the right. For each, from
// the drop (pointer up): when the card shows in its new lane, and when the
// server has confirmed it. Ours shows a card only once confirmed, so the
// two are the same moment; PLANKA moves it at once (optimistic) and its
// server confirms over the WebSocket. Also: the move as seen by a second
// person, opening a card, adding a comment, load, INP, CLS, page weight.
// The load is a first visit of a signed-in person: a new browser context
// with the session cookie, so no cache and no open connection.
import { chromium } from 'playwright-core'

const [app, base, movesArg] = process.argv.slice(2)
const moves = +(movesArg || 20)
const sleep = (ms) => new Promise((r) => setTimeout(r, ms))
const pct = (xs, p) => {
	const s = xs.filter((x) => x != null).sort((a, b) => a - b)
	return s.length ? Math.round(s[Math.min(s.length - 1, Math.floor(s.length * p))]) : null
}
const summary = (xs) => ({ n: xs.filter((x) => x != null).length, p50: pct(xs, 0.5), p95: pct(xs, 0.95), max: pct(xs, 1), samples: xs })

const adapters = {
	ours: {
		name: 'kanban (Go, Datastar)',
		password: 'demo',
		board: '/boards/2',
		async login(page, email) {
			await page.goto(base + '/login')
			await page.fill('input[name=email]', email)
			await page.fill('input[name=password]', 'demo')
			await Promise.all([page.waitForURL(base + '/'), page.click('button[type=submit]')])
		},
		lanes: '.lane',
		laneDrop: '.lane-cards',
		card: '.card',
		cardId: (el) => el.id,
		cardSel: (id) => '#' + id,
		laneOf: (id) => `document.getElementById(${JSON.stringify(id)})?.closest('.lane')`,
		confirmedJS: (id) => `!document.querySelector('#pending-style').textContent.includes('#${id}{opacity')`,
		openTarget: (page, i) => page.locator('.lane').nth(2).locator('.card-title').nth(i),
		openedJS: `document.querySelector('#detail')?.isOpen && !!document.querySelector('#detail .detail-title')`,
		close: async (page) => {
			await page.keyboard.press('Escape')
			await page.waitForFunction(() => !document.querySelector('#detail')?.isOpen)
		},
		commentBox: '#detail .comment-new textarea',
		commentSend: '#detail .comment-new button',
		commentShownJS: (text) => `[...document.querySelectorAll('#detail .comment-text')].some((p) => p.textContent === ${JSON.stringify(text)})`,
	},
	planka: {
		name: 'PLANKA Community 2.2.1',
		password: 'kanban-demo-2026',
		board: null, // found on the home page
		async login(page, email) {
			await page.goto(base + '/login')
			await page.fill('input[name=emailOrUsername]', email)
			await page.fill('input[name=password]', 'kanban-demo-2026')
			await Promise.all([page.waitForURL(base + '/'), page.keyboard.press('Enter')])
			if (!this.board) {
				// The home page links the project's first board; its tabs name the others.
				await page.goto(base + (await page.locator('a[href^="/boards/"]').first().getAttribute('href')))
				await page.waitForSelector('a[href^="/boards/"][title="Release 2.0"]', { timeout: 30000 })
				this.board = await page.getAttribute('a[href^="/boards/"][title="Release 2.0"]', 'href')
			}
		},
		lanes: '[data-rbd-droppable-id^="list:"]',
		laneDrop: null,
		card: '[data-rbd-drag-handle-draggable-id]',
		cardId: (el) => el.getAttribute('data-rbd-drag-handle-draggable-id'),
		cardSel: (id) => `[data-rbd-drag-handle-draggable-id="${id}"]`,
		laneOf: (id) => `document.querySelector('[data-rbd-drag-handle-draggable-id="${id}"]')?.closest('[data-rbd-droppable-id^="list:"]')`,
		confirmedJS: null, // the WebSocket answer
		openTarget: (page, i) => page.locator('[data-rbd-droppable-id^="list:"]').nth(2).locator('[data-rbd-drag-handle-draggable-id]').nth(i),
		openedJS: `location.pathname.startsWith('/cards/') && !!document.querySelector('textarea.mentions-input__input')`,
		close: async (page) => {
			await page.keyboard.press('Escape')
			await page.waitForFunction(() => !location.pathname.startsWith('/cards/'))
		},
		commentBox: 'textarea.mentions-input__input',
		commentSend: null, // Ctrl+Enter
		commentShownJS: (text) => `[...document.querySelectorAll('*')].some((n) => n.childElementCount === 0 && n.textContent === ${JSON.stringify(text)})`,
	},
}
const A = adapters[app]
if (!A) throw new Error('app: ours or planka')

// Paint and layout metrics, and when the first card is in the page (the
// first animation frame that finds one), in the page's own clock.
function observe(card) {
	window.__perf = { cls: 0, lcp: 0, inp: 0, cards: null }
	new PerformanceObserver((l) => l.getEntries().forEach((e) => !e.hadRecentInput && (window.__perf.cls += e.value))).observe({ type: 'layout-shift', buffered: true })
	new PerformanceObserver((l) => l.getEntries().forEach((e) => (window.__perf.lcp = e.startTime))).observe({ type: 'largest-contentful-paint', buffered: true })
	new PerformanceObserver((l) => l.getEntries().forEach((e) => e.interactionId && (window.__perf.inp = Math.max(window.__perf.inp, e.duration)))).observe({ type: 'event', durationThreshold: 16, buffered: true })
	const tick = () => (document.querySelector(card) ? (window.__perf.cards = performance.now()) : requestAnimationFrame(tick))
	requestAnimationFrame(tick)
}

async function person(browser, email, throttled) {
	const ctx = await browser.newContext({ viewport: { width: 1280, height: 860 } })
	const page = await ctx.newPage()
	const cdp = await ctx.newCDPSession(page)
	await cdp.send('Network.enable')
	page.acks = new Map() // ack id -> time
	page.sentAt = new Map() // ack id -> {url, t}
	cdp.on('Network.webSocketFrameSent', (e) => {
		const m = /^42(\d+)\["(\w+)",(.*)$/s.exec(e.response.payloadData)
		if (m) page.sentAt.set(m[1], { method: m[2], url: /"url":"([^"]+)"/.exec(m[3])?.[1], t: Date.now() })
	})
	cdp.on('Network.webSocketFrameReceived', (e) => {
		const m = /^43(\d+)\[/.exec(e.response.payloadData)
		if (m) page.acks.set(m[1], Date.now())
	})
	await A.login(page, email)
	if (throttled) await cdp.send('Emulation.setCPUThrottlingRate', { rate: 4 })
	await page.addInitScript(observe, A.card)
	return { ctx, page, cdp }
}

// Resolves with Date.now() when js (an expression) becomes true in the page.
const when = (page, js, timeout = 15000) =>
	page
		.waitForFunction(js, null, { timeout, polling: 'raf' })
		.then(() => Date.now())
		.catch(() => null)

async function ackFor(page, cardId, since, timeout = 15000) {
	const id = cardId.replace(/^card:/, '')
	const start = Date.now()
	while (Date.now() - start < timeout) {
		for (const [ack, s] of page.sentAt) {
			if (s.t >= since && s.method === 'patch' && s.url?.includes(id) && page.acks.has(ack)) return page.acks.get(ack)
		}
		await sleep(2)
	}
	return null
}

const browser = await chromium.launch({ executablePath: process.env.CHROME || '/usr/bin/chromium' })
const a = await person(browser, 'anna@example.com', true)
const b = await person(browser, 'ben@example.com', false)

// Cold load: a new context with only the session (cookies, storage).
const cold = await browser.newContext({ viewport: { width: 1280, height: 860 }, storageState: await a.ctx.storageState() })
const coldPage = await cold.newPage()
const coldCdp = await cold.newCDPSession(coldPage)
await coldCdp.send('Emulation.setCPUThrottlingRate', { rate: 4 })
await coldPage.addInitScript(observe, A.card)
let bytes = 0
coldPage.on('response', async (r) => {
	try {
		const s = await r.request().sizes()
		bytes += s.responseBodySize + s.responseHeadersSize
	} catch {}
})
await coldPage.goto(base + A.board, { waitUntil: 'load' })
await coldPage.waitForFunction(() => window.__perf.cards != null, null, { timeout: 30000 })
await sleep(4000)
const load = {
	transferred_bytes: bytes,
	...(await coldPage.evaluate(() => ({
		ms_to_cards: Math.round(window.__perf.cards),
		ms_to_load_event: Math.round(performance.getEntriesByType('navigation')[0].loadEventEnd),
		lcp_ms: Math.round(window.__perf.lcp),
		cls: +window.__perf.cls.toFixed(4),
	}))),
}
await cold.close()
await a.page.goto(base + A.board)
await a.page.waitForSelector(A.card, { timeout: 30000 })
await b.page.goto(base + A.board)
await b.page.waitForSelector(A.card, { timeout: 30000 })
await sleep(2000)

const shown = []
const misses = []
const confirmed = []
const other = []
for (let i = 0; i < moves; i++) {
	const pick = await a.page.evaluate(
		({ lanes, card }) => {
			const ls = [...document.querySelectorAll(lanes)]
			const from = ls.findIndex((l, i) => i < ls.length - 1 && l.querySelector(card))
			const el = ls[from]?.querySelector(card)
			return el ? { from, id: el.id || el.getAttribute('data-rbd-drag-handle-draggable-id') } : null
		},
		{ lanes: A.lanes, card: A.card },
	)
	if (!pick) break
	const { id, from } = pick
	const src = await a.page.locator(A.cardSel(id)).boundingBox()
	const lane = a.page.locator(A.lanes).nth(from + 1)
	const tgt = await (A.laneDrop ? lane.locator(A.laneDrop) : lane).boundingBox()
	await a.page.mouse.move(src.x + 40, src.y + 15)
	await a.page.mouse.down()
	await a.page.mouse.move(src.x + 50, src.y + 25, { steps: 4 })
	await a.page.mouse.move(tgt.x + 60, tgt.y + Math.min(tgt.height - 10, 60), { steps: 20 })
	await sleep(500) // a slowed CPU needs its frames to settle the drop target
	const inTarget = `${A.laneOf(id)} === document.querySelectorAll(${JSON.stringify(A.lanes)})[${from + 1}]`
	// Every watcher starts before the drop: one started later would add its
	// own start-up time to what it measures.
	const watchShown = when(a.page, inTarget)
	const watchConfirmed = A.confirmedJS && when(a.page, `${inTarget} && ${A.confirmedJS(id)}`)
	const watchOther = when(b.page, inTarget)
	const drop = Date.now()
	await a.page.mouse.up()
	const tShown = await watchShown
	const tConfirmed = A.confirmedJS ? await watchConfirmed : await ackFor(a.page, id, drop)
	const tOther = await watchOther
	if (!tShown) misses.push(id)
	shown.push(tShown && tShown - drop)
	confirmed.push(tConfirmed && tConfirmed - drop)
	other.push(tOther && tOther - drop)
	await sleep(600)
}

const opened = []
for (let i = 0; i < 5; i++) {
	const start = Date.now()
	await A.openTarget(a.page, i).click()
	const t = await when(a.page, A.openedJS)
	opened.push(t && t - start)
	await sleep(500)
	await A.close(a.page)
	await sleep(400)
}

// Comments: on one card, five times.
await A.openTarget(a.page, 0).click()
await when(a.page, A.openedJS)
await sleep(800)
const box = A.commentBox
const commentShown = []
const commentConfirmed = []
for (let i = 0; i < 5; i++) {
	const text = `Benchmark comment ${app} ${Date.now()} ${i}`
	const field = a.page.locator(box).last()
	await field.click()
	await a.page.keyboard.type(text) // PLANKA's mentions field ignores a programmatic fill
	const watch = when(a.page, A.commentShownJS(text))
	const start = Date.now()
	if (A.commentSend) await a.page.click(A.commentSend)
	else await a.page.keyboard.press('Control+Enter')
	const t = await watch
	commentShown.push(t && t - start)
	if (A.confirmedJS) commentConfirmed.push(t && t - start)
	else {
		const s = Date.now()
		let ack = null
		while (Date.now() - s < 10000 && !ack) {
			for (const [id, sent] of a.page.sentAt) if (sent.t >= start && sent.method === 'post' && sent.url?.includes('/comments') && a.page.acks.has(id)) ack = a.page.acks.get(id)
			await sleep(2)
		}
		commentConfirmed.push(ack && ack - start)
	}
	await sleep(500)
}
await A.close(a.page).catch(() => {})
await sleep(1500)
await a.cdp.send('HeapProfiler.collectGarbage')
const heap = await a.cdp.send('Runtime.getHeapUsage')
const after = await a.page.evaluate(() => ({ dom_nodes: document.getElementsByTagName('*').length, inp_ms: Math.round(window.__perf.inp), cls_session: +window.__perf.cls.toFixed(4) }))

console.log(JSON.stringify({
	app: A.name,
	base,
	date: new Date().toISOString(),
	profile: 'cmd/netem 150 ms RTT, 1.6 Mbps down, 750 kbps up; measured person 4x CPU; 1280x860; pointer drags',
	load,
	move_drop_to_shown_ms: summary(shown),
	moves_not_landed: misses.length,
	move_drop_to_confirmed_ms: summary(confirmed),
	move_seen_by_second_person_ms: summary(other),
	open_card_ms: summary(opened),
	comment_to_shown_ms: summary(commentShown),
	comment_to_confirmed_ms: summary(commentConfirmed),
	after_session: { ...after, js_heap_used_bytes: heap.usedSize },
}, null, 2))
await browser.close()
