import './style.css';
import { Events as WailsEvents } from '@wailsio/runtime';
import { App } from '../bindings/resolve-sync';

// Opening the page with ?mock in a plain browser (npm run dev) swaps the app
// for a stand-in with sample data, for layout work.
const mock = new URLSearchParams(location.search).has('mock') ? await import('./mock.js') : null;
const api = mock ? mock.App : App;
const Events = mock ? mock.Events : WailsEvents;
const $ = (id) => document.getElementById(id);
const el = (tag, props = {}, ...kids) => {
  const n = Object.assign(document.createElement(tag), props);
  n.append(...kids);
  return n;
};

// One colour per angle, in order; the PA mix and unassigned sources are grey.
const COLORS = ['#4c8dff', '#3fb97a', '#e0a33a', '#c678dd', '#2bb5b8', '#e46fa8', '#9ccc4f', '#f08a4b'];
const color = (i) => (i >= 0 ? COLORS[i % COLORS.length] : '#5a5a63');

const state = {
  project: null,
  folders: [],   // {path, name, detail, input, files, live: [], clips}
  mix: null,     // {path, name}
  mixRow: null,  // sync result for the mix
  mixLive: {},
  audio: '',     // '' | 'mix' | angle index as string
  timeline: null, // {length, program}
  synced: false,
  busy: false,
  progress: null,
  started: 0,
};

function status(text, error = false) {
  $('status').textContent = text || '';
  $('status').classList.toggle('error', error);
}

// Anything that changes what would be synced makes the last result stale.
function stale() {
  state.synced = false;
  state.mixRow = null;
  state.mixLive = {};
  state.timeline = null;
  for (const f of state.folders) { f.clips = null; f.live = []; }
  $('report').hidden = true;
}

function take(opened) {
  if (!opened) return;
  if (opened.project) { state.project = opened.project; stale(); }
  for (const f of opened.folders || []) {
    if (!state.folders.some((x) => x.path === f.path)) {
      state.folders.push({ ...f, input: 0, clips: null, live: [] });
      stale();
    }
  }
  if (opened.mix) { state.mix = opened.mix; state.audio = 'mix'; stale(); }
  status((opened.errors || []).join(' · '), true);
  render();
}

function time(seconds) {
  const sign = seconds < 0 ? '−' : '';
  const s = Math.abs(seconds);
  const h = Math.floor(s / 3600), m = Math.floor(s / 60) % 60;
  return `${sign}${h}:${String(m).padStart(2, '0')}:${(s % 60).toFixed(2).padStart(5, '0')}`;
}

function clock(seconds) {
  const s = Math.max(0, Math.round(seconds));
  const h = Math.floor(s / 3600), m = Math.floor(s / 60) % 60;
  return h ? `${h}:${String(m).padStart(2, '0')}:${String(s % 60).padStart(2, '0')}` : `${m}:${String(s % 60).padStart(2, '0')}`;
}

// Waveforms arrive as base64 (one byte a second); decode once and keep.
function bytes(wave) {
  if (!wave) return null;
  if (wave instanceof Uint8Array) return wave;
  const s = atob(wave);
  const out = new Uint8Array(s.length);
  for (let i = 0; i < s.length; i++) out[i] = s.charCodeAt(i);
  return out;
}

// Canvases are drawn after layout, at the screen's pixel density.
const pending = [];
function later(canvas, draw) { pending.push([canvas, draw]); }
function drawPending() {
  for (const [canvas, draw] of pending.splice(0)) {
    const w = canvas.clientWidth, h = canvas.clientHeight;
    if (!w || !h) continue;
    const dpr = devicePixelRatio || 1;
    canvas.width = Math.round(w * dpr);
    canvas.height = Math.round(h * dpr);
    const g = canvas.getContext('2d');
    g.scale(dpr, dpr);
    draw(g, w, h);
  }
}

// drawWave draws a mirrored level graph of wave[from..to) seconds into the
// box x, y, w, h.
function drawWave(g, wave, from, to, x, y, w, h, fill) {
  if (!wave || !wave.length || w <= 0) return;
  g.fillStyle = fill;
  const mid = y + h / 2;
  const span = Math.max(1e-9, to - from);
  for (let px = 0; px < w; px++) {
    const a = Math.max(0, Math.floor(from + (px / w) * span));
    const b = Math.min(wave.length, Math.max(a + 1, Math.ceil(from + ((px + 1) / w) * span)));
    let peak = 0;
    for (let i = a; i < b; i++) if (wave[i] > peak) peak = wave[i];
    const half = Math.max(0.5, (peak / 255) * (h / 2));
    g.fillRect(x + px, mid - half, 1, half * 2);
  }
}

