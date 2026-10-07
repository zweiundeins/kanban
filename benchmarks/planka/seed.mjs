// Loads seed.json (written by `kanban export` from a freshly seeded
// database) into PLANKA through its REST API, so both apps hold the same
// boards: users, project, boards, members, labels, lists, cards with
// descriptions, due dates and labels, and every comment by its author.
//
//   node seed.mjs http://172.17.0.1:3300 seed.json
import { readFileSync } from 'node:fs'

const base = process.argv[2] || 'http://172.17.0.1:3300'
const data = JSON.parse(readFileSync(process.argv[3] || new URL('seed.json', import.meta.url), 'utf8'))
const env = Object.fromEntries(readFileSync(new URL('.env', import.meta.url), 'utf8').trim().split('\n').map((l) => l.split(/=(.*)/s).slice(0, 2)))
// PLANKA refuses weak passwords (zxcvbn score 2), so its demo users get this one.
export const PASSWORD = 'kanban-demo-2026'
const GAP = 65536

// PLANKA's label colours closest to ours.
const colors = { green: 'bright-moss', yellow: 'egg-yellow', orange: 'pumpkin-orange', red: 'berry-red', purple: 'sugar-plum', blue: 'lagoon-blue', sky: 'summer-sky', grey: 'muddy-grey' }

async function api(token, method, path, body) {
	const res = await fetch(base + path, {
		method,
		headers: { 'content-type': 'application/json', ...(token ? { authorization: `Bearer ${token}` } : {}) },
		body: body ? JSON.stringify(body) : undefined,
	})
	const text = await res.text()
	if (!res.ok) throw new Error(`${method} ${path}: ${res.status} ${text.slice(0, 300)}`)
	return text ? JSON.parse(text) : null
}

// The first login of each user asks to accept the instance's terms (a
// template on a fresh install), signed by their hash.
async function login(emailOrUsername, password) {
	const res = await fetch(base + '/api/access-tokens', { method: 'POST', headers: { 'content-type': 'application/json' }, body: JSON.stringify({ emailOrUsername, password }) })
	const body = await res.json()
	if (res.ok) return body.item
	if (body.step !== 'accept-terms') throw new Error(`login ${emailOrUsername}: ${res.status} ${JSON.stringify(body).slice(0, 200)}`)
	const terms = (await api(null, 'GET', '/api/terms')).item
	return (await api(null, 'POST', '/api/access-tokens/accept-terms', { pendingToken: body.pendingToken, signature: terms.signature })).item
}

const admin = await login('admin@example.com', env.PLANKA_ADMIN_PASSWORD)
const existing = (await api(admin, 'GET', '/api/projects')).items
if (existing.length) {
	console.error('PLANKA already has projects; start from an empty one (docker compose down -v && ./setup.sh)')
	process.exit(1)
}

const users = {}
for (const u of data.users) {
	const username = u.email.split('@')[0]
	const item = (await api(admin, 'POST', '/api/users', { email: u.email, password: PASSWORD, role: 'boardUser', name: u.name, username })).item
	users[u.email] = { id: item.id, token: await login(u.email, PASSWORD) }
}

let cards = 0
let comments = 0
for (const p of data.projects) {
	const project = (await api(admin, 'POST', '/api/projects', { type: 'shared', name: p.name })).item
	for (const [bi, b] of p.boards.entries()) {
		const board = (await api(admin, 'POST', `/api/projects/${project.id}/boards`, { position: (bi + 1) * GAP, name: b.name })).item
		for (const email of b.members) {
			await api(admin, 'POST', `/api/boards/${board.id}/board-memberships`, { userId: users[email].id, role: 'editor' })
		}
		const labels = {}
		for (const [li, l] of b.labels.entries()) {
			labels[l.Name] = (await api(admin, 'POST', `/api/boards/${board.id}/labels`, { position: (li + 1) * GAP, name: l.Name, color: colors[l.Color] })).item.id
		}
		for (const [li, l] of b.lists.entries()) {
			const list = (await api(admin, 'POST', `/api/boards/${board.id}/lists`, { type: 'active', position: (li + 1) * GAP, name: l.name })).item
			for (const [ci, c] of l.cards.entries()) {
				const body = { type: 'project', position: (ci + 1) * GAP, name: c.title }
				if (c.description) body.description = c.description
				if (c.due_on) {
					body.dueDate = `${c.due_on}T12:00:00.000Z`
					body.isDueCompleted = !!c.due_done
				}
				const card = (await api(admin, 'POST', `/api/lists/${list.id}/cards`, body)).item
				for (const name of c.labels || []) await api(admin, 'POST', `/api/cards/${card.id}/card-labels`, { labelId: labels[name] })
				for (const cm of c.comments || []) {
					await api(users[cm.author].token, 'POST', `/api/cards/${card.id}/comments`, { text: cm.body })
					comments++
				}
				cards++
			}
		}
		console.log(`${b.name}: ${b.lists.length} lists, board ${board.id}`)
	}
}
console.log(`${Object.keys(users).length} users, ${cards} cards, ${comments} comments`)
