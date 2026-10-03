// Checks the web client, which has no build step to catch mistakes: every module parses, every
// named import exists in the module it comes from, and every named import is used.
//
//   node server/scripts/check-web.mjs [server/internal/web/static]
//
// Exits non-zero when something is wrong (CI runs it).

import { execFileSync } from 'node:child_process';
import { copyFileSync, mkdtempSync, readFileSync, readdirSync, rmSync, statSync } from 'node:fs';
import { tmpdir } from 'node:os';
import { dirname, join, relative, resolve } from 'node:path';
import { fileURLToPath } from 'node:url';

const root = resolve(process.argv[2] || join(dirname(fileURLToPath(import.meta.url)), '..', 'internal', 'web', 'static'));
const files = [];
const walk = (d) => {
  for (const f of readdirSync(d)) {
    const p = join(d, f);
    if (statSync(p).isDirectory()) {
      if (f !== 'vendor') walk(p);
    } else if (p.endsWith('.js')) {
      files.push(p);
    }
  }
};
walk(join(root, 'js'));

const problems = [];
const name = (f) => relative(root, f);

// Syntax: as an ES module (the client is served as modules; .mjs makes node read it as one).
const tmp = mkdtempSync(join(tmpdir(), 'check-web-'));
try {
  for (const f of files) {
    const copy = join(tmp, 'm.mjs');
    copyFileSync(f, copy);
    try {
      execFileSync(process.execPath, ['--check', copy], { stdio: 'pipe' });
    } catch (e) {
      problems.push(`${name(f)}: ${String(e.stderr).split('\n').filter(Boolean).slice(0, 4).join(' | ').replaceAll(copy, name(f))}`);
    }
  }
} finally {
  rmSync(tmp, { recursive: true, force: true });
}

const exportsOf = new Map();
const exported = (file) => {
  if (!exportsOf.has(file)) {
    const src = readFileSync(file, 'utf8');
    const names = new Set();
    for (const m of src.matchAll(/export\s+(?:async\s+)?(?:function\*?|const|let|var|class)\s+([A-Za-z_$][\w$]*)/g)) names.add(m[1]);
    for (const m of src.matchAll(/export\s*\{([^}]*)\}/g)) {
      for (const x of m[1].split(',')) {
        const n = x.trim().split(/\s+as\s+/).pop().trim();
        if (n) names.add(n);
      }
    }
    exportsOf.set(file, names);
  }
  return exportsOf.get(file);
};

for (const f of files) {
  const src = readFileSync(f, 'utf8');
  for (const m of src.matchAll(/import\s*\{([^}]*)\}\s*from\s*(['"])([^'"]+)\2/g)) {
    const from = m[3];
    const target = resolve(dirname(f), from);
    let names;
    try {
      names = exported(target);
    } catch {
      problems.push(`${name(f)}: imports ${from}, which does not exist`);
      continue;
    }
    for (const spec of m[1].split(',').map((x) => x.trim()).filter(Boolean)) {
      const [orig, local = orig] = spec.split(/\s+as\s+/).map((x) => x.trim());
      if (!names.has(orig)) problems.push(`${name(f)}: '${orig}' is not exported by ${from}`);
      // A use is the name on its own: not part of a longer name, not a property (a.name), but
      // spread (...name) counts.
      const uses = src.split(new RegExp(`(?<![\\w$])(?<!(?<!\\.\\.)\\.)${local.replaceAll('$', '\\$')}(?![\\w$])`)).length - 1;
      if (uses < 2) problems.push(`${name(f)}: '${local}' is imported but not used`);
    }
  }
}

if (problems.length) {
  console.log(problems.join('\n'));
  console.log(`${problems.length} problem(s) in ${files.length} files`);
  process.exit(1);
}
console.log(`web client ok (${files.length} files)`);
