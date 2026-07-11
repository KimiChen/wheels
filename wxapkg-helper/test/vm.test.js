import assert from 'node:assert/strict';
import { describe, it } from 'node:test';
import { createVM, runVmCode } from '../src/decoder/core/utils/create-vm.js';

describe('decoder VM execution', () => {
  it('propagates syntax errors instead of reporting a false success', () => {
    const decoderVm = createVM();

    assert.throws(
      () => runVmCode(decoderVm, 'function broken('),
      (error) => error?.name === 'SyntaxError' && /Unexpected end of input/.test(error.message)
    );
  });

  it('enforces the per-execution timeout', () => {
    const decoderVm = createVM({ timeout: 20 });

    assert.throws(
      () => runVmCode(decoderVm, 'while (true) {}'),
      (error) => error?.code === 'ERR_SCRIPT_EXECUTION_TIMEOUT'
    );
  });
});
