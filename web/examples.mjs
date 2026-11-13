// ─────────────────────────────────────────────────────────────────────────────
// The playground's example list — every runnable deck under examples/.
//
// Discovered, not listed by hand: a deck is offered when it has a
// `module main` and imports nothing but the standard library (the browser
// compiler sees one main.hvr plus stdlib.json, so a deck that pulls in its own
// project files or an hpm package cannot run there). Title and summary come
// from the deck's header comment:
//
//   //  HVR Testbench — Full-Bridge Rectifier
//   //  Validates: AC to DC conversion, diode commutation, and capacitive ...
//
// ("HVR Example — …" / "Demonstrates: …" work the same way). The category is
// the deck's top-level folder.
//
// A deck may have a hand-drawn schematic beside it, `<name>.schematic.svg`
// (drawn to the rules in the playground's schematic spec: currentColor only,
// classes for fill/dash/net labels, no <style> or scripts). The playground
// inlines it under the plot, so it is checked here for anything that would
// leak into or script the page.
//
// Slow decks are still offered, flagged with their native run time so the page
// can warn before the browser — several times slower — spends minutes on them.
// The times below are measured natively (compile + run, `hover <deck>`), so
// refresh them when a deck or the solver changes noticeably; a deck missing
// from the table is simply not flagged.
//
// Used by build.mjs; `node web/examples.mjs` prints the list without building.
// ─────────────────────────────────────────────────────────────────────────────

import fs from 'node:fs';
import path from 'node:path';
import { fileURLToPath } from 'node:url';

// Folder → heading in the picker, in the order the picker shows them. The
// first deck of the first category is what an empty editor opens with.
const CATEGORIES = [
  ['Diode', 'Diodes'],
  ['BJT', 'BJTs'],
  ['MOSFET', 'MOSFETs'],
  ['Optoelectronics', 'Optoelectronics'],
  ['PIDController', 'Control'],
  ['DCMotor', 'DC motors'],
  ['Inverter', 'Inverters'],
  ['PMSM', 'Motor control'],
];

// Native wall time in seconds (compile + run, about 0.5 s of it compile),
// measured 2026-11-11. Every deck not listed finishes in under 2 s; only decks
// worth a warning need an entry, see SLOW_SECONDS.
const NATIVE_SECONDS = {
  'Inverter/switched_h_bridge.hvr': 2.4,
  'Inverter/switched_three_phase.hvr': 4.5,
  'DCMotor/h_bridge.hvr': 5.0,
  'MOSFET/mosfet_h_bridge.hvr': 6.3,
  'MOSFET/h_bridge_inverter.hvr': 26.6,
  'PMSM/foc.hvr': 26.5,
};

// Above this native time the picker marks a deck as slow.
const SLOW_SECONDS = 4;

function walk(dir, rel = '') {
  const out = [];
  for (const e of fs.readdirSync(path.join(dir, rel), { withFileTypes: true })) {
    const r = rel ? `${rel}/${e.name}` : e.name;
    if (e.isDirectory()) out.push(...walk(dir, r));
    else if (r.endsWith('.hvr')) out.push(r);
  }
  return out;
}

// Imports a browser build cannot resolve: a quoted path (a project file), or
// an angle-bracket root that is not a stdlib top-level folder (an hpm package).
function foreignImports(source, stdlibRoots) {
  const bad = [];
  const re = /^\s*(?:import|from)\s+(?:"([^"]*)"|<([^>]*)>)/gm;
  let m;
  while ((m = re.exec(source))) {
    if (m[1] !== undefined) bad.push(`"${m[1]}"`);
    else if (!stdlibRoots.has(m[2].split('/')[0])) bad.push(`<${m[2]}>`);
  }
  return bad;
}

