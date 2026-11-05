// Build release notes from the tagged source, including direct commits without PRs.
// CI supplies the published Releases list; local previews can use --previous-tag.
import { execFileSync } from 'node:child_process';
import { readFileSync, writeFileSync } from 'node:fs';
import { resolve } from 'node:path';
import { parseArgs } from 'node:util';
import { pathToFileURL } from 'node:url';

const releaseTag = /^v(0|[1-9]\d*)\.(0|[1-9]\d*)\.(0|[1-9]\d*)$/;

export function generateReleaseNotes({ tag, repository, releases, previousTag, cwd = process.cwd() }) {
  if (!releaseTag.test(tag ?? '')) throw new Error('标签必须是 vX.Y.Z 正式版');
  if (!/^[\w.-]+\/[\w.-]+$/.test(repository ?? '')) throw new Error('仓库必须是 owner/repo');
  const git = (...args) => execFileSync('git', ['-c', 'i18n.logOutputEncoding=utf-8', ...args], {
    cwd, encoding: 'utf8', stdio: ['ignore', 'pipe', 'pipe'], maxBuffer: 8 * 1024 * 1024,
  }).trim();
  const ref = `refs/tags/${tag}`;
  git('rev-parse', '--verify', `${ref}^{commit}`);
  const isAncestor = candidate => {
    try {
      git('merge-base', '--is-ancestor', `refs/tags/${candidate}`, ref);
      return true;
    } catch (error) {
      if (error.status === 1) return false;
      throw error; // Missing history/tags must not silently produce an incomplete changelog.
    }
  };
  const compare = (a, b) => {
    const av = a.slice(1).split('.').map(BigInt);
    const bv = b.slice(1).split('.').map(BigInt);
    for (let i = 0; i < 3; i++) if (av[i] !== bv[i]) return av[i] < bv[i] ? -1 : 1;
    return 0;
  };
  if (previousTag !== undefined) {
    if (!releaseTag.test(previousTag) || compare(previousTag, tag) >= 0 || !isAncestor(previousTag)) {
      throw new Error('上个版本必须是当前标签之前、且在其历史中的正式版标签');
    }
  } else {
    if (!Array.isArray(releases)) throw new Error('请提供 GitHub Releases 列表或 --previous-tag');
    // gh api --paginate --slurp returns an array of pages. Tags without Releases
    // deliberately do not qualify, nor do drafts, previews or newer releases on reruns.
    const candidates = [...new Set(releases.flat()
      .filter(r => !r.draft && !r.prerelease && releaseTag.test(r.tag_name ?? '') && compare(r.tag_name, tag) < 0)
      .map(r => r.tag_name))].sort((a, b) => compare(b, a));
    previousTag = candidates.find(isAncestor);
  }

  const notesPath = `docs/releases/${tag}.md`;
  const hasNotes = git('ls-tree', '--name-only', ref, '--', notesPath) === notesPath;
  let changes;
  if (hasNotes) {
    changes = git('show', `${ref}:${notesPath}`);
  } else {
    const range = previousTag ? `refs/tags/${previousTag}..${ref}` : ref;
    const subjects = git('log', '--no-merges', '--format=%s', range).split('\n')
      .map(s => s.replace(/^v?\d+\.\d+\.\d+\s*[：:]\s*/u, '').trim())
      .filter(Boolean);
    changes = [...new Set(subjects)].map(s => `- ${s}`).join('\n');
  }
  if (!changes) throw new Error(`更新说明为空：请在 ${notesPath} 写明本版本的变化`);

  const version = tag.slice(1);
  const url = `https://github.com/${repository}`;
  const history = previousTag
    ? `**Full Changelog**: [${previousTag}...${tag}](${url}/compare/${previousTag}...${tag})`
    : `**完整记录**: [${tag}](${url}/commits/${tag})`;
  return `## 本次更新

${changes}

## 下载

| 文件 | 用途 |
| --- | --- |
| \`Ferry_${version}_x64-setup.exe\` | Windows 桌面版安装包 |
| \`Ferry_${version}_aarch64.dmg\` | macOS 桌面版（Apple 芯片） |
| \`Ferry_${version}_x64.dmg\` | macOS 桌面版（Intel） |
| \`ferry-linux-amd64\` / \`ferry-linux-arm64\` | Linux 命令行（CI 里部署用，见 [docs/ci.md](${url}/blob/${tag}/docs/ci.md)） |
| \`SHA256SUMS.txt\` | 各文件的 sha256 |

macOS 版是临时签名的：第一次打开要右键 →「打开」，或执行 \`xattr -cr /Applications/Ferry.app\`。

${history}
`;
}

if (process.argv[1] && import.meta.url === pathToFileURL(resolve(process.argv[1])).href) {
  try {
    const { values } = parseArgs({ options: {
      tag: { type: 'string' }, repo: { type: 'string' }, releases: { type: 'string' },
      'previous-tag': { type: 'string' }, output: { type: 'string' },
    } });
    const notes = generateReleaseNotes({
      tag: values.tag ?? process.env.GITHUB_REF_NAME,
      repository: values.repo ?? process.env.GITHUB_REPOSITORY,
      releases: values.releases ? JSON.parse(readFileSync(values.releases, 'utf8')) : undefined,
      previousTag: values['previous-tag'],
    });
    if (values.output) writeFileSync(values.output, notes, 'utf8');
    else process.stdout.write(notes);
  } catch (error) {
    console.error(`生成发布说明失败：${error.message}`);
    process.exitCode = 1;
  }
}