const LIVE = { listening: 'listening', searching: 'searching', matching: 'matching' };

// Thumbnails by clip path. They are made in the background and can arrive
// before the folder they belong to has been added (several folders opened
// at once), so they are kept here rather than on the folder.
const thumbs = new Map();

function clipRows(f, fi) {
  const box = el('div', { className: 'clips' });
  f.files.forEach((file, ci) => {
    const res = f.clips?.[ci];
    const live = f.live[ci] || {};
    const wave = bytes(res?.wave || live.wave);
    let dot = 'idle', at = '', how = clock(file.duration), howClass = 'how';
    if (res) {
      dot = !res.placed ? 'bad' : res.warning || !res.exact ? 'warn' : '';
      at = res.placed ? time(res.offset) : '';
      how = res.placed ? res.method : 'not placed';
    } else if (LIVE[live.state]) {
      dot = 'live';
      how = LIVE[live.state];
      howClass = 'how live';
    } else if (live.state === 'placed') {
      dot = '';
      at = time(live.offset);
      how = live.method;
    } else if (live.state === 'failed') {
      dot = 'bad';
      how = 'not placed';
    }
    const thumb = el('div', { className: 'thumb' }, el('span', { className: `dot ${dot}` }));
    thumb.dataset.path = file.path;
    const url = file.thumb || thumbs.get(file.path);
    if (url) thumb.style.backgroundImage = `url("${url}")`;
    const wv = el('canvas');
    const placedColor = res?.placed || live.state === 'placed' ? color(fi) : '#6c6c75';
    later(wv, (g, w, h) => drawWave(g, wave, 0, file.duration, 0, 0, w, h, placedColor));
    box.append(
      thumb,
      el('div', { className: 'namewave' }, el('span', { className: 'file', textContent: file.name, title: file.path }), wv),
      el('span', { className: 'at', textContent: at }),
      el('span', { className: howClass, textContent: how }),
    );
    if (res?.warning) box.append(el('span', { className: 'note', textContent: res.warning }));
  });
  return box;
}

let renderQueued = false;
function render() {
  if (renderQueued) return;
  renderQueued = true;
  requestAnimationFrame(() => { renderQueued = false; renderNow(); });
}

function renderNow() {
  // Keep the cursor in an angle name being edited.
  const active = document.activeElement;
  const editing = active?.classList?.contains('rename') ? { i: active.dataset.i, s: active.selectionStart, e: active.selectionEnd } : null;

  // Project
  const p = $('project');
  p.replaceChildren();
  if (state.project) {
    p.append(el('div', { className: 'card' }, el('div', { className: 'item' },
      el('span', { className: 'name', textContent: state.project.name }),
      el('span', { className: 'detail', textContent: state.project.detail }),
      el('span', { className: 'spacer' }),
      el('button', { textContent: 'Change', disabled: state.busy, onclick: async () => take(await api.PickProject()) }),
    )));
  } else {
    p.append(el('div', { className: 'empty' },
      el('button', { textContent: 'Choose ATEM project', onclick: async () => take(await api.PickProject()) })));
  }

  // Cameras
  const cams = $('cameras');
  cams.replaceChildren();
  state.folders.forEach((f, i) => {
    const name = el('input', { className: 'rename', value: f.name, spellcheck: false });
    name.dataset.i = i;
    name.style.color = color(i);
    name.oninput = () => { f.name = name.value; renderAudio(); drawTimeline(); };
    const input = el('select', { disabled: state.busy },
      el('option', { value: 0, textContent: 'Auto' }),
      ...(state.project?.inputs || []).map((x) => el('option', { value: x.index, textContent: x.name })),
      el('option', { value: -1, textContent: 'Not on switcher' }),
    );
    input.value = f.input;
    input.onchange = () => { f.input = Number(input.value); f.chosen = true; stale(); render(); };
    const card = el('div', { className: 'card' }, el('div', { className: 'item' },
      name,
      el('span', { className: 'detail', textContent: f.detail }),
      input,
      el('button', { className: 'icon', textContent: '×', title: 'Remove', disabled: state.busy, onclick: () => {
        state.folders.splice(i, 1);
        if (state.audio === String(i)) state.audio = '';
        stale(); render();
      } }),
    ));
    card.append(clipRows(f, i));
    cams.append(card);
  });
  if (!state.folders.length) cams.append(el('div', { className: 'empty', textContent: 'Drop camera folders here' }));

  // PA mix
  const mix = $('mix');
  mix.replaceChildren();
  if (state.mix) {
    const live = state.mixLive.state;
    const dot = state.mixRow ? (state.mixRow.placed ? '' : 'bad') : LIVE[live] ? 'live' : live === 'placed' ? '' : null;
    if (dot !== null) mix.append(el('span', { className: `dot ${dot}` }));
    mix.append(
      el('span', { className: 'name', textContent: state.mix.name, title: state.mixRow?.warning || state.mix.path }),
      el('button', { className: 'icon', textContent: '×', title: 'Remove', disabled: state.busy, onclick: () => {
        state.mix = null;
        if (state.audio === 'mix') state.audio = '';
        stale(); render();
      } }),
    );
  } else {
    mix.append(el('button', { textContent: 'Choose', onclick: async () => take(await api.PickMix()) }));
  }

  renderAudio();
  renderProgress();
  $('timeline-section').hidden = !state.timeline;
  $('sync').textContent = state.busy ? 'Cancel' : 'Sync';
  $('sync').disabled = !state.busy && !(state.project && state.folders.length);
  $('save').disabled = state.busy || !state.synced;
  $('add-folder').disabled = state.busy;

  drawPending();
  drawTimeline();
  if (editing) {
    const n = document.querySelector(`.rename[data-i="${editing.i}"]`);
    if (n) { n.focus(); n.setSelectionRange(editing.s, editing.e); }
  }
}