// Title and summary from the header comment (see the top of this file).
function header(source) {
  const lines = source.split('\n');
  let title = null;
  const summary = [];
  let inSummary = false;
  for (const raw of lines) {
    if (!raw.startsWith('//')) {
      if (title) break;          // header ended
      continue;
    }
    const text = raw.replace(/^\/\/\s?/, '').trim();
    if (!title) {
      const t = text.match(/^HVR\s+(?:Testbench|Example)\s+[—–-]+\s+(.+)$/);
      if (t) title = t[1].trim();
      continue;
    }
    const s = text.match(/^(?:Validates|Demonstrates):\s*(.*)$/);
    if (s) { inSummary = true; summary.push(s[1]); continue; }
    if (inSummary) {
      if (!text || /^[─━=-]{3,}/.test(text)) break;
      summary.push(text);
    }
  }
  return { title, summary: summary.join(' ').replace(/\s+/g, ' ').trim() };
}

// The deck's schematic, or null. Inlined into the page, so refuse anything that
// could style or script it rather than trust the file.
function schematic(exDir, rel, log) {
  const file = path.join(exDir, rel.replace(/\.hvr$/, '.schematic.svg'));
  if (!fs.existsSync(file)) return null;
  const svg = fs.readFileSync(file, 'utf8').replace(/^\s*<\?xml[^>]*>\s*/, '').trim();
  const bad = [/<script/i, /<style/i, /<foreignObject/i, /\son\w+\s*=/i, /href\s*=/i, /<image/i, /<use\b/i]
    .find((re) => re.test(svg));
  if (!svg.startsWith('<svg') || bad) {
    log(`examples: ignoring ${path.relative(exDir, file)} (${bad ? 'contains ' + bad : 'not an <svg>'})`);
    return null;
  }
  return svg;
}

function fallbackTitle(file) {
  const base = path.basename(file, '.hvr').replace(/[_-]+/g, ' ');
  return base.charAt(0).toUpperCase() + base.slice(1);
}

export function collectExamples(repo, { log = () => {} } = {}) {
  const exDir = path.join(repo, 'examples');
  // The stdlib's top-level folders, plus "stdlib" itself (`import
  // <stdlib/optoelectronics/leds/>` is the long spelling of the same thing).
  const stdlibRoots = new Set(['stdlib',
    ...fs.readdirSync(path.join(repo, 'stdlib'), { withFileTypes: true })
      .filter((e) => e.isDirectory()).map((e) => e.name)]);
  const order = new Map(CATEGORIES.map(([dir], i) => [dir, i]));
  const label = new Map(CATEGORIES);

  const list = [];
  for (const rel of walk(exDir)) {
    const source = fs.readFileSync(path.join(exDir, rel), 'utf8');
    if (!/\bmodule\s+main\b/.test(source)) continue;           // a library file
    const bad = foreignImports(source, stdlibRoots);
    if (bad.length) { log(`examples: skipping ${rel} (imports ${bad.join(', ')})`); continue; }
    const dir = rel.split('/')[0];
    const { title, summary } = header(source);
    const seconds = NATIVE_SECONDS[rel];
    const svg = schematic(exDir, rel, log);
    list.push({
      id: rel.replace(/\.hvr$/, '').toLowerCase(),
      title: title || fallbackTitle(rel),
      summary,
      category: label.get(dir) || dir,
      file: `examples/${rel}`,
      ...(seconds !== undefined ? { seconds } : {}),
      ...(seconds !== undefined && seconds > SLOW_SECONDS ? { slow: true } : {}),
      ...(svg ? { schematic: svg } : {}),
      source,
    });
  }
  const rank = (e) => order.get(e.file.split('/')[1]) ?? CATEGORIES.length;
  list.sort((a, b) => rank(a) - rank(b) || a.category.localeCompare(b.category) || a.file.localeCompare(b.file));
  return list;
}

// `node web/examples.mjs` — print what the picker would offer.
if (process.argv[1] && path.resolve(process.argv[1]) === fileURLToPath(import.meta.url)) {
  const repo = path.resolve(path.dirname(fileURLToPath(import.meta.url)), '..');
  for (const e of collectExamples(repo, { log: (m) => console.log(m) })) {
    const t = e.seconds !== undefined ? ` ${e.seconds}s${e.slow ? ' SLOW' : ''}` : '';
    console.log(`${e.category.padEnd(16)} ${e.id.padEnd(44)} ${e.schematic ? '▣' : ' '} ${e.title}${t}`);
  }
}
