import assert from 'node:assert/strict'
import { after, before, test } from 'node:test'
import { addCard, card, delay, lane, launch, login, openBoard, sleep, startServer, titles } from './harness.mjs'

let server, browser
before(async () => {
	server = await startServer()
	browser = await launch()
})
after(async () => {
	await browser?.close()
	await server?.stop()
})

const noErrors = (...pages) => {
	for (const p of pages) assert.deepEqual(p.errors, [], 'console errors')
}

test('a card is added only when the server confirms it', async () => {
	const p = await login(browser, server.base, 'anna@example.com')
	await openBoard(p, server.base)
	await delay(p, '**/boards/1/cards', 500)
	const input = lane(p, 'To do').locator('.add-card input')
	await input.fill('Honest card')
	await input.press('Enter')
	await sleep(150)
	assert.equal(await card(p, 'Honest card').count(), 0, 'no card before the server has it')
	assert.equal(await input.getAttribute('readonly'), '', 'the input waits, read-only')
	await card(p, 'Honest card').waitFor()
	await p.waitForFunction(() => document.querySelector('.lane .add-card input').value === '')
	assert.ok((await titles(p, 'To do')).includes('Honest card'))
	noErrors(p)
	await p.context().close()
})

test('a keyboard move is pending until the board shows it', async () => {
	const p = await login(browser, server.base, 'anna@example.com')
	await openBoard(p, server.base)
	await addCard(p, 'To do', 'Keyboard card')
	await delay(p, '**/move', 600)
	const c = card(p, 'Keyboard card')
	await c.focus()
	await p.keyboard.down('Alt')
	await p.keyboard.press('ArrowRight')
	await p.keyboard.up('Alt')
	await sleep(150)
	assert.ok((await p.locator('#pending-style').textContent()).includes(`#${await c.getAttribute('id')}{opacity`), 'marked pending')
	assert.ok((await titles(p, 'To do')).includes('Keyboard card'), 'still where the server put it')
	await p.waitForFunction(() => [...document.querySelectorAll('.lane')][1].textContent.includes('Keyboard card'))
	await p.waitForFunction(() => !document.querySelector('#pending-style').textContent.includes('opacity'))
	assert.match(await p.locator('#status').textContent(), /Moved ‘Keyboard card’ to ‘Doing’/)
	noErrors(p)
	await p.context().close()
})

test('someone else’s change doesn’t end a pending move early', async () => {
	const a = await login(browser, server.base, 'anna@example.com')
	const b = await login(browser, server.base, 'ben@example.com')
	await openBoard(a, server.base)
	await openBoard(b, server.base)
	await addCard(a, 'To do', 'Slow card')
	await delay(a, '**/move', 1200)
	await card(a, 'Slow card').focus()
	await a.keyboard.down('Alt')
	await a.keyboard.press('ArrowRight')
	await a.keyboard.up('Alt')
	await addCard(b, 'Done', 'Unrelated card') // a new frame reaches Anna meanwhile
	await card(a, 'Unrelated card').waitFor()
	const id = await card(a, 'Slow card').getAttribute('id')
	assert.ok((await a.locator('#pending-style').textContent()).includes(`#${id}{opacity`), 'still pending')
	assert.ok((await titles(a, 'To do')).includes('Slow card'), 'still where the server has it')
	await a.waitForFunction(() => [...document.querySelectorAll('.lane')][1].textContent.includes('Slow card'))
	await a.waitForFunction(() => !document.querySelector('#pending-style').textContent.includes('opacity'))
	noErrors(a, b)
	await a.context().close()
	await b.context().close()
})

