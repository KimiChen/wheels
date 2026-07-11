import { confirm, input, select } from '@inquirer/prompts';
import { formatCandidateLabel, formatIdentityLabel, formatIdentitySources, shortenPath } from './report.js';

export async function chooseCandidate(candidates) {
  return select({
    message: '选择要反解的 wxapkg 缓存目录',
    pageSize: 12,
    choices: candidates.map((candidate, index) => ({
      name: formatCandidateLabel(candidate, index),
      value: candidate
    }))
  });
}

export async function chooseDecodeMode() {
  const mode = await select({
    message: '选择反解模式',
    choices: [
      {
        name: '完整反解',
        value: 'decode',
        description: '解包并尽量还原源代码'
      },
      {
        name: '只解包',
        value: 'unpack-only',
        description: '只展开 wxapkg 内容，不继续反编译'
      }
    ]
  });

  return mode === 'unpack-only';
}

export async function chooseExistingOutputAction(outDir) {
  return select({
    message: `输出目录已存在：${shortenPath(outDir)}`,
    choices: [
      {
        name: '换一个输出目录',
        value: 'change',
        description: '输入新的输出路径'
      },
      {
        name: '继续写入当前目录',
        value: 'continue',
        description: '不清空旧产物，可能覆盖同名文件'
      },
      {
        name: '取消',
        value: 'cancel',
        description: '停止本次反解'
      }
    ]
  });
}

export async function inputOutputDir(defaultValue) {
  return input({
    message: '新的输出目录',
    default: defaultValue,
    required: true
  });
}

export async function confirmClearOutput(outDir) {
  return confirm({
    message: `确认清空输出目录后再反解？${shortenPath(outDir)}`,
    default: false
  });
}

export async function confirmDecode({ target, outDir, clear, unpackOnly, candidate }) {
  console.log('\n即将执行：');
  console.log('  授权用途：仅用于你拥有权利或已获授权的代码审计、恢复和学习场景。');
  if (candidate?.identity) {
    console.log(`  识别：${formatIdentityLabel(candidate)}`);
    const sources = formatIdentitySources(candidate.identity);
    if (sources) {
      console.log(`  线索：${sources}`);
    }
  }
  console.log(`  输入：${shortenPath(target)}`);
  console.log(`  输出：${shortenPath(outDir)}`);
  console.log(`  清空旧产物：${clear ? '是' : '否'}`);
  console.log(`  只解包：${unpackOnly ? '是' : '否'}`);
  if (clear) {
    console.log('  风险提示：--clear 会在反编译前清空输出目录。');
  }

  return confirm({
    message: clear ? '已确认授权用途和清空风险，开始反解？' : '已确认授权用途，开始反解？',
    default: true
  });
}