function renderAudio() {
  const a = $('audio');
  a.replaceChildren(
    el('option', { value: '', textContent: 'Follows picture' }),
    ...state.folders.map((f, i) => el('option', { value: String(i), textContent: f.name })),
    ...(state.mix ? [el('option', { value: 'mix', textContent: 'PA mix' })] : []),
  );
  a.value = state.audio;
  if (a.value !== state.audio) { state.audio = ''; a.value = ''; }
}

// Progress across the whole run: each stage has a share of the bar.
const STAGES = { sound: ['Listening', 0.3], inputs: ['Finding cameras', 0.3], match: ['Matching', 0.38], mix: ['PA mix', 0.02] };
function overall(p) {
  let sum = 0;
  for (const [key, [, share]] of Object.entries(STAGES)) {
    if (key === p.stage) return sum + share * (p.total ? p.done / p.total : 0);
    sum += share;
  }
  return sum;
}

function renderProgress() {
  const footer = document.querySelector('footer');
  footer.classList.toggle('busy', state.busy);
  const p = state.progress;
  $('bar').firstElementChild.style.width = state.busy && p ? `${(overall(p) * 100).toFixed(1)}%` : '0';
  if (state.busy && p) {
    const [label] = STAGES[p.stage] || [p.stage];
    const count = p.total > 1 ? ` · ${p.done} of ${p.total}` : '';
    const detail = p.text && p.text !== label && p.stage === 'inputs' ? ` · ${p.text}` : '';
    status(`${label}${count}${detail}`);
  }
}

setInterval(() => {
  $('elapsed').textContent = state.busy && state.started ? clock((Date.now() - state.started) / 1000) : '';
}, 500);

// Timeline: the switcher's program on top, then one lane per angle with its
// clips and their sound, then the PA mix.
const LANE = 26, TOP = 22, PROGRAM = 8, LABEL = 96, PAD = 10;
let hits = [];

