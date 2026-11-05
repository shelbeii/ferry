import { describe, expect, it } from 'vitest'
import { connFromSoftware, formatReply, passwordEnv, ttlText } from './db'
import { statementAt } from './editor/sqlEditor'
import type { Software } from './ferry'

const via = { config: 'C:/p/ferry.yaml', target: 'prod' }

describe('connFromSoftware', () => {
  it('maps a host-listening MySQL to 127.0.0.1', () => {
    const sw = { kind: 'mysql', name: 'MySQL', state: 'running', via: 'docker', container: '1Panel-mysql-x', listen: ['0.0.0.0:3307'] } as Software
    const c = connFromSoftware(sw, via, 'prod')!
    expect(c).toMatchObject({ kind: 'mysql', host: '127.0.0.1', port: 3307, user: 'root', via })
    expect(c.container).toBeUndefined()
  })
  it('keeps a specific listen address and IPv6 any', () => {
    expect(connFromSoftware({ kind: 'mariadb', name: 'MariaDB', state: 'running', via: 'systemd', listen: ['10.0.0.5:3306'] } as Software, via, 'p')!.host).toBe('10.0.0.5')
    expect(connFromSoftware({ kind: 'postgresql', name: 'PostgreSQL', state: 'running', via: 'systemd', listen: ['[::]:5433'] } as Software, via, 'p')).toMatchObject({ kind: 'postgres', host: '127.0.0.1', port: 5433, user: 'postgres' })
  })
  it('uses the container address when the port is not published', () => {
    const c = connFromSoftware({ kind: 'postgresql', name: 'PostgreSQL', state: 'running', via: 'docker', container: 'pg', inner: ['0.0.0.0:5432'] } as Software, via, 'p')!
    expect(c).toMatchObject({ container: 'pg', port: 5432, host: '' })
  })
  it('uses the Ferry instance and account', () => {
    const c = connFromSoftware({ kind: 'mysql', name: 'MySQL', state: 'running', via: 'systemd', instance: 'mysql', account: 'admin', listen: ['127.0.0.1:3306'] } as Software, via, 'p')!
    expect(c).toMatchObject({ software: 'mysql', user: 'admin' })
  })
  it('maps Redis without a user', () => {
    const c = connFromSoftware({ kind: 'redis', name: 'Redis', state: 'running', via: 'systemd', instance: 'redis', account: 'x', listen: ['0.0.0.0:6380'] } as Software, via, 'p')!
    expect(c).toMatchObject({ kind: 'redis', port: 6380, host: '127.0.0.1', user: '', software: 'redis' })
  })
  it('ignores other software', () => {
    expect(connFromSoftware({ kind: 'nacos', name: 'Nacos', state: 'running', via: 'systemd' } as Software, via, 'p')).toBeNull()
  })
})

describe('formatReply', () => {
  it('formats like redis-cli', () => {
    expect(formatReply({ t: 'str', v: 'OK' })).toEqual(['OK'])
    expect(formatReply({ t: 'str', v: 'a"b' })).toEqual(['"a\\"b"'])
    expect(formatReply({ t: 'int', v: '3' })).toEqual(['(integer) 3'])
    expect(formatReply({ t: 'nil' })).toEqual(['(nil)'])
    expect(formatReply({ t: 'arr', items: [] })).toEqual(['(empty array)'])
    expect(formatReply({ t: 'arr', items: [{ t: 'str', v: 'x' }, { t: 'arr', items: [{ t: 'int', v: '1' }, { t: 'nil' }] }] })).toEqual(['1) "x"', '2) 1) (integer) 1', '   2) (nil)'])
    expect(formatReply({ t: 'str', v: '# Server\r\nredis_version:7\r\n' })).toEqual(['# Server', 'redis_version:7'])
  })
  it('says how long until a key expires', () => {
    expect(ttlText(-1)).toBe('永不过期')
    expect(ttlText(30_000)).toBe('30 秒后过期')
    expect(ttlText(3_720_000)).toBe('1 小时 2 分后过期')
    expect(ttlText(90_000_000)).toBe('1 天 1 小时后过期')
  })
})

describe('statementAt', () => {
  const text = 'select 1;\nselect 2;\n\nselect 3'
  it('finds the statement under the cursor', () => {
    expect(statementAt(text, 3)).toBe('select 1;')
    expect(statementAt(text, text.indexOf('2'))).toBe('select 2;')
    expect(statementAt(text, text.length)).toBe('select 3')
  })
  it('right after a semicolon picks the previous statement', () => {
    expect(statementAt(text, text.indexOf(';') + 1)).toBe('select 1;')
  })
})

it('passwordEnv matches dbx.PasswordEnv', () => {
  expect(passwordEnv('a1b2-c3')).toBe('FERRY_DB_PASSWORD_A1B2_C3')
})
