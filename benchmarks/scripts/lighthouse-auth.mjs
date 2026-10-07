// Lighthouse (performance only) on a page behind a login, the same way for
// both apps. The person logs in once in a Chrome profile; Lighthouse then
// launches Chrome with that profile. Its storage reset clears the cache
// and site storage but keeps cookies, so every run is a cold load of the
// signed-in page. (PLANKA's client reads its token from document.cookie,
// so a Cookie header alone would only get its login page.) Three runs per
// form factor; the one with the median score is kept.
//
//   node lighthouse-auth.mjs ours http://127.0.0.1:18080 /boards/2
//   node lighthouse-auth.mjs planka http://127.0.0.1:19001 /boards/<id>
import { execFileSync } from 'node:child_process'
import { mkdirSync, mkdtempSync, readFileSync, rmSync, writeFileSync } from 'node:fs'
import { tmpdir } from 'node:os'
import { join } from 'node:path'
import { chromium } from 'playwright-core'

const [app, base, path] = process.argv.slice(2)
const chrome = process.env.CHROME_PATH || '/usr/bin/chromium'
const profile = mkdtempSync(join(tmpdir(), 'lh-profile-'))
const out = new URL(`../results/${new Date().toISOString().slice(0, 7)}/`, import.meta.url).pathname
mkdirSync(out, { recursive: true })

const ctx = await chromium.launchPersistentContext(profile, { executablePath: chrome, headless: true })
const page = await ctx.newPage()
await page.goto(base + '/login')
if (app === 'planka') {
	await page.fill('input[name=emailOrUsername]', 'anna@example.com')
	await page.fill('input[name=password]', 'kanban-demo-2026')
	await Promise.all([page.waitForURL(base + '/'), page.keyboard.press('Enter')])
} else {
	await page.fill('input[name=email]', 'anna@example.com')
	await page.fill('input[name=password]', 'demo')
	await Promise.all([page.waitForURL(base + '/'), page.click('button[type=submit]')])
}
await ctx.close()

for (const form of ['desktop', 'mobile']) {
	const runs = []
	for (let i = 1; i <= 3; i++) {
		const file = join(out, `.lh-${app}-${form}-${i}.json`)
		const args = [base + path, '--only-categories=performance', '--output=json', `--output-path=${file}`, '--quiet', '--max-wait-for-load=45000',
			`--chrome-flags=--headless=new --no-sandbox --user-data-dir=${profile}`]
		if (form === 'desktop') args.push('--preset=desktop')
		try {
			execFileSync('lighthouse', args, { stdio: 'ignore', env: { ...process.env, CHROME_PATH: chrome }, timeout: 180000 })
			const r = JSON.parse(readFileSync(file, 'utf8'))
			if (!r.finalDisplayedUrl.includes(path)) throw new Error(`ended on ${r.finalDisplayedUrl}, not signed in`)
			runs.push({ score: r.categories.performance.score ?? 0, file, r })
		} catch (e) {
			console.error(`${app} ${form} run ${i} failed: ${e.message.split('\n')[0]}`)
			rmSync(file, { force: true })
		}
	}
	if (!runs.length) continue
	runs.sort((a, b) => a.score - b.score)
	const pick = runs[Math.floor(runs.length / 2)]
	writeFileSync(join(out, `${app}-lighthouse-${form}.json`), JSON.stringify(pick.r))
	for (const r of runs) rmSync(r.file, { force: true })
	const a = pick.r.audits
	const reqs = a['network-requests'].details.items
	const kb = (xs) => Math.round(xs.reduce((s, x) => s + (x.transferSize || 0), 0) / 1000)
	console.log(`${app} ${form}: ${pick.r.finalDisplayedUrl.replace(base, '')} score ${Math.round(pick.score * 100)} (runs ${runs.map((r) => Math.round(r.score * 100)).join(', ')}), FCP ${a['first-contentful-paint'].displayValue}, LCP ${a['largest-contentful-paint'].displayValue}, TBT ${a['total-blocking-time'].displayValue}, CLS ${a['cumulative-layout-shift'].displayValue}, ${reqs.length} requests, ${kb(reqs)} kB (JS ${kb(reqs.filter((r) => r.resourceType === 'Script'))} kB)`)
}
rmSync(profile, { recursive: true, force: true })