test('a lane move stays pending while someone else changes that lane', async () => {
	const a = await login(browser, server.base, 'anna@example.com')
	const b = await login(browser, server.base, 'ben@example.com')
	await openBoard(a, server.base)
	await openBoard(b, server.base)
	const names = () => a.locator('.lane-name').allTextContents()
	const before = await names()
	const doing = lane(a, 'Doing')
	const id = await doing.getAttribute('id')
	await delay(a, '**/lists/*/move', 1500)
	await doing.locator('.icon-btn').click()
	await a.locator('.menu:popover-open button', { hasText: 'Move left' }).click()
	await addCard(b, 'Doing', 'Card during a lane move') // the moving lane changes for Anna
	await card(a, 'Card during a lane move').waitFor()
	assert.ok((await a.locator('#pending-style').textContent()).includes(`#${id}{opacity`), 'still pending')
	assert.deepEqual(await names(), before, 'not moved before the server says so')
	await a.waitForFunction((id) => document.querySelector('.lane')?.id === id, id)
	await a.waitForFunction((id) => !document.querySelector('#pending-style').textContent.includes(`#${id}{opacity`), id)
	// Back where it was, for the tests that follow.
	await lane(a, 'Doing').locator('.icon-btn').click()
	await a.locator('.menu:popover-open button', { hasText: 'Move right' }).click()
	await a.waitForFunction((names) => [...document.querySelectorAll('.lane-name')].map((n) => n.textContent).join('|') === names, before.join('|'))
	noErrors(a, b)
	await a.context().close()
	await b.context().close()
})

const laneOrder = (page) => page.locator('.lane-name').allTextContents()
const waitOrder = (page, names) => page.waitForFunction((n) => [...document.querySelectorAll('.lane-name')].map((x) => x.textContent).join('|') === n, names.join('|'))

test('a lane dragged by its grip lands where it was dropped', async () => {
	const p = await login(browser, server.base, 'chiara@example.com')
	await openBoard(p, server.base)
	const before = await laneOrder(p)
	assert.deepEqual(before.slice(0, 3), ['To do', 'Doing', 'Done'])
	await delay(p, '**/lists/*/move', 500)
	const grip = await lane(p, 'Done').locator('.lane-grip').boundingBox()
	const target = await lane(p, 'To do').boundingBox()
	await p.mouse.move(grip.x + grip.width / 2, grip.y + grip.height / 2)
	await p.mouse.down()
	await p.mouse.move(grip.x - 20, grip.y + 10, { steps: 4 })
	await p.mouse.move(target.x + 20, target.y + 40, { steps: 15 })
	await p.mouse.up()
	await sleep(150)
	assert.deepEqual(await laneOrder(p), before, 'nothing moves before the server says so')
	const id = await lane(p, 'Done').getAttribute('id')
	assert.ok((await p.locator('#pending-style').textContent()).includes(`#${id}{opacity`), 'the lane looks pending')
	await waitOrder(p, ['Done', 'To do', 'Doing', ...before.slice(3)])
	// back, with the keyboard: Alt and the arrows on the grip
	await lane(p, 'Done').locator('.lane-grip').focus()
	await p.keyboard.down('Alt')
	await p.keyboard.press('ArrowRight')
	await p.keyboard.press('ArrowRight')
	await p.keyboard.up('Alt')
	await waitOrder(p, before)
	assert.equal(await p.evaluate(() => document.activeElement?.closest('.lane')?.querySelector('.lane-name')?.textContent), 'Done', 'the grip keeps the focus')
	noErrors(p)
	await p.context().close()
})

test('a lane move from a stale page is refused and says why', async () => {
	const a = await login(browser, server.base, 'anna@example.com')
	await openBoard(a, server.base)
	const before = await laneOrder(a)
	const b = await login(browser, server.base, 'ben@example.com')
	let release
	const gate = new Promise((r) => (release = r))
	await b.route('**/live?**', async (route) => {
		await gate
		await route.continue()
	})
	await b.goto(`${server.base}/boards/1`)
	await lane(b, 'Doing').waitFor()
	// Anna moves Doing to the end; Ben, still seeing the old order, moves it left.
	await lane(a, 'Doing').locator('.lane-grip').focus()
	await a.keyboard.down('Alt')
	await a.keyboard.press('ArrowRight')
	await a.keyboard.up('Alt')
	await waitOrder(a, ['To do', 'Done', 'Doing', ...before.slice(3)])
	await lane(b, 'Doing').locator('.icon-btn').click()
	await b.locator('.menu:popover-open button', { hasText: 'Move left' }).click()
	await sleep(300)
	release()
	await b.waitForFunction(() => document.querySelector('sb-toast')?.getAttribute('toasts')?.includes('Anna moved the list'), null, { timeout: 5000 })
	await waitOrder(b, ['To do', 'Done', 'Doing', ...before.slice(3)])
	// back for the tests that follow
	await lane(a, 'Doing').locator('.icon-btn').click()
	await a.locator('.menu:popover-open button', { hasText: 'Move left' }).click()
	await waitOrder(a, before)
	noErrors(a, b)
	await a.context().close()
	await b.context().close()
})