function drawTimeline() {
  const tl = state.timeline;
  const canvas = $('timeline');
  if (!tl || $('timeline-section').hidden) return;
  const lanes = state.folders.length + (state.mixRow?.placed ? 1 : 0);
  canvas.style.height = `${TOP + PROGRAM + 8 + lanes * LANE + 8}px`;
  later(canvas, (g, w) => {
    hits = [];
    const x0 = LABEL, x1 = w - PAD;
    const sx = (t) => x0 + (Math.min(Math.max(t, 0), tl.length) / tl.length) * (x1 - x0);

    // Time axis
    g.font = '10px -apple-system, sans-serif';
    g.fillStyle = '#8e8e96';
    g.strokeStyle = '#2e2e33';
    const step = [60, 300, 600, 900, 1800, 3600].find((s) => tl.length / s <= 8) || 3600;
    for (let t = 0; t <= tl.length; t += step) {
      const x = Math.round(sx(t)) + 0.5;
      g.beginPath(); g.moveTo(x, 16); g.lineTo(x, TOP + PROGRAM + 8 + lanes * LANE); g.stroke();
      g.fillText(clock(t), x + 3, 12);
    }

    // Program strip, with stretches on air without footage in red
    const py = TOP;
    g.fillStyle = '#8e8e96';
    g.fillText('Program', 10, py + 7);
    for (const seg of tl.program) {
      g.fillStyle = color(seg.angle);
      const a = sx(seg.start), b = Math.max(sx(seg.end), a + 0.5);
      g.fillRect(a, py, b - a, PROGRAM);
      hits.push({ x: a, y: py, w: b - a, h: PROGRAM, tip: [state.folders[seg.angle]?.name || 'Other source', `${time(seg.start)} – ${time(seg.end)}`] });
      if (seg.angle >= 0) {
        for (const gap of uncovered(seg.angle, seg.start, seg.end)) {
          g.fillStyle = '#e5605a';
          const ga = sx(gap[0]), gb = Math.max(sx(gap[1]), ga + 1);
          g.fillRect(ga, py + PROGRAM + 1, gb - ga, 2);
          hits.push({ x: ga, y: py, w: gb - ga, h: PROGRAM + 4, tip: ['No footage on air', `${state.folders[seg.angle].name} · ${time(gap[0])} – ${time(gap[1])}`] });
        }
      }
    }

    // Angle lanes
    const lane = (row, name, col, clips) => {
      const y = TOP + PROGRAM + 8 + row * LANE;
      g.fillStyle = col;
      g.font = '600 11px -apple-system, sans-serif';
      g.fillText(fit(g, name, LABEL - 18), 10, y + LANE / 2 + 4);
      g.fillStyle = '#1a1a1d';
      g.fillRect(x0, y + 3, x1 - x0, LANE - 6);
      for (const c of clips) {
        if (!c.placed) continue;
        const a = sx(c.offset), b = sx(c.offset + c.duration);
        if (b - a < 0.5) continue;
        g.fillStyle = col + '38';
        roundRect(g, a, y + 3, b - a, LANE - 6, 3);
        g.fill();
        // Only the part inside the recording is drawn.
        const from = Math.max(0, -c.offset), to = Math.min(c.duration, tl.length - c.offset);
        drawWave(g, bytes(c.wave), from, to, a, y + 5, b - a, LANE - 10, col + 'cc');
        hits.push({ x: a, y: y + 3, w: b - a, h: LANE - 6, tip: [c.file, `${time(c.offset)} · ${c.method}${c.warning ? ' · ' + c.warning : ''}`] });
      }
    };
    state.folders.forEach((f, i) => lane(i, f.name, color(i), f.clips || []));
    if (state.mixRow?.placed) lane(state.folders.length, 'PA mix', '#a0a0aa', [state.mixRow]);
  });
  drawPending();
}

function fit(g, text, w) {
  if (g.measureText(text).width <= w) return text;
  while (text.length > 1 && g.measureText(text + '…').width > w) text = text.slice(0, -1);
  return text + '…';
}

function roundRect(g, x, y, w, h, r) {
  r = Math.min(r, w / 2, h / 2);
  g.beginPath();
  g.moveTo(x + r, y);
  g.arcTo(x + w, y, x + w, y + h, r);
  g.arcTo(x + w, y + h, x, y + h, r);
  g.arcTo(x, y + h, x, y, r);
  g.arcTo(x, y, x + w, y, r);
  g.closePath();
}

// Parts of [start, end) where an angle has no placed clip.
function uncovered(angle, start, end) {
  const spans = (state.folders[angle]?.clips || []).filter((c) => c.placed)
    .map((c) => [c.offset, c.offset + c.duration]).sort((a, b) => a[0] - b[0]);
  const out = [];
  let at = start;
  for (const [a, b] of spans) {
    if (b <= at) continue;
    if (a >= end) break;
    if (a > at) out.push([at, Math.min(a, end)]);
    at = Math.max(at, b);
  }
  if (at < end) out.push([at, end]);
  return out.filter(([a, b]) => b - a >= 0.5);
}

