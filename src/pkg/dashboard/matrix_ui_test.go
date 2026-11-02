package dashboard

import (
	"os"
	"os/exec"
	"testing"
)

// Exercise browser lifecycle and pull behavior with deterministic DOM/clock
// doubles. No browser dependencies, network or real animation timers required.
func TestMatrixUIBehavior(t *testing.T) {
	node, err := exec.LookPath("node")
	if err != nil {
		t.Skip("node unavailable")
	}
	cmd := exec.Command(node, "-e", matrixBrowserTest)
	cmd.Env = append(os.Environ(), "TZ=Pacific/Honolulu")
	if out, err := cmd.CombinedOutput(); err != nil {
		t.Fatalf("Matrix browser behavior: %v\n%s", err, out)
	}
}

const matrixBrowserTest = `
const assert = require('assert');
const fs = require('fs');
const vm = require('vm');
const matrix = fs.readFileSync('static-src/js/matrix.js', 'utf8');
const boot = fs.readFileSync('static-src/js/boot.js', 'utf8').split('})();')[0] + '})();';
const refresh = fs.readFileSync('static-src/js/refresh.js', 'utf8');
const timezoneHelpers = refresh.slice(refresh.indexOf('const DASHBOARD_TIMEZONE_STORAGE_KEY'), refresh.indexOf('function formatNumber('));
const at = '2026-10-31T12:00:00Z';
function row(seq, block, change, line, extra = {}) {
  return Object.assign({seq, block, change, line, repo: 'org/repo', pr: 2, path: 'main.go', stage: 'pushed', at}, extra);
}
const rows = [row(1, 1, 'remove', 'Ωold paragraph'), row(2, 1, 'add', '  new <script>alert(1)</script>'),
  row(3, 1, 'add', ''), row(4, 1, 'add', '}'), row(5, 1, 'add', '}'),
  row(6, 6, 'remove', 'deleted paragraph'), row(7, 7, 'add', 'another task', {pr: 3})];
async function scenario(query, reduced = false, initial = rows) {
  const events = {}, docEvents = {}, raf = new Map(), timers = new Map(), requests = [], draws = [];
  let id = 0, responseStatus = 200, epoch = 1, dataset = initial;
  const context = {clearRect(){}, fillRect(){}, fillText(...a){draws.push(a)}, drawImage(){}};
  const elements = [];
  function el(tag) {
    const e = {tagName: tag.toUpperCase(), children: [], style: {setProperty(){}}, events: {}, _top: 0,
      classList: {add(){}}, setAttribute(n, v){this[n] = v},
      addEventListener(n, fn){(this.events[n] ||= []).push(fn)},
      append(...a){a.forEach(x => this.appendChild(x))},
      appendChild(a){a.parentNode = this; this.children.push(a); return a},
      replaceChildren(...a){this.children.forEach(x => x.parentNode = null); this.children = []; this.append(...a)},
      remove(){if (this.parentNode) this.parentNode.children.splice(this.parentNode.children.indexOf(this), 1); this.parentNode = null},
      getContext(){return context}};
    Object.defineProperties(e, {
      innerHTML: {set(){throw Error('code must never be interpreted as HTML')}},
      clientHeight: {get(){return this.tagName === 'SECTION' ? 360 : this.offsetHeight}},
      offsetHeight: {get(){
        if (this.tagName === 'PRE') return (this.textContent || '').split('\n').length * 24 + 24;
        if (this.tagName === 'HEADER') return 64;
        return Math.max(1, this.children.reduce((n, c) => n + c.offsetHeight, 0) + (this.tagName === 'ARTICLE' ? 16 : 0));
      }},
      scrollHeight: {get(){return this.children.reduce((n, c) => n + c.offsetHeight, 0)}},
      scrollTop: {get(){return Math.min(this._top, Math.max(0, this.scrollHeight - this.clientHeight))},
        set(v){this._top = Math.max(0, Math.min(v, Math.max(0, this.scrollHeight - this.clientHeight)))}}
    });
    elements.push(e); return e;
  }
  const body = el('body');
  const doc = {body, hidden: false, createElement: el, createTextNode(text){const n = el('text'); n.textContent = text; return n},
    getElementById(id){return elements.find(e => e.id === id)},
    addEventListener(n, fn){(docEvents[n] ||= []).push(fn)}};
  const motion = {matches: reduced, addEventListener(n, fn){this.change = fn}};
  const win = {location: {href: 'https://hive.test/' + query, pathname: '/'}, innerWidth: 800, innerHeight: 600,
    matchMedia(){return motion}, addEventListener(){}};
  const c = vm.createContext({window: win, document: doc, URL, location: {hash: '#activity'},
    history: {replaceState(){}}, localStorage: {getItem(key){return key === 'hive-dashboard-display-timezone' ? 'UTC' : null}, setItem(){}},
    requestAnimationFrame(fn){raf.set(++id, fn); return id}, cancelAnimationFrame(i){raf.delete(i)},
    setTimeout(fn){timers.set(++id, fn); return id}, clearTimeout(i){timers.delete(i)}, setInterval(){}, dashboardSetInterval(){},
    fetch(url){
      requests.push(url);
      const since = Number(new URL(url, 'https://hive.test').searchParams.get('since')) || 0;
      const latest = dataset.at(-1)?.seq || 0;
      const pending = dataset.filter(r => r.seq > since);
      const boundary = pending[0]?.block === 1 ? pending.filter(r => r.block === 1).at(-1).seq : latest;
      const page = pending.filter(r => r.seq <= boundary);
      return Promise.resolve({status: responseStatus, ok: responseStatus === 200,
        json(){return Promise.resolve({seq: page.at(-1)?.seq || latest, latest, epoch, workspaces: true, lines: page})}});
    }, Date, Math});
  vm.runInContext(timezoneHelpers, c);
  vm.runInContext(matrix, c); c.hiveMatrixStart = win.hiveMatrixStart;
  vm.runInContext(boot, c);
  for (const fn of docEvents.DOMContentLoaded || []) fn();
  const flush = async () => {for(let i = 0; i < 40; i++) await Promise.resolve()};
  await flush();
  if (!win.HIVE_MATRIX) {
    assert.equal(requests.length, 0); assert(!doc.getElementById('hive-matrix')); return null;
  }
  win.hiveMatrixAttach({addEventListener(n, fn){events[n] = fn}});
  const byClass = n => elements.find(e => e.className === n);
  const tick = time => {const fns = [...raf.values()]; raf.clear(); fns.forEach(fn => fn(time))};
  const key = key => docEvents.keydown[0]({key, target: body, preventDefault(){}});
  return {doc, docEvents, motion, raf, timers, draws, byClass, tick, key, flush,
    async update(next, nextEpoch = epoch, status = 200){
      dataset = next; epoch = nextEpoch; responseStatus = status; events['code-lines'](); await flush();
    }};
}
function paragraphs(h) {
  return h.byClass('matrix-cascade-track').children.flatMap(card => card.children.filter(c => c.tagName === 'PRE').map(p => p.textContent));
}
(async () => {
  for (const query of ['', '?view=matrix', '?kiosk=1']) await scenario(query);
  for (const reduced of [false, true]) {
    const h = await scenario('?kiosk=1&view=matrix&mx.effect=cascade', reduced);
    assert.deepEqual(paragraphs(h), ['− Ωold paragraph', '+   new <script>alert(1)</script>\n+ \n+ }\n+ }', '− deleted paragraph', '+ another task']);
    assert.equal(h.byClass('matrix-cascade-track').children.length, 3, 'different blocks and PRs must not merge');
    assert(h.byClass('matrix-cascade-track').children.every(card => card.children[0].children[2].textContent.includes('UTC')), 'observed timestamps must honor the viewer timezone, not the browser timezone');
    assert.equal(h.raf.size, reduced ? 0 : 1);
    if (reduced) assert.equal(h.timers.size, 0, 'reduced-motion code must not auto-page');
    const active = h.byClass('matrix-cascade-track').children[0];
    h.key('p');
    h.motion.matches = !reduced; h.motion.change();
    assert.strictEqual(h.byClass('matrix-cascade-track').children[0], active, 'changing reduced motion must not erase a paused paragraph');
    h.motion.matches = reduced; h.motion.change();
    const next = rows.concat([row(8, 8, 'add', 'new workspace paragraph', {stage: 'in-progress', agent: 'quality', pr: 0})]);
    await h.update(next);
    assert.strictEqual(h.byClass('matrix-cascade-track').children[0], active, 'incoming activity must not interrupt a paused reader');
    assert.equal(h.raf.size, 0);
    h.key('p'); h.tick(1); h.tick(101);
    assert(paragraphs(h).includes('+ new workspace paragraph'));
    h.doc.hidden = true; h.docEvents.visibilitychange.forEach(fn => fn());
    assert.equal(h.raf.size, 0); assert.equal(h.timers.size, 0);
    await h.update([], 2);
    assert.deepEqual(paragraphs(h), [], 'policy epochs must forget previously displayed code even while hidden');
    await h.update(rows, 2, 403);
    assert(h.byClass('matrix-cascade').hidden);
    assert.deepEqual(paragraphs(h), [], 'loss of permission must clear code');
  }
  for (const effect of ['rain', 'ghost', 'terminal']) {
    const h = await scenario('?kiosk=1&view=matrix&mx.effect=' + effect);
    for (let i = 0; i < 80; i++) h.tick(i * 100 + 1);
    assert(!h.draws.some(a => String(a[0]).includes('Ω') || String(a[0]).includes('deleted paragraph')), 'removals must never masquerade as green additions');
  }
  const many = [];
  for (let b = 0; b < 100; b++) for (let line = 0; line < 16; line++) many.push(row(b * 16 + line + 1, b * 16 + 1, 'add', 'block ' + b + ' line ' + line));
  const bounded = await scenario('?kiosk=1&view=matrix&mx.effect=cascade', false, many);
  for (let i = 0; i < 100; i++) bounded.tick(i * 100 + 1);
  assert(bounded.byClass('matrix-cascade-track').children.length <= 32);
  for (const text of paragraphs(bounded)) {
    assert(text.split('\n')[0].endsWith('line 0') && text.split('\n').at(-1).endsWith('line 15'), 'history/viewport bounds must preserve whole paragraphs');
  }
})().catch(e => {console.error(e); process.exit(1)});
`