test('a dragged card lands where it was dropped', async () => {
	const p = await login(browser, server.base, 'anna@example.com')
	await openBoard(p, server.base)
	await addCard(p, 'Done', 'Dragged card')
	const src = await card(p, 'Dragged card').boundingBox()
	const tgt = await lane(p, 'To do').locator('.lane-cards').boundingBox()
	await p.mouse.move(src.x + 40, src.y + 15)
	await p.mouse.down()
	await p.mouse.move(src.x + 50, src.y + 25, { steps: 3 })
	await p.mouse.move(tgt.x + 60, tgt.y + tgt.height - 8, { steps: 15 })
	await p.mouse.up()
	await lane(p, 'To do').locator('.card-title', { hasText: /^Dragged card$/ }).waitFor()
	assert.equal((await titles(p, 'To do')).at(-1), 'Dragged card')
	noErrors(p)
	await p.context().close()
})

test('a drop is marked where the card goes until the board shows it there', async () => {
	const p = await login(browser, server.base, 'anna@example.com')
	await openBoard(p, server.base)
	await addCard(p, 'Done', 'Landing card')
	// The page is drawn, but its stream is held back: no frame can move the card.
	let release
	const gate = new Promise((r) => (release = r))
	await p.route('**/live?**', async (route) => {
		await gate
		await route.continue()
	})
	await p.goto(`${server.base}/boards/1`)
	await card(p, 'Landing card').waitFor()
	await p.evaluate(() => document.querySelector('sb-kanban-board').addEventListener('sb-kanban-landing-end', (e) => (window.__landing = e.detail.reason)))
	const src = await card(p, 'Landing card').boundingBox()
	const tgt = await lane(p, 'To do').locator('.lane-cards').boundingBox()
	await p.mouse.move(src.x + 40, src.y + 15)
	await p.mouse.down()
	await p.mouse.move(src.x + 50, src.y + 25, { steps: 3 })
	await p.mouse.move(tgt.x + 60, tgt.y + tgt.height - 8, { steps: 15 })
	await p.mouse.up()
	await sleep(300)
	assert.ok((await titles(p, 'Done')).includes('Landing card'), 'the card stays where the server put it')
	const marker = p.locator('[data-landing-marker="card"]')
	assert.equal(await marker.count(), 1, 'a marker shows where it goes')
	assert.equal(await marker.getAttribute('aria-hidden'), 'true')
	release()
	await lane(p, 'To do').locator('.card-title', { hasText: /^Landing card$/ }).waitFor()
	await marker.waitFor({ state: 'detached', timeout: 5000 })
	assert.equal(await p.evaluate(() => window.__landing), 'arrived')
	noErrors(p)
	await p.context().close()
})

