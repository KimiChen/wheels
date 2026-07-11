#!/usr/bin/env node

import { run } from '../src/cli.js';

run().catch((error) => {
  if (error?.name === 'ExitPromptError') {
    console.error('\n已取消。');
    process.exitCode = 130;
    return;
  }

  console.error(error?.message || error);
  process.exitCode = 1;
});
