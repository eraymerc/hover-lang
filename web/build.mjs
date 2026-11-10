// ─────────────────────────────────────────────────────────────────────────────
// make web-playground — assemble the browser playground's assets
//
//   node web/build.mjs --out <dir> --frontend <hover-web.wasm> --wasm-exec <wasm_exec.js>
//
// The playground compiles Hover entirely in the browser:
//
//   .hvr ──(hover-web.wasm: this repo's Go front end)──▶ sim.cpp
//   sim.cpp ──(YoWASP Clang/LLD, WebAssembly)──▶ sim.wasm, linked against
//   libhover_runtime.a, which this script prebuilds for wasm32-wasip1.
//
// The runtime is built with the SAME Clang the browser runs — YoWASP, not
// Zig — for the reason runtimebuild.go gives for native builds: sim.cpp and
// the runtime must come from one toolchain, or their C++ ABIs need not agree.
// It is the slow step (a few minutes), so it is skipped while the archive is
// newer than every runtime source.
//
// Large files are written gzipped. The page's worker inflates them itself
// (DecompressionStream), so the download is small whatever compression the
// web host does or does not apply. manifest.json records what a first run
// actually downloads, which is the number the page shows.
// ─────────────────────────────────────────────────────────────────────────────

import fs from 'node:fs';
import path from 'node:path';
import zlib from 'node:zlib';
import { fileURLToPath } from 'node:url';

const HERE = path.dirname(fileURLToPath(import.meta.url));
const REPO = path.resolve(HERE, '..');
const RUNTIME = path.join(REPO, 'runtime');
const STDLIB = path.join(REPO, 'stdlib');

// Examples offered in the page's picker. Kept to decks that finish in well
// under a second natively: the browser runs them a few times slower, and a
// playground that hangs on its own examples makes a poor first impression.
const EXAMPLES = [
  { id: 'rectifier', title: 'Full-bridge rectifier', file: 'examples/Diode/rectifier.hvr' },
  { id: 'npn_amp', title: 'NPN common-emitter amplifier', file: 'examples/BJT/npn_amp.hvr' },
  { id: 'pnp_amp', title: 'PNP amplifier', file: 'examples/BJT/pnp_amp.hvr' },
  { id: 'bjt_opamp', title: 'Discrete BJT op-amp', file: 'examples/BJT/opamp.hvr' },
  { id: 'nmos_amp', title: 'NMOS common-source amplifier', file: 'examples/MOSFET/nmos_amp.hvr' },
  { id: 'cmos_inverter', title: 'CMOS inverter', file: 'examples/MOSFET/cmos_inverter.hvr' },
  { id: 'led_ammeter', title: 'White LED with an ammeter', file: 'examples/Optoelectronics/leds/white_led_ammeter.hvr' },
  { id: 'optocoupler', title: 'Optocoupler', file: 'examples/Optoelectronics/phototransistor/optocoupler.hvr' },
  { id: 'pid', title: 'PID controller (10 kHz clock)', file: 'examples/PIDController/pid_controller.hvr' },
];

// Must match the flags the worker compiles sim.cpp with.
// The floating-point and overflow rules match cxxOpt in runtimebuild.go (no FMA
// contraction, wrapping int overflow). That removes one source of difference
// from a native run; libm and vectorisation can still differ in the last bit.
const CXXFLAGS = ['-std=c++20', '-O2', '-ffp-contract=off', '-fwrapv', '-fno-strict-aliasing', '-fno-exceptions', '-w'];

function parseArgs(argv) {
  const opts = {};
  for (let i = 0; i < argv.length; i++) {
    const key = argv[i].replace(/^--/, '');
    opts[key] = argv[++i];
  }
  for (const req of ['out', 'frontend', 'wasm-exec']) {
    if (!opts[req]) throw new Error(`missing --${req}`);
  }
  return opts;
}

const log = (...a) => console.log('[web]', ...a);

function walk(dir, filter, rel = '') {
  const out = [];
  for (const e of fs.readdirSync(path.join(dir, rel), { withFileTypes: true })) {
    const r = rel ? `${rel}/${e.name}` : e.name;
    if (e.isDirectory()) out.push(...walk(dir, filter, r));
    else if (filter(r)) out.push(r);
  }
  return out;
}

