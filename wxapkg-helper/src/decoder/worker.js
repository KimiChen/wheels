// SPDX-License-Identifier: GPL-3.0-or-later

import { DecompilationController } from './controller.js';

const originalInternalSend = typeof process._send === 'function' ? process._send.bind(process) : null;
const originalReallyExit = process.reallyExit.bind(process);
const originalNextTick = process.nextTick.bind(process);
const originalChannel = process.channel;
const originalPrivateChannel = process._channel;
let finished = false;

hidePublicProcessControls();

process.once('message', async (message) => {
  const requestId = message?.requestId;
  if (message?.type !== 'decode' || !isRequestId(requestId)) {
    finish({
      type: 'error',
      requestId: isRequestId(requestId) ? requestId : '',
      error: serializeError(new Error('反编译 worker 收到了无效请求。'))
    }, 1);
    return;
  }

  try {
    assertWorkerPermissions(message.config);
    const controller = new DecompilationController(message.config);
    const result = await controller.run();
    finish({ type: 'result', requestId, result }, 0);
  } catch (error) {
    console.error(error?.stack || error?.message || String(error));
    finish({ type: 'error', requestId, error: serializeError(error) }, 1);
  }
});

function assertWorkerPermissions(config) {
  if (!process.permission) {
    const error = new Error('反编译 worker 未启用 Node 权限模型。');
    error.code = 'WXAPKG_WORKER_PERMISSION';
    throw error;
  }

  for (const scope of ['net', 'child', 'worker']) {
    if (process.permission.has(scope)) {
      const error = new Error(`反编译 worker 不应获得 ${scope} 权限。`);
      error.code = 'WXAPKG_WORKER_PERMISSION';
      throw error;
    }
  }

  if (process.permission.has('fs.write')) {
    const error = new Error('反编译 worker 不应获得文件写入权限。');
    error.code = 'WXAPKG_WORKER_PERMISSION';
    throw error;
  }

  if (!process.permission.has('fs.read', config.outputPath)) {
    const error = new Error('反编译 worker 没有输出目录读取权限。');
    error.code = 'WXAPKG_WORKER_PERMISSION';
    throw error;
  }

  for (const packageInfo of config.packageInfos) {
    const packagePath = packageInfo.inputPath;
    if (!process.permission.has('fs.read', packagePath)) {
      const error = new Error(`反编译 worker 没有输入包读取权限：${packagePath}`);
      error.code = 'WXAPKG_WORKER_PERMISSION';
      throw error;
    }
  }

  for (const item of config.customPolyfillRoots || []) {
    if (item.root && !process.permission.has('fs.read', item.root)) {
      const error = new Error(`反编译 worker 没有自定义 polyfill 快照读取权限：${item.root}`);
      error.code = 'WXAPKG_WORKER_PERMISSION';
      throw error;
    }
  }
}

function serializeError(error) {
  return {
    name: error?.name || 'Error',
    message: error?.message || String(error),
    code: error?.code,
    stack: error?.stack
  };
}

function finish(message, exitCode) {
  if (finished) {
    return;
  }
  finished = true;

  if (!originalInternalSend) {
    process.exitCode = exitCode;
    return;
  }

  try {
    originalInternalSend(message, undefined, { swallowErrors: false }, (error) => {
      originalReallyExit(error ? 1 : exitCode);
    });
  } catch {
    originalReallyExit(1);
  }
}

function hidePublicProcessControls() {
  const hiddenProperties = [
    'abort',
    'binding',
    'chdir',
    'dlopen',
    'getBuiltinModule',
    'kill',
    'loadEnvFile',
    'send',
    '_send',
    '_debugEnd',
    '_debugProcess',
    '_fatalException',
    '_getActiveHandles',
    '_getActiveRequests',
    '_kill',
    '_linkedBinding',
    '_rawDebug',
    '_startProfilerIdleNotifier',
    '_stopProfilerIdleNotifier',
    '_tickCallback',
    'disconnect',
    '_disconnect',
    'exit',
    'reallyExit'
  ];

  for (const property of hiddenProperties) {
    try {
      Object.defineProperty(process, property, {
        value: undefined,
        configurable: false,
        enumerable: false,
        writable: false
      });
    } catch {
      // Captured functions remain available to the trusted response path.
    }
  }

  defineFixedProcessValue('connected', true);
  defineFixedProcessValue('channel', originalChannel);
  defineFixedProcessValue('_channel', originalPrivateChannel);
  defineFixedProcessValue('_handleQueue', null);
  defineFixedProcessValue('nextTick', originalNextTick);
}

function defineFixedProcessValue(property, value) {
  try {
    Object.defineProperty(process, property, {
      value,
      configurable: false,
      enumerable: false,
      writable: false
    });
  } catch {
    // Permission assertions and parent-side validation remain authoritative.
  }
}

function isRequestId(value) {
  return typeof value === 'string' && /^[a-f0-9]{64}$/.test(value);
}
