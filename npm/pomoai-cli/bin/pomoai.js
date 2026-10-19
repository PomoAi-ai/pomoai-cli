#!/usr/bin/env node
'use strict';
// Runs the platform binary: bin/<platform>-<arch>/ in the release tarball, otherwise the
// pomoai-cli-<platform>-<arch> optionalDependency of the registry package.
const { spawnSync } = require('node:child_process');
const fs = require('node:fs');
const os = require('node:os');
const path = require('node:path');

const pkg = `pomoai-cli-${process.platform}-${process.arch}`;
const exe = process.platform === 'win32' ? 'pomoai.exe' : 'pomoai';

let bin = path.join(__dirname, `${process.platform}-${process.arch}`, exe);
try {
  if (!fs.existsSync(bin)) bin = require.resolve(`${pkg}/bin/${exe}`);
} catch {
  process.stderr.write(
    `pomoai: could not find the binary package ${pkg}.\n` +
      'Either this platform is not supported (darwin/linux/win32 on x64/arm64), or optional\n' +
      'dependencies were skipped (--omit=optional, --no-optional, some pnpm/Yarn PnP setups).\n' +
      'Reinstall from the release tarball, which bundles every platform:\n' +
      '  npm i -g https://github.com/PomoAi-ai/pomoai-cli/releases/latest/download/pomoai-cli.tgz\n',
  );
  process.exit(1);
}

const r = spawnSync(bin, process.argv.slice(2), { stdio: 'inherit' });
if (r.error) {
  process.stderr.write(`pomoai: failed to run ${bin}: ${r.error.message}\n`);
  process.exit(1);
}
if (r.signal) process.exit(128 + (os.constants.signals[r.signal] || 0));
process.exit(r.status);
