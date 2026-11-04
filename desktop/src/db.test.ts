import { describe, expect, it } from 'vitest'
import { connFromSoftware, dangerOf, passwordEnv } from './db'
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
  it('ignores other software', () => {
    expect(connFromSoftware({ kind: 'redis', name: 'Redis', state: 'running', via: 'systemd' } as Software, via, 'p')).toBeNull()
  })
})

describe('dangerOf', () => {
  it('flags destructive statements', () => {
    expect(dangerOf('DROP TABLE t')).not.toBe('')
    expect(dangerOf('truncate t')).not.toBe('')
    expect(dangerOf('DELETE FROM t')).toContain('删掉整张表')
    expect(dangerOf("UPDATE t SET a = 'where'")).toContain('改整张表')
    expect(dangerOf('ALTER TABLE t DROP COLUMN a')).not.toBe('')
  })
  it('lets normal statements through', () => {
    expect(dangerOf('DELETE FROM t WHERE id = 1')).toBe('')
    expect(dangerOf('-- drop table\nSELECT 1')).toBe('')
    expect(dangerOf('INSERT INTO t VALUES (1)')).toBe('')
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
