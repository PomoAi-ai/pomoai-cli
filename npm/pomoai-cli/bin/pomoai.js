#!/usr/bin/env node
'use strict';
// Runs the platform binary shipped in pomoai-cli-<platform>-<arch> (an optionalDependency).
const { spawnSync } = require('node:child_process');
const os = require('node:os');

const pkg = `pomoai-cli-${process.platform}-${process.arch}`;
const exe = process.platform === 'win32' ? 'pomoai.exe' : 'pomoai';

let bin;
try {
  bin = require.resolve(`${pkg}/bin/${exe}`);
} catch {
  process.stderr.write(
    `pomoai: could not find the binary package ${pkg}.\n` +
      'Either this platform is not supported (darwin/linux/win32 on x64/arm64), or optional\n' +
      'dependencies were skipped (--omit=optional, --no-optional, some pnpm/Yarn PnP setups).\n' +
      'Reinstall with optional dependencies, or use the install script:\n' +
      '  curl -fsSL https://raw.githubusercontent.com/PomoAi-ai/pomoai-cli/main/install.sh | sh\n',
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
