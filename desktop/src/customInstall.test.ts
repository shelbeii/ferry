import { describe, expect, it } from 'vitest'
import {
  buildCustomYAML,
  createCustomDraft,
  deriveTitleFromFile,
  detectPackageFormat,
  isArchiveFormat,
  nextFreeTcpPort,
  validateCustomDraft,
} from './customInstall'

describe('customInstall helpers', () => {
  it('recognizes all archive and system package formats', () => {
    expect(detectPackageFormat('app.tar.gz')).toBe('tar.gz')
    expect(detectPackageFormat('app.TGZ')).toBe('tar.gz')
    expect(detectPackageFormat('app.tar.xz')).toBe('tar.xz')
    expect(detectPackageFormat('app.tar.bz2')).toBe('tar.bz2')
    expect(detectPackageFormat('app.tar.zst')).toBe('tar.zst')
    expect(detectPackageFormat('app.tar')).toBe('tar')
    expect(detectPackageFormat('app.zip')).toBe('zip')
    expect(detectPackageFormat('app.jar')).toBe('jar')
    expect(detectPackageFormat('agent-1.0.x86_64.rpm')).toBe('rpm')
    expect(detectPackageFormat('agent_1.0_amd64.deb')).toBe('deb')
    expect(detectPackageFormat('minio')).toBe('binary')
    expect(isArchiveFormat('tar.zst')).toBe(true)
    expect(isArchiveFormat('rpm')).toBe(false)
  })

  it('picks next free tcp port and derives clean title', () => {
    expect(nextFreeTcpPort(8080, new Set([8080, 8081]))).toBe(8082)
    expect(deriveTitleFromFile('caddy_2.8.4_linux_amd64.tar.zst')).toBe('caddy')
    expect(deriveTitleFromFile('my-worker.rpm')).toBe('my-worker')
  })

  it('validates archive architecture mismatch and allows portless process mode', () => {
    const draft = createCustomDraft('worker')
    draft.pkg = 'worker-arm64.tar.gz'
    draft.format = 'tar.gz'
    draft.strip = 0
    draft.exec = '${dir}/worker'
    draft.readyMode = 'process'
    draft.port = 8080

    const errs = validateCustomDraft(draft, {
      takenNames: new Set(),
      serverArch: 'amd64',
      archivePreview: { files: ['worker'], entries: 1, strip: 0, recommendedStrip: 0, arches: ['arm64'] },
      portTakenText: () => '已被占用',
    })
    expect(errs.pkg).toContain('不匹配')
    // process 模式不检查端口占用
    expect(errs.port).toBeUndefined()

    const okErrs = validateCustomDraft(draft, {
      takenNames: new Set(),
      serverArch: 'arm64',
      archivePreview: { files: ['worker'], entries: 1, strip: 0, recommendedStrip: 0, arches: ['arm64'] },
    })
    expect(Object.keys(okErrs)).toHaveLength(0)
  })

  it('builds YAML with env, inline config, process mode, keepOnFail, forking, and allowRoot', () => {
    const draft = createCustomDraft('agent')
    draft.pkg = 'agent.tar.gz'
    draft.format = 'tar.gz'
    draft.strip = 0
    draft.title = 'My Agent'
    draft.exec = '${dir}/bin/agent --config ${dir}/conf/app.yaml'
    draft.readyMode = 'process'
    draft.serviceType = 'forking'
    draft.allowRoot = true
    draft.keepOnFail = true
    draft.readyTimeout = 90
    draft.envRows = [{ key: 'APP_ENV', value: 'prod' }]
    draft.configPath = 'conf/app.yaml'
    draft.configContent = 'mode: prod\n'
    draft.logPath = 'logs/agent.log'

    const yaml = buildCustomYAML(draft)
    expect(yaml).toContain('mode: process')
    expect(yaml).toContain('type: forking')
    expect(yaml).toContain('allowRoot: true')
    expect(yaml).toContain('keepOnFail: true')
    expect(yaml).toContain('APP_ENV: prod')
    expect(yaml).toContain('path: ${dir}/conf/app.yaml')
    expect(yaml).toContain('${dir}/logs/agent.log')
    expect(yaml).not.toContain('params:')
  })
})
