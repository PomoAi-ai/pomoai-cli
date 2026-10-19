// Build npm packages from GoReleaser's dist/. Run from CI after `goreleaser release`;
// needs GITHUB_REF_NAME (vX.Y.Z).
// Always: dist/npm/pomoai-cli.tgz, one package with all 6 binaries, uploaded to the GitHub
// Release so `npm i -g <release url>` works without the npm registry.
// With --publish (needs npm auth): also publish pomoai-cli + 6 platform packages to the
// registry. Idempotent: a package version that already exists there is skipped.
import { spawnSync } from 'node:child_process';
import { chmodSync, copyFileSync, cpSync, mkdirSync, readFileSync, renameSync, rmSync, writeFileSync } from 'node:fs';
import { join } from 'node:path';
import { fileURLToPath } from 'node:url';

process.chdir(fileURLToPath(new URL('..', import.meta.url)));

const ref = process.env.GITHUB_REF_NAME ?? '';
if (!/^v\d+\.\d+\.\d+(-[0-9A-Za-z.-]+)?$/.test(ref)) {
  throw new Error(`GITHUB_REF_NAME must be a release tag like v1.2.3, got "${ref}"`);
}
const version = ref.slice(1);
const distTag = version.includes('-') ? 'next' : 'latest';

const OS = { darwin: 'darwin', linux: 'linux', windows: 'win32' };
const CPU = { amd64: 'x64', arm64: 'arm64' };
const meta = {
  license: 'MIT',
  homepage: 'https://github.com/PomoAi-ai/pomoai-cli',
  repository: { type: 'git', url: 'git+https://github.com/PomoAi-ai/pomoai-cli.git' },
};

const binaries = JSON.parse(readFileSync('dist/artifacts.json', 'utf8')).filter(
  (a) => a.type === 'Binary' && OS[a.goos] && CPU[a.goarch],
);
if (binaries.length !== 6) {
  throw new Error(`expected 6 binaries in dist/artifacts.json, found ${binaries.length}`);
}

const out = 'dist/npm';
rmSync(out, { recursive: true, force: true });

const platformPkgs = binaries.map((a) => {
  const os = OS[a.goos];
  const cpu = CPU[a.goarch];
  const name = `pomoai-cli-${os}-${cpu}`;
  const dir = join(out, `cli-${os}-${cpu}`);
  const exe = os === 'win32' ? 'pomoai.exe' : 'pomoai';
  mkdirSync(join(dir, 'bin'), { recursive: true });
  copyFileSync(a.path, join(dir, 'bin', exe));
  chmodSync(join(dir, 'bin', exe), 0o755);
  const pkg = {
    name,
    version,
    description: `pomoai CLI binary for ${os}-${cpu}`,
    os: [os],
    cpu: [cpu],
    files: ['bin/'],
    ...meta,
  };
  writeFileSync(join(dir, 'package.json'), JSON.stringify(pkg, null, 2) + '\n');
  return { name, dir, key: `${os}-${cpu}` };
});

const mainDir = join(out, 'cli');
cpSync('npm/pomoai-cli', mainDir, { recursive: true });
const mainPkg = JSON.parse(readFileSync(join(mainDir, 'package.json'), 'utf8'));
mainPkg.version = version;
mainPkg.optionalDependencies = Object.fromEntries(platformPkgs.map((p) => [p.name, version]));
writeFileSync(join(mainDir, 'package.json'), JSON.stringify(mainPkg, null, 2) + '\n');

function npm(args, opts = {}) {
  return spawnSync('npm', args, { encoding: 'utf8', ...opts });
}

function publish({ name, dir }) {
  const view = npm(['view', `${name}@${version}`, 'version']);
  if (view.status === 0 && view.stdout.trim() === version) {
    console.log(`skip ${name}@${version}: already published`);
    return;
  }
  const r = npm(['publish', '--access', 'public', '--provenance', '--tag', distTag], { cwd: dir, stdio: 'inherit' });
  if (r.status !== 0) throw new Error(`npm publish ${name}@${version} failed (exit ${r.status})`);
}

// Single-file package: every binary under bin/<os>-<cpu>/, picked by bin/pomoai.js.
const fatDir = join(out, 'fat');
cpSync('npm/pomoai-cli', fatDir, { recursive: true });
for (const p of platformPkgs) cpSync(join(p.dir, 'bin'), join(fatDir, 'bin', p.key), { recursive: true });
writeFileSync(join(fatDir, 'package.json'), JSON.stringify({ ...JSON.parse(readFileSync(join(fatDir, 'package.json'), 'utf8')), version }, null, 2) + '\n');
const pack = npm(['pack', '--pack-destination', '..'], { cwd: fatDir });
if (pack.status !== 0) throw new Error(`npm pack failed: ${pack.stderr}`);
renameSync(join(out, pack.stdout.trim().split('\n').pop()), join(out, 'pomoai-cli.tgz'));
console.log(`built ${join(out, 'pomoai-cli.tgz')}`);

if (process.argv.includes('--publish')) {
  // Platform packages first so the main package never points at missing versions.
  for (const p of platformPkgs) publish(p);
  publish({ name: mainPkg.name, dir: mainDir });
}
