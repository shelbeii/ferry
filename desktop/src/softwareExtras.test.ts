import { describe, expect, it } from 'vitest'
import type { SwExtra } from './ferry'
import { extraLine, extrasCount, extrasPayload, extraWhere, keyError, normKey, rowErrors, valueError } from './softwareExtras'

const mysql: SwExtra = { id: 'mysqld', label: 'MySQL 配置', format: 'ini', file: '${dir}/my.cnf', section: 'mysqld', locked: ['port', 'bind-address', 'daemonize'] }
const zlm: SwExtra = { id: 'ini', label: 'ZLM', format: 'ini', file: '${dir}/config.ini', locked: ['http.port'] }
const redis: SwExtra = { id: 'conf', label: 'Redis', format: 'keyvalue', file: '${dir}/redis.conf', locked: ['port'] }
const minio: SwExtra = { id: 'env', label: 'MinIO', format: 'env', locked: ['MINIO_ROOT_PASSWORD'] }
const pg: SwExtra = { id: 'conf', label: 'PG', format: 'args', prefix: '-c ', locked: ['listen_addresses'] }

describe('附加配置', () => {
  it('比较名称的写法和 Go 一致', () => {
    expect(normKey(mysql, 'Bind_Address')).toBe(normKey(mysql, 'bind-address'))
    expect(normKey(zlm, 'HTTP.port')).toBe('http.port')
    expect(normKey(redis, 'MaxMemory')).toBe('maxmemory')
    expect(normKey(minio, 'MINIO_X')).not.toBe(normKey(minio, 'minio_x'))
  })

  it('锁住的名称、写错的名称', () => {
    expect(keyError(mysql, 'bind_address')).toContain('由 Ferry 管理')
    expect(keyError(mysql, 'innodb_buffer_pool_size')).toBe('')
    expect(keyError(mysql, 'a b')).toContain('只能用')
    expect(keyError(zlm, 'enable')).toContain('段.名称')
    expect(keyError(zlm, 'Http.Port')).toContain('由 Ferry 管理')
    expect(keyError(zlm, 'hook.enable')).toBe('')
    expect(keyError(minio, '1X')).toContain('不能以数字开头')
    expect(keyError(pg, 'LISTEN_ADDRESSES')).toContain('由 Ferry 管理')
    expect(keyError(redis, ' ')).toBe('写名称')
  })

  it('值的检查', () => {
    expect(valueError(redis, '  ')).toBe('要写值')
    expect(valueError(mysql, '')).toBe('')
    expect(valueError(mysql, 'a\nb')).toBe('不能换行')
    expect(valueError(mysql, 'x'.repeat(4097))).toContain('太长')
  })

  it('每一行：空行不算，重复的指出是哪一行', () => {
    const errs = rowErrors(mysql, [
      { key: 'max_connections', value: '900' },
      { key: '', value: '' },
      { key: 'max-connections', value: '1' },
      { key: 'port', value: '1' },
    ])
    expect(errs[0]).toEqual({ key: '', value: '' })
    expect(errs[1]).toEqual({ key: '', value: '' })
    expect(errs[2].key).toBe('和第 1 行是同一项')
    expect(errs[3].key).toContain('由 Ferry 管理')
  })

  it('交给 ferry 的 JSON：去掉空行和首尾空白', () => {
    const rows = { mysqld: [{ key: ' innodb_buffer_pool_size ', value: ' 1G ' }, { key: '', value: '' }, { key: 'skip-name-resolve', value: '' }], other: [{ key: 'x', value: '1' }] }
    expect(extrasPayload([mysql], rows)).toBe(JSON.stringify({ mysqld: { innodb_buffer_pool_size: '1G', 'skip-name-resolve': '' } }))
    expect(extrasPayload([mysql], { mysqld: [{ key: '', value: '' }] })).toBe('')
    expect(extrasCount(rows)).toBe(3)
  })

  it('给人看的写法', () => {
    expect(extraWhere(mysql)).toBe('my.cnf 的 [mysqld]')
    expect(extraWhere(zlm)).toContain('段.名称')
    expect(extraWhere(pg)).toBe('启动参数 -c 名称=值')
    expect(extraWhere(minio)).toBe('服务的环境变量')
    expect(extraLine('keyvalue', { key: 'maxmemory', value: '2gb' })).toBe('maxmemory 2gb')
    expect(extraLine('ini', { section: 'mysqld', key: 'skip-name-resolve', value: '' })).toBe('[mysqld] skip-name-resolve')
    expect(extraLine('properties', { key: 'db.num', value: '1' })).toBe('db.num=1')
  })
})