test('on a touch screen the grip drags a card and the card scrolls the board', async () => {
	const p = await login(browser, server.base, 'dev@example.com', { viewport: { width: 390, height: 844 }, isMobile: true, hasTouch: true })
	await openBoard(p, server.base)
	await addCard(p, 'To do', 'Touch card')
	const cdp = await p.context().newCDPSession(p)
	const touch = (type, x, y) => cdp.send('Input.dispatchTouchEvent', { type, touchPoints: type === 'touchEnd' ? [] : [{ x, y }] })
	await card(p, 'Touch card').scrollIntoViewIfNeeded()
	const box = await card(p, 'Touch card').boundingBox()
	const before = await p.evaluate(() => document.querySelector('.board-scroll').scrollLeft)
	await touch('touchStart', box.x + 40, box.y + box.height / 2)
	for (let i = 1; i <= 8; i++) await touch('touchMove', box.x + 40 - i * 25, box.y + box.height / 2)
	await touch('touchEnd')
	await sleep(600)
	assert.ok((await p.evaluate(() => document.querySelector('.board-scroll').scrollLeft)) > before, 'the board scrolled')
	assert.ok((await titles(p, 'To do')).includes('Touch card'), 'the card stayed')
	await p.evaluate(() => (document.querySelector('.board-scroll').scrollLeft = 0))
	await sleep(300)
	const b2 = await card(p, 'Touch card').boundingBox()
	const tgt = await lane(p, 'Doing').locator('.lane-cards').boundingBox()
	const gx = b2.x + b2.width - 14
	const gy = b2.y + b2.height / 2
	await touch('touchStart', gx, gy)
	for (let i = 1; i <= 12; i++) await touch('touchMove', gx + (tgt.x + 40 - gx) * i / 12, gy + (tgt.y + tgt.height - 6 - gy) * i / 12)
	await touch('touchEnd')
	await lane(p, 'Doing').locator('.card-title', { hasText: /^Touch card$/ }).waitFor()
	assert.ok(!(await titles(p, 'To do')).includes('Touch card'), 'it left To do')
	noErrors(p)
	await p.context().close()
})

test('two people see each other and each other’s changes', async () => {
	const a = await login(browser, server.base, 'anna@example.com')
	const b = await login(browser, server.base, 'ben@example.com')
	await openBoard(a, server.base)
	await openBoard(b, server.base)
	await a.waitForFunction(() => document.querySelectorAll('#presence .avatar').length === 2)
	const t0 = Date.now()
	await addCard(a, 'Doing', 'Shared card')
	await card(b, 'Shared card').waitFor({ timeout: 2000 })
	assert.ok(Date.now() - t0 < 2000)
	// Ben leaves: after the grace period Anna sees only herself.
	await b.context().close()
	await a.waitForFunction(() => document.querySelectorAll('#presence .avatar').length === 1, null, { timeout: 8000 })
	noErrors(a)
	await a.context().close()
})

test('a move from a stale page is refused and says why', async () => {
	const a = await login(browser, server.base, 'anna@example.com')
	await openBoard(a, server.base)
	await addCard(a, 'To do', 'Contested card')
	// Ben's page is drawn, but its stream is held back: it stays stale.
	const b = await login(browser, server.base, 'ben@example.com')
	let release
	const gate = new Promise((r) => (release = r))
	await b.route('**/live?**', async (route) => {
		await gate
		await route.continue()
	})
	await b.goto(`${server.base}/boards/1`)
	await card(b, 'Contested card').waitFor()
	// Anna moves it to Done.
	await card(a, 'Contested card').focus()
	await a.keyboard.down('Alt')
	await a.keyboard.press('ArrowRight')
	await a.keyboard.press('ArrowRight')
	await a.keyboard.up('Alt')
	await lane(a, 'Done').locator('.card-title', { hasText: /^Contested card$/ }).waitFor()
	// Ben, still seeing it in To do, moves it to Doing.
	await card(b, 'Contested card').focus()
	await b.keyboard.down('Alt')
	await b.keyboard.press('ArrowRight')
	await b.keyboard.up('Alt')
	await sleep(300)
	assert.ok((await b.locator('#pending-style').textContent()).includes(`#${await card(b, 'Contested card').getAttribute('id')}{opacity`), 'pending: no answer without a stream')
	assert.equal(await b.locator('[data-landing-marker="card"]').count(), 1, 'the drop is marked where it should go')
	release()
	await b.waitForFunction(() => document.querySelector('sb-toast')?.getAttribute('toasts')?.includes('Anna moved'), null, { timeout: 5000 })
	await b.locator('[data-landing-marker]').waitFor({ state: 'detached', timeout: 5000 })
	await lane(b, 'Done').locator('.card-title', { hasText: /^Contested card$/ }).waitFor()
	assert.match(await b.locator('#status').textContent(), /Anna moved ‘Contested card’ to ‘Done’ meanwhile/)
	assert.ok(!(await b.locator('#pending-style').textContent()).includes('opacity'), 'nothing left pending')
	assert.ok((await titles(a, 'Done')).includes('Contested card'))
	noErrors(a, b)
	await a.context().close()
	await b.context().close()
})