$('timeline').addEventListener('mousemove', (e) => {
  const r = e.currentTarget.getBoundingClientRect();
  const x = e.clientX - r.left, y = e.clientY - r.top;
  const hit = [...hits].reverse().find((h) => x >= h.x && x <= h.x + h.w && y >= h.y && y <= h.y + h.h);
  const tip = $('tip');
  if (!hit) { tip.hidden = true; return; }
  tip.replaceChildren(el('b', { textContent: hit.tip[0] }), ' ', el('span', { textContent: hit.tip[1] }));
  tip.hidden = false;
  const left = Math.min(x + 12, r.width - tip.offsetWidth - 4);
  tip.style.left = `${Math.max(4, left)}px`;
  tip.style.top = `${y + 14}px`;
});
$('timeline').addEventListener('mouseleave', () => { $('tip').hidden = true; });

let resizeTimer;
addEventListener('resize', () => { clearTimeout(resizeTimer); resizeTimer = setTimeout(render, 100); });

$('audio').onchange = () => { state.audio = $('audio').value; };
$('add-folder').onclick = async () => take(await api.PickFolder());

$('sync').onclick = async () => {
  if (state.busy) { api.Cancel(); return; }
  stale();
  state.busy = true;
  state.progress = null;
  state.started = Date.now();
  status('Starting');
  render();
  try {
    const view = await api.Sync({
      folders: state.folders.map((f) => ({ path: f.path, name: f.name, input: f.chosen ? f.input : 0 })),
      mix: state.mix?.path || '',
    });
    view.angles.forEach((a, i) => {
      const f = state.folders[i];
      f.clips = a.clips;
      // Show what was detected, without turning it into a fixed choice.
      if (!f.chosen) f.input = a.input > 0 ? a.input : -1;
    });
    state.mixRow = view.mix;
    state.timeline = { length: view.length, program: view.program || [] };
    state.synced = true;
    const all = view.angles.flatMap((a) => a.clips);
    const placed = all.filter((c) => c.placed).length;
    status(`${placed} of ${all.length} clips placed in ${clock((Date.now() - state.started) / 1000)}`);
    showReport(null, view.notes);
  } catch (e) {
    status(String(e?.message || e), true);
  }
  state.busy = false;
  state.started = 0;
  render();
};

function showReport(saved, notes) {
  const r = $('report');
  r.replaceChildren();
  const items = [];
  if (saved) {
    r.append(el('div', { className: 'summary', textContent: saved.summary }));
    for (const g of saved.gaps || []) items.push(el('li', { className: 'gap', textContent: `No footage on air: ${g}` }));
  }
  for (const n of notes || []) items.push(el('li', { textContent: n }));
  if (items.length) r.append(el('ul', {}, ...items));
  r.hidden = !r.childNodes.length;
}

$('save').onclick = async () => {
  try {
    const saved = await api.Save({
      names: state.folders.map((f) => f.name),
      audio: state.audio,
      minShot: Math.max(0, parseInt($('min-shot').value, 10) || 0),
    });
    if (!saved) return;
    status(`Saved ${saved.path}`);
    showReport(saved, saved.notes);
  } catch (e) {
    status(String(e?.message || e), true);
  }
};

Events.On('progress', (e) => {
  if (!state.busy) return;
  const p = e.data;
  state.progress = p;
  if (p.folder >= 0 && state.folders[p.folder]) {
    const live = (state.folders[p.folder].live[p.clip] ||= {});
    live.state = p.state;
    if (p.state === 'heard') live.wave = bytes(p.wave);
    if (p.state === 'placed') { live.offset = p.offset; live.method = p.method; }
    render();
  } else if (p.folder === -1) {
    state.mixLive = { ...state.mixLive, state: p.state };
    render();
  } else {
    renderProgress();
  }
});

// Thumbnails are made in the background after a folder is added.
Events.On('thumb', (e) => {
  const { path, url } = e.data;
  thumbs.set(path, url);
  for (const n of document.querySelectorAll('.thumb')) {
    if (n.dataset.path === path) n.style.backgroundImage = `url("${url}")`;
  }
});

// Dropped files arrive from the app; the runtime highlights the window while
// they are dragged over it.
Events.On('opened', (e) => take(e.data));

render();
api.Ready();
