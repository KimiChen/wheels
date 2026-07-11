import { shortenPath } from './report.js';

export function printDecodeErrorHelp(error, context = {}) {
  console.error('\n反解失败排查建议：');
  console.error(`  输入：${shortenPath(context.target || '')}`);
  console.error(`  输出：${shortenPath(context.outDir || '')}`);

  const message = String(error?.message || '');
  const suggestions = getSuggestions(message, context, error?.code);

  for (const suggestion of suggestions) {
    console.error(`  - ${suggestion}`);
  }
}

function getSuggestions(message, context, errorCode) {
  const lowerMessage = message.toLowerCase();
  const code = String(errorCode || '');
  const suggestions = [];

  if (lowerMessage.includes('enoent') || lowerMessage.includes('not found')) {
    suggestions.push('确认输入路径存在，并且目录下至少有一个 .wxapkg 文件。');
  }

  if (lowerMessage.includes('wxid') || lowerMessage.includes('appid') || lowerMessage.includes('加密') || lowerMessage.includes('decrypt')) {
    suggestions.push('如果是加密包，确认路径中包含正确的游戏标识 ID/appid，或用 --wxid 显式指定。');
  }

  if (lowerMessage.includes('no such file') || lowerMessage.includes('主包') || lowerMessage.includes('__app__')) {
    suggestions.push('确认主包（如 __APP__.wxapkg、app.wxapkg 或 __WITHOUT_MULTI_PLUGINCODE__.wxapkg）和相关分包放在同一目录内。');
  }

  if (lowerMessage.includes('permission') || lowerMessage.includes('eacces') || lowerMessage.includes('eperm')) {
    suggestions.push('确认输出目录有写入权限，必要时换一个 --out 目录。');
  }

  if (
    code === 'WXAPKG_UNSAFE_OUTPUT'
    || code === 'WXAPKG_UNSAFE_POLYFILL'
    || code === 'WXAPKG_UNSAFE_PERMISSION_PATH'
    || code === 'WXAPKG_CORE_FS_DENIED'
    || code.startsWith('WXAPKG_MANIFEST_')
    || code.includes('WXAPKG_PATH')
    || code.includes('WXAPKG_OUTPUT_ESCAPE')
    || code.includes('WXAPKG_OUTPUT_SYMLINK')
  ) {
    suggestions.push('包内路径或输出目录未通过安全检查，请不要绕过该保护。');
  }

  const isSecurityError = code.includes('WXAPKG_PATH')
    || code.includes('WXAPKG_OUTPUT_')
    || code.startsWith('WXAPKG_MANIFEST_')
    || code === 'WXAPKG_UNSAFE_OUTPUT'
    || code === 'WXAPKG_UNSAFE_POLYFILL'
    || code === 'WXAPKG_UNSAFE_PERMISSION_PATH'
    || code === 'WXAPKG_CORE_FS_DENIED';
  const isIoError = code.includes('INPUT_READ') || code.includes('OUTPUT_CREATE') || code.includes('OUTPUT_WRITE');

  if (code.startsWith('ERR_WXAPKG_') && !isSecurityError && !isIoError && !code.includes('WXID') && !code.includes('DECRYPT')) {
    suggestions.push('输入包已损坏、尚未完整缓存，或不是受支持的 wxapkg 格式。');
  }

  if (code.includes('WXID') || code.includes('DECRYPT')) {
    suggestions.push('如果是 V1MMWX 加密包，请确认 --wxid 与包所属小程序一致。');
  }

  if (code === 'WXAPKG_DECODE_TIMEOUT') {
    suggestions.push('包内代码执行超时，请先用 --unpack-only 确认包结构。');
  }

  if (code === 'WXAPKG_UNSAFE_POLYFILL') {
    suggestions.push('移除自定义 polyfill 目录中的符号链接或特殊文件，再重新执行。');
  }

  if (code === 'WXAPKG_INPUT_CHANGED') {
    suggestions.push('输入包在检查和解包之间发生了变化；等待微信缓存写入结束后重试。');
  }

  if (code === 'WXAPKG_WORKER_PROTOCOL') {
    suggestions.push('worker 完成消息未通过协议校验，请保留原始错误并报告可复现包特征。');
  }

  if (code === 'WXAPKG_UNSUPPORTED_NODE') {
    suggestions.push('升级到 Node.js 25 或更高版本；旧版本无法为包内代码提供完整的网络权限隔离。');
  }

  if (context.clear) {
    suggestions.push('本次使用了 --clear；如果输出目录不对，先换 --out 再重试。');
  }

  suggestions.push('可以先运行 decode <target> --list-packages 预览包清单。');
  suggestions.push('可以运行 decode <target> --dry-run --yes 检查内置反编译执行计划。');

  return Array.from(new Set(suggestions));
}
