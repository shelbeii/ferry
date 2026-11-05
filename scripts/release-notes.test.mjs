import assert from 'node:assert/strict';
import { execFileSync } from 'node:child_process';
import { mkdtempSync, mkdirSync, rmSync, writeFileSync } from 'node:fs';
import { tmpdir } from 'node:os';
import { basename, dirname, join, resolve } from 'node:path';
import { test } from 'node:test';
import { generateReleaseNotes } from './release-notes.mjs';

function fixture(t) {
  const cwd = mkdtempSync(join(tmpdir(), 'ferry-release-notes-'));
  t.after(() => {
    assert.equal(dirname(resolve(cwd)), resolve(tmpdir()));
    assert.ok(basename(cwd).startsWith('ferry-release-notes-'));
    rmSync(cwd, { recursive: true, force: true });
  });
  const git = (...args) => execFileSync('git', args, { cwd, encoding: 'utf8', stdio: ['ignore', 'pipe', 'pipe'] });
  git('init', '--initial-branch=main');
  git('config', 'user.name', 'Release Notes Test');
  git('config', 'user.email', 'release-test@example.invalid');
  const commit = subject => git('commit', '--allow-empty', '-m', subject);
  commit('已有功能');
  git('tag', 'v1.0.0');
  commit('1.0.1：新增内置浏览器 🚢');
  git('tag', 'v1.0.1'); // Intentionally not a published Release.
  commit('修复窗口卡死');
  return { cwd, git, commit };
}

test('direct commits since the previous published release appear before downloads', t => {
  const { cwd, git } = fixture(t);
  git('tag', 'v1.0.2');
  git('config', 'i18n.logOutputEncoding', 'GB18030');
  const notes = generateReleaseNotes({ cwd, tag: 'v1.0.2', repository: 'example/ferry', releases: [[
    { tag_name: 'v1.0.2' }, // Rerun: the current release must not be its own baseline.
    { tag_name: 'v1.0.1', draft: true },
    { tag_name: 'v1.0.1', prerelease: true },
    { tag_name: 'v1.0.0' },
  ]] });
  assert.match(notes, /- 新增内置浏览器 🚢/);
  assert.match(notes, /- 修复窗口卡死/);
  assert.doesNotMatch(notes, /已有功能|1\.0\.1：/);
  assert.ok(notes.indexOf('修复窗口卡死') < notes.indexOf('## 下载'));
  assert.match(notes, /compare\/v1\.0\.0\.\.\.v1\.0\.2/);
  assert.match(notes, /Ferry_1\.0\.2_x64-setup\.exe/);
});

test('old release reruns exclude later releases and commits', t => {
  const { cwd, git, commit } = fixture(t);
  git('tag', 'v1.0.2');
  commit('新版独有功能');
  git('tag', 'v1.0.3');
  const notes = generateReleaseNotes({ cwd, tag: 'v1.0.2', repository: 'example/ferry', releases: [
    { tag_name: 'v1.0.3' }, { tag_name: 'v1.0.2' }, { tag_name: 'v1.0.0' },
  ] });
  assert.doesNotMatch(notes, /新版独有功能/);
  assert.match(notes, /compare\/v1\.0\.0\.\.\.v1\.0\.2/);
});

test('version-specific descriptions come from the tag, not the working tree', t => {
  const { cwd, git, commit } = fixture(t);
  const directory = join(cwd, 'docs', 'releases');
  mkdirSync(directory, { recursive: true });
  const file = join(directory, 'v1.0.2.md');
  writeFileSync(file, '- 新增浏览器，可在部署时直接检查页面。\n');
  git('add', 'docs');
  commit('整理发布说明');
  git('tag', 'v1.0.2');
  writeFileSync(file, '- 尚未发布的内容\n');
  const notes = generateReleaseNotes({ cwd, tag: 'v1.0.2', repository: 'example/ferry', previousTag: 'v1.0.0' });
  assert.match(notes, /新增浏览器，可在部署时直接检查页面/);
  assert.doesNotMatch(notes, /尚未发布|整理发布说明/);
});

test('first release includes commit descriptions without inventing a previous tag', t => {
  const { cwd } = fixture(t);
  const notes = generateReleaseNotes({ cwd, tag: 'v1.0.1', repository: 'example/ferry', releases: [] });
  assert.match(notes, /新增内置浏览器/);
  assert.match(notes, /commits\/v1\.0\.1/);
  assert.doesNotMatch(notes, /\/compare\//);
});

test('missing history and empty explicit descriptions fail instead of hiding updates', t => {
  const { cwd, git, commit } = fixture(t);
  mkdirSync(join(cwd, 'docs', 'releases'), { recursive: true });
  writeFileSync(join(cwd, 'docs', 'releases', 'v1.0.2.md'), ' \n');
  git('add', 'docs');
  commit('发布说明');
  git('tag', 'v1.0.2');
  const options = { cwd, tag: 'v1.0.2', repository: 'example/ferry' };
  assert.throws(() => generateReleaseNotes({ ...options, previousTag: 'v1.0.0' }), /更新说明为空/);
  assert.throws(() => generateReleaseNotes({ ...options, releases: [{ tag_name: 'v1.0.0' }, { tag_name: 'v1.0.1' }, { tag_name: 'v1.0.0' }] }), /更新说明为空/);
  assert.throws(() => generateReleaseNotes({ ...options, previousTag: 'v0.9.0' }));
  assert.throws(() => generateReleaseNotes(options), /Releases 列表/);
});