test('the card dialog: rename, comment, label, due date, description, archive', async () => {
	const p = await login(browser, server.base, 'chiara@example.com')
	await openBoard(p, server.base)
	await addCard(p, 'To do', 'Detail card')
	await card(p, 'Detail card').locator('.card-title').click()
	await p.waitForFunction(() => document.querySelector('#detail')?.isOpen)
	assert.match(p.url(), /\/boards\/1\/cards\/\d+$/)
	const d = p.locator('#detail')

	// rename
	await d.locator('.title-text').dblclick()
	const ti = d.locator('.title-text-input')
	await ti.fill('Renamed card')
	await ti.press('Enter')
	await card(p, 'Renamed card').waitFor()

	// comment, then delete it
	await d.locator('.comment-new textarea').fill('First!\nSecond line')
	await d.locator('.comment-new button').click()
	await d.locator('.comment-text', { hasText: 'Second line' }).waitFor()
	assert.equal(await d.locator('.comment-new textarea').inputValue(), '')
	p.once('dialog', (dlg) => dlg.accept())
	await d.locator('.comment .own-only').first().click()
	await d.locator('.comment').first().waitFor({ state: 'detached' }).catch(() => {})
	await p.waitForFunction(() => !document.querySelector('#detail .comment-text'))

	// label
	await d.locator('.label-picker button', { hasText: 'Bug' }).click()
	await p.waitForFunction(() => document.querySelector('#detail .label-picker button[aria-pressed="true"]')?.textContent.includes('Bug'))
	await card(p, 'Renamed card').locator('.label', { hasText: 'Bug' }).waitFor()

	// due date
	await d.locator('input[type=date]').fill('2026-12-24')
	await card(p, 'Renamed card').locator('.badge.due').waitFor()
	assert.match(await card(p, 'Renamed card').locator('.badge.due').textContent(), /Dec 24/)

	// description, Markdown and no HTML
	await d.locator('.section-head button', { hasText: 'Edit' }).click()
	await d.locator('.desc-edit textarea').fill('**Bold** and <script>alert(1)</script> [x](javascript:alert(1))')
	await d.locator('.desc-edit button[type=submit]').click()
	await d.locator('.markdown strong', { hasText: 'Bold' }).waitFor()
	assert.equal(await d.locator('.markdown script').count(), 0)
	assert.ok(!((await d.locator('.markdown a').getAttribute('href')) || '').startsWith('javascript'))

	// archive
	p.once('dialog', (dlg) => dlg.accept())
	await d.locator('button', { hasText: 'Archive card' }).click()
	await card(p, 'Renamed card').waitFor({ state: 'detached' })
	await p.waitForFunction(() => document.querySelector('#detail')?.getAttribute('heading') === 'Card not found')

	// Escape closes it and the URL follows
	await p.keyboard.press('Escape')
	await p.waitForURL(/\/boards\/1$/)
	noErrors(p)
	await p.context().close()
})

test('a card URL opens its dialog on a full page load, and back closes it', async () => {
	const p = await login(browser, server.base, 'anna@example.com')
	await openBoard(p, server.base)
	const href = await p.locator('.card-title').first().getAttribute('href')
	await p.goto(server.base + href)
	await p.waitForFunction(() => document.querySelector('#detail')?.isOpen)
	await p.locator('.card-title').nth(1).click({ force: true }).catch(() => {})
	await p.keyboard.press('Escape')
	await p.waitForFunction(() => !document.querySelector('#detail')?.isOpen)
	await p.goBack()
	await p.waitForFunction(() => document.querySelector('#detail')?.isOpen, null, { timeout: 4000 })
	noErrors(p)
	await p.context().close()
})

