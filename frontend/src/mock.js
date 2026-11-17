// Stand-in for the app when the page is opened in a browser with ?mock, for
// working on the layout. The real window never loads this file.
const wait = (ms) => new Promise((r) => setTimeout(r, ms));
const handlers = {};
export const Events = { On: (name, fn) => { handlers[name] = fn; } };
const emit = (name, data) => handlers[name]?.({ name, data });

const LENGTH = 6572;
const project = { path: '/x/show.drp', name: '19_6_GS_Kranj_switcher', detail: '1080p60 · 1:49:32 · 346 cuts',
  inputs: [{ index: 1, name: '1 - din pero' }, { index: 2, name: '2 - levo' }, { index: 3, name: '3 - center' }, { index: 4, name: '4 - total' }] };

// Camera folders: clips with durations and where they really start.
const cameras = [
  { name: 'Dinamicna', input: 1, clips: [['C0024.MP4', 6311, 267.45]] },
  { name: 'Dirigent', input: -1, clips: [['C0001.MP4', 169, 86.6], ['C0002.MP4', 276, 272.8], ['C0003.MP4', 1790, 1337.6], ['C0004.MP4', 1685, 3269.3], ['C0005.MP4', 1624, 4955.8]] },
  { name: 'FOH', input: 3, clips: [['20260619_cam1_2681.MP4', 6322, 266.44]] },
  { name: 'Total', input: 4, clips: [['C1781.MP4', 6263, 325.86]] },
  { name: 'Left', input: 2, clips: [['C1475.MP4', 11, -2209.5], ['C1476.MP4', 6317, 266.45]] },
];

let seed = 7;
const rnd = () => ((seed = (seed * 16807) % 2147483647) / 2147483647);
function wave(seconds) {
  const out = new Uint8Array(Math.ceil(seconds));
  let level = 120;
  for (let i = 0; i < out.length; i++) {
    level += (rnd() - 0.5) * 30;
    level = Math.min(235, Math.max(40, level));
    out[i] = Math.max(0, Math.min(255, level + (rnd() - 0.5) * 50));
  }
  return out;
}
function thumb(hue) {
  const c = document.createElement('canvas');
  c.width = 160; c.height = 90;
  const g = c.getContext('2d');
  const grad = g.createLinearGradient(0, 0, 160, 90);
  grad.addColorStop(0, `hsl(${hue} 35% 22%)`);
  grad.addColorStop(1, `hsl(${(hue + 40) % 360} 45% 45%)`);
  g.fillStyle = grad; g.fillRect(0, 0, 160, 90);
  g.fillStyle = 'rgba(255,255,255,.75)';
  g.beginPath(); g.arc(80, 52, 14, 0, 7); g.fill(); g.fillRect(62, 64, 36, 26);
  return c.toDataURL('image/jpeg', 0.8);
}

let n = 0;
const waves = {};
export const App = {
  PickProject: async () => ({ project }),
  PickFolder: async () => {
    const cam = cameras[n++ % cameras.length];
    const files = cam.clips.map(([name, duration]) => ({ path: `/x/${cam.name}/${name}`, name, duration, thumb: '' }));
    // Like the real app, the first thumbnails can arrive before the folder.
    files.forEach((f, i) => (i === 0 ? emit('thumb', { path: f.path, url: thumb(n * 70) }) : setTimeout(() => emit('thumb', { path: f.path, url: thumb(n * 70 + i * 20) }), 150 * i)));
    const total = cam.clips.reduce((s, c) => s + c[1], 0);
    return { folders: [{ path: `/x/${cam.name}`, name: cam.name, detail: `${files.length} clip${files.length > 1 ? 's' : ''} · ${Math.floor(total / 3600)}:${String(Math.floor(total / 60) % 60).padStart(2, '0')}:00`, files }] };
  },
  PickMix: async () => ({ mix: { path: '/x/mix.wav', name: 'mix_orkestri-GS KRANJ_2026.wav' } }),
  Cancel: () => {},
  Ready: () => {},
  Sync: async (req) => {
    const cams = req.folders.map((f) => cameras.find((c) => c.name === f.name) || cameras[0]);
    const all = cams.flatMap((c, fi) => c.clips.map((clip, ci) => ({ fi, ci, clip })));
    const progress = (stage, done, total, text, extra = {}) => emit('progress', { stage, done, total, text, folder: -2, clip: -2, ...extra });
    progress('sound', 0, all.length + 1, 'Listening');
    for (const [i, a] of all.entries()) {
      progress('sound', i, all.length + 1, 'Listening', { folder: a.fi, clip: a.ci, state: 'listening' });
      await wait(120);
      waves[`${a.fi}/${a.ci}`] = wave(a.clip[1]);
      progress('sound', i + 1, all.length + 1, 'Listening', { folder: a.fi, clip: a.ci, state: 'heard', wave: waves[`${a.fi}/${a.ci}`] });
    }
    waves.mix = wave(5866);
    progress('sound', all.length + 1, all.length + 1, 'Listening', { folder: -1, clip: 0, state: 'heard' });
    for (const [fi, c] of cams.entries()) {
      progress('inputs', fi, cams.length, `${c.name}: searching the first 15 min`, { folder: fi, clip: 0, state: 'searching' });
      await wait(350);
      progress('inputs', fi + 1, cams.length, 'Finding cameras', { folder: fi, clip: 0, state: '' });
    }
    for (const [i, a] of all.entries()) {
      progress('match', i, all.length, 'Matching', { folder: a.fi, clip: a.ci, state: 'matching' });
      await wait(90);
      progress('match', i + 1, all.length, 'Matching', { folder: a.fi, clip: a.ci, state: 'placed', offset: a.clip[2], method: cams[a.fi].input > 0 ? 'picture' : 'sound' });
    }
    const program = [];
    const angles = cams.map((c) => c.input);
    for (let t = 0, last = -2; t < LENGTH;) {
      let angle = Math.floor(rnd() * cams.length);
      if (angle === last) angle = (angle + 1) % cams.length;
      const len = 4 + rnd() * 30;
      program.push({ start: t, end: Math.min(LENGTH, t + len), angle: angles[angle] > 0 ? angle : -1 });
      t += len; last = angle;
    }
    return {
      notes: ['Dirigent matches no switcher input; placed by sound only.'],
      length: LENGTH, program,
      mix: { path: '/x/mix.wav', file: 'mix.wav', duration: 5866, placed: true, offset: 518.77, method: 'sound', exact: false, warning: '', wave: waves.mix },
      angles: cams.map((c, fi) => ({ path: req.folders[fi].path, input: c.input, clips: c.clips.map(([file, duration, offset], ci) => {
        const outside = offset + duration < 0;
        return { path: `/x/${c.name}/${file}`, file, duration, wave: waves[`${fi}/${ci}`],
          placed: true, offset, exact: c.input > 0 && !outside,
          method: c.input > 0 && !outside ? (c.name === 'Total' ? 'picture · 3 parts' : 'picture') : outside ? 'clock' : 'sound',
          warning: outside ? 'placed by camera clock, may be seconds off' : '' };
      }) })),
    };
  },
  Save: async () => ({ path: '/Users/me/Movies/show.fcpxml', summary: '25 fps · 01:49:31:23 · 338 cuts · 89 dissolves',
    gaps: ['FOH  00:00:00:00 – 00:03:48:08', 'Total  00:03:48:08 – 00:05:25:23'], notes: [] }),
};