function writeGz(dst, data) {
  const gz = zlib.gzipSync(data, { level: 9 });
  fs.mkdirSync(path.dirname(dst), { recursive: true });
  fs.writeFileSync(dst + '.gz', gz);
  return { raw: data.length, gz: gz.length };
}

function copy(src, dst) {
  fs.mkdirSync(path.dirname(dst), { recursive: true });
  fs.copyFileSync(src, dst);
  return fs.statSync(dst).size;
}

// ── Runtime → libhover_runtime.a (wasm32-wasip1, YoWASP Clang) ─────────────

// YoWASP's virtual filesystem treats a ".." path segment as a no-op instead
// of going up a directory, so `#include "../vm/vm.hpp"` from runtime/mna/x.h
// looks for runtime/mna/vm/vm.hpp. In the copy handed to Clang, every such
// include is rewritten to its normalized path relative to runtime/, which
// -Iruntime then finds. Sources on disk are untouched.
const PARENT_INCLUDE = /(#\s*include\s*")((?:\.\.\/)+[^"]+)"/g;

function runtimeTree() {
  const build = (dir, virt) => {
    const out = {};
    for (const e of fs.readdirSync(dir, { withFileTypes: true })) {
      const p = path.join(dir, e.name);
      if (e.isDirectory()) {
        if (e.name !== 'build') out[e.name] = build(p, `${virt}/${e.name}`);
        continue;
      }
      const buf = fs.readFileSync(p);
      const text = buf.toString('latin1');
      out[e.name] = text.includes('../')
        ? Buffer.from(text.replace(PARENT_INCLUDE, (all, head, rel) => {
            const norm = path.posix.normalize(path.posix.join(virt, rel));
            return norm.startsWith('runtime/') ? `${head}${norm.slice('runtime/'.length)}"` : all;
          }), 'latin1')
        : buf;
    }
    return out;
  };
  return { runtime: build(RUNTIME, 'runtime') };
}

function newestRuntimeInput() {
  let newest = 0;
  for (const f of walk(RUNTIME, (r) => !r.startsWith('build/') && /\.(cpp|hpp|h)$/.test(r) && !r.startsWith('Eigen/'))) {
    newest = Math.max(newest, fs.statSync(path.join(RUNTIME, f)).mtimeMs);
  }
  return newest;
}

async function buildRuntime(archivePath) {
  if (fs.existsSync(archivePath) && fs.statSync(archivePath).mtimeMs > newestRuntimeInput()) {
    log('runtime archive is up to date');
    return fs.readFileSync(archivePath);
  }
  const { runClang, runLLVM } = await import('@yowasp/clang');
  // Capture diagnostics instead of discarding them, so a failure says why.
  const capture = () => {
    let text = '';
    const sink = (b) => { if (b) text += new TextDecoder().decode(b); };
    return { opts: { stdout: sink, stderr: sink, decodeASCII: false }, text: () => text };
  };

  const tree = runtimeTree();
  const sources = walk(RUNTIME, (r) => r.endsWith('.cpp') && !r.startsWith('build/') && !r.startsWith('Eigen/'));
  const objects = {};
  log(`building runtime for wasm32-wasip1 (${sources.length} files, a few minutes)...`);
  for (const src of sources) {
    const obj = src.replace(/\//g, '_').replace(/\.cpp$/, '.o');
    const c = capture();
    const t = Date.now();
    try {
      const out = await runClang(['clang++', ...CXXFLAGS, '-Iruntime', '-Iruntime/Eigen',
        '-c', `runtime/${src}`, '-o', obj], tree, c.opts);
      objects[obj] = out[obj];
    } catch (e) {
      throw new Error(`compiling runtime/${src} failed:\n${c.text()}`);
    }
    log(`  ${src} (${((Date.now() - t) / 1000).toFixed(1)}s)`);
  }
  const c = capture();
  let ar;
  try {
    ar = await runLLVM(['ar', 'rcs', 'libhover_runtime.a', ...Object.keys(objects)], objects, c.opts);
  } catch (e) {
    throw new Error(`archiving runtime failed:\n${c.text()}`);
  }
  const archive = ar['libhover_runtime.a'];
  fs.mkdirSync(path.dirname(archivePath), { recursive: true });
  fs.writeFileSync(archivePath, archive);
  return Buffer.from(archive);
}

// ── Main ────────────────────────────────────────────────────────────────────

async function main() {
  const opts = parseArgs(process.argv.slice(2));
  const out = path.resolve(opts.out);
  const manifest = { generated: new Date().toISOString(), cxxflags: CXXFLAGS, files: {} };
  const note = (name, sizes) => { manifest.files[name] = sizes; };

  fs.mkdirSync(out, { recursive: true });

  // Front end and its JS glue (wasm_exec.js must match the Go that built it).
  note('hover-web.wasm', writeGz(path.join(out, 'hover-web.wasm'), fs.readFileSync(opts.frontend)));
  copy(opts['wasm-exec'], path.join(out, 'wasm_exec.js'));

  // Standard library, as one JSON object keyed by path within stdlib/.
  const stdlib = {};
  for (const f of walk(STDLIB, (r) => r.endsWith('.hvr'))) {
    stdlib[f] = fs.readFileSync(path.join(STDLIB, f), 'utf8');
  }
  fs.writeFileSync(path.join(out, 'stdlib.json'), JSON.stringify(stdlib));
  log(`stdlib: ${Object.keys(stdlib).length} files`);

  // Examples.
  const examples = EXAMPLES.map((e) => ({ ...e, source: fs.readFileSync(path.join(REPO, e.file), 'utf8') }));
  fs.writeFileSync(path.join(out, 'examples.json'), JSON.stringify(examples));

  // Runtime: the archive, plus the one header sim.cpp includes.
  const archiveCache = path.join(HERE, 'build', 'libhover_runtime.a');
  const archive = await buildRuntime(archiveCache);
  note('runtime/libhover_runtime.a', writeGz(path.join(out, 'runtime', 'libhover_runtime.a'), archive));
  copy(path.join(RUNTIME, 'hover_sim.hpp'), path.join(out, 'runtime', 'hover_sim.hpp'));

  // Clang/LLD. bundle.js is imported as a module and stays plain; the rest is
  // what it fetches, which the worker serves to it inflated.
  const clangGen = path.join(HERE, 'node_modules', '@yowasp', 'clang', 'gen');
  copy(path.join(clangGen, 'bundle.js'), path.join(out, 'clang', 'bundle.js'));
  for (const f of fs.readdirSync(clangGen)) {
    if (f.endsWith('.wasm') || f.endsWith('.tar')) {
      note(`clang/${f}`, writeGz(path.join(out, 'clang', f), fs.readFileSync(path.join(clangGen, f))));
    }
  }
  // The package ships no separate LICENSE file; its README carries the
  // license section (Apache 2, as LLVM's), so that travels with it.
  copy(path.join(HERE, 'node_modules', '@yowasp', 'clang', 'README.md'), path.join(out, 'clang', 'README.md'));

  // Charts.
  const uplot = path.join(HERE, 'node_modules', 'uplot');
  copy(path.join(uplot, 'dist', 'uPlot.iife.min.js'), path.join(out, 'uplot', 'uPlot.iife.min.js'));
  copy(path.join(uplot, 'dist', 'uPlot.min.css'), path.join(out, 'uplot', 'uPlot.min.css'));
  copy(path.join(uplot, 'LICENSE'), path.join(out, 'uplot', 'LICENSE'));

  // What a first Run downloads: every gzipped file (the page loads the rest
  // up front, and they are small).
  manifest.firstRunBytes = Object.values(manifest.files).reduce((n, f) => n + f.gz, 0);
  fs.writeFileSync(path.join(out, 'manifest.json'), JSON.stringify(manifest, null, 2));
  log(`first run downloads ${(manifest.firstRunBytes / 1048576).toFixed(1)} MB (gzip)`);
  log(`done: ${out}`);
}

main().catch((e) => {
  console.error('[web] ' + (e.stack || e));
  process.exit(1);
});
