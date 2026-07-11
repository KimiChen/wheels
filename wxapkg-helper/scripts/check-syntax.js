import { spawnSync } from 'node:child_process';
import fs from 'node:fs/promises';
import path from 'node:path';

const roots = ['bin', 'src', 'scripts', 'test-support'];
const files = [];

for (const root of roots) {
  await collectJavaScriptFiles(path.resolve(root), files);
}

files.sort((left, right) => left.localeCompare(right));

for (const file of files) {
  const result = spawnSync(process.execPath, ['--check', file], {
    stdio: 'inherit'
  });

  if (result.status !== 0) {
    process.exitCode = result.status || 1;
    break;
  }
}

async function collectJavaScriptFiles(directory, output) {
  const entries = await fs.readdir(directory, { withFileTypes: true });

  for (const entry of entries) {
    const fullPath = path.join(directory, entry.name);

    if (entry.isDirectory()) {
      await collectJavaScriptFiles(fullPath, output);
    } else if (entry.isFile() && entry.name.endsWith('.js')) {
      output.push(fullPath);
    }
  }
}