test('lists: add, rename, move, archive', async () => {
	const p = await login(browser, server.base, 'dev@example.com')
	await openBoard(p, server.base)
	await p.locator('.add-list input').fill('Later')
	await p.locator('.add-list input').press('Enter')
	await lane(p, 'Later').waitFor()
	await lane(p, 'Later').locator('.lane-name').dblclick()
	await lane(p, 'Later').locator('.lane-name-input').fill('Someday')
	await p.keyboard.press('Enter')
	await lane(p, 'Someday').waitFor()
	const names = () => p.locator('.lane-name').allTextContents()
	assert.equal((await names()).at(-1), 'Someday')
	await lane(p, 'Someday').locator('.icon-btn').click()
	await p.locator('.menu:popover-open button', { hasText: 'Move left' }).click()
	await p.waitForFunction(() => [...document.querySelectorAll('.lane-name')].at(-2)?.textContent === 'Someday')
	await lane(p, 'Someday').locator('.icon-btn').click()
	p.once('dialog', (dlg) => dlg.accept())
	await p.locator('.menu:popover-open button', { hasText: 'Archive list' }).click()
	await lane(p, 'Someday').waitFor({ state: 'detached' })
	noErrors(p)
	await p.context().close()
})

test('a change that can’t reach the server waits, says so, and lands once', async () => {
	const p = await login(browser, server.base, 'anna@example.com')
	await openBoard(p, server.base)
	let blocked = true
	let attempts = 0
	await p.route('**/boards/1/cards', (route) => {
		attempts++
		return blocked ? route.abort('internetdisconnected') : route.continue()
	})
	const input = lane(p, 'To do').locator('.add-card input')
	await input.fill('Retried card')
	await input.press('Enter')
	await p.locator('.net-problem', { hasText: 'Trying again' }).waitFor({ timeout: 5000 })
	assert.equal(await card(p, 'Retried card').count(), 0, 'not shown as added')
	assert.equal(await input.inputValue(), 'Retried card', 'the text is kept')
	blocked = false
	await card(p, 'Retried card').waitFor({ timeout: 10000 })
	await p.locator('.net-problem').waitFor({ state: 'hidden' })
	assert.equal(await card(p, 'Retried card').count(), 1, 'added once')
	assert.ok(attempts >= 2)
	await p.context().close()
})

test('the page survives a server restart', async () => {
	const p = await login(browser, server.base, 'anna@example.com')
	await openBoard(p, server.base)
	await server.stop()
	await p.locator('.offline').waitFor({ timeout: 5000 })
	await server.start()
	await p.locator('.offline').waitFor({ state: 'hidden', timeout: 15000 })
	await addCard(p, 'Doing', 'After restart')
	await p.context().close()
})

test('security: other boards, other sites, no session', async () => {
	const p = await login(browser, server.base, 'anna@example.com')
	const cookies = await p.context().cookies()
	const cookie = cookies.map((c) => `${c.name}=${c.value}`).join('; ')
	const post = (path, origin) =>
		fetch(server.base + path, {
			method: 'POST',
			headers: { cookie, origin, 'datastar-request': 'true', 'content-type': 'application/json' },
			body: JSON.stringify({ tab: 'aaaaaaaaaaaaaaaaaaaaaa', op: 'x1', list: 1, title: 'evil' }),
			redirect: 'manual',
		})
	assert.equal((await post('/boards/1/cards', 'https://evil.example')).status, 403)
	assert.equal((await post('/boards/1/cards', server.base)).status, 204)
	assert.equal((await fetch(server.base + '/boards/1/live?tab=aaaaaaaaaaaaaaaaaaaaaa')).status, 401)
	assert.equal((await fetch(server.base + '/boards/1', { redirect: 'manual' })).status, 303)
	const res = await fetch(server.base + '/boards/999', { headers: { cookie } })
	assert.equal(res.status, 404)
	const forged = await fetch(server.base + '/login', { method: 'POST', headers: { 'content-type': 'application/x-www-form-urlencoded', origin: 'https://evil.example' }, body: 'email=a&password=b&csrf=x' })
	assert.equal(forged.status, 403)
	await p.context().close()
})
