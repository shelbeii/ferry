// ferry.yaml 的结构说明：编辑器的补全、悬停提示和表单编辑都用它。与 docs/config.md、internal/config 保持一致
// （字段增减由 schema.test.ts 对照 internal/config/testdata/fields.txt 检查）。

/** 表单里的输入方式。 */
export type Kind =
  | 'str' // 一行文字
  | 'num' // 数字
  | 'bool' // 是 / 否
  | 'dur' // 时长：90s、2m
  | 'list' // 一组文字（或数字，见 numbers）
  | 'map' // 名字 → 文字（vars、env）
  | 'obj' // 固定字段的对象
  | 'objMap' // 名字 → 对象（targets、builds）
  | 'objList' // 对象列表（apps、backups）

export interface SchemaNode {
  /** 表单里显示的名字 */
  label?: string
  doc: string
  kind?: Kind
  /** 固定字段 */
  fields?: Record<string, SchemaNode>
  /** 任意键名的映射（targets.<名字>、builds.<名字>、vars、env） */
  map?: SchemaNode
  /** 对象列表的元素（apps 下的每一项） */
  item?: SchemaNode
  /** 可选值 */
  values?: string[]
  /** 除了 values 还可以写别的（如 schedule 的 OnCalendar） */
  open?: boolean
  /** 值的类型说明（显示在补全项右侧） */
  type?: string
  /** 不写时用的值（只用来显示） */
  default?: string
  /** 必填；写应用类型时表示只对这些类型必填 */
  required?: boolean | string[]
  /** 只用于这些应用类型（表单里其他类型的应用不显示） */
  appTypes?: string[]
  /** 只在同一对象里写了这个字段时有用 */
  needs?: string
  /** 同一对象里写了这个字段时就不用写它 */
  unless?: string
  /** 对象里这几组字段，每组只能写一个（第一组为 true 时表示必须写一个） */
  oneOf?: string[][]
  oneOfRequired?: boolean
  /** 对象也可以只写一个值：等于只写了这个字段（hooks 的 run、files 的 from） */
  short?: string
  /** list：元素是数字 */
  numbers?: boolean
  /** list：也可以只写一个值（不写成列表） */
  single?: boolean
}

type Opts = Partial<Omit<SchemaNode, 'label' | 'doc'>>

const str = (label: string, doc: string, o?: Opts): SchemaNode => ({ label, doc, kind: 'str', type: '字符串', ...o })
const pick = (label: string, doc: string, values: string[], o?: Opts): SchemaNode => ({ label, doc, kind: 'str', type: '字符串', values, ...o })
const dur = (label: string, doc: string, o?: Opts): SchemaNode => ({ label, doc, kind: 'dur', type: '时长', ...o })
const bool = (label: string, doc: string, o?: Opts): SchemaNode => ({ label, doc, kind: 'bool', type: '布尔', values: ['true', 'false'], ...o })
const num = (label: string, doc: string, o?: Opts): SchemaNode => ({ label, doc, kind: 'num', type: '数字', ...o })
const list = (label: string, doc: string, o?: Opts): SchemaNode => ({ label, doc, kind: 'list', type: '列表', ...o })
const strMap = (label: string, doc: string, o?: Opts): SchemaNode => ({
  label,
  doc,
  kind: 'map',
  type: '映射',
  map: { doc: '值（可以用 ${变量}）', kind: 'str', type: '字符串' },
  ...o,
})
const obj = (label: string, doc: string, fields: Record<string, SchemaNode>, o?: Opts): SchemaNode => ({
  label,
  doc,
  kind: 'obj',
  type: '对象',
  fields,
  ...o,
})

const health = (forStatic = false): SchemaNode =>
  obj(
    '健康检查',
    forStatic ? '切换后检查网站能不能访问（可选）' : '启动后怎样算成功：健康检查通过（并稳定 stable）才算启动成功；进程退出立即判失败',
    {
      tcp: str('TCP 端口', '端口或 主机:端口，能连上就算健康，如 "${port}"'),
      http: str('HTTP 地址', 'http:// 或 https:// 开头，返回 2xx/3xx（或 status 指定的状态码）就算健康'),
      status: list('HTTP 状态码', '认为健康的状态码，如 [200]', { numbers: true, default: '2xx、3xx', needs: 'http' }),
      contains: str('响应里要有的文字', 'HTTP 响应里必须包含的文字', { needs: 'http' }),
      timeout: dur('最长等待', '等多久还不健康就算启动失败，如 90s、2m', { default: forStatic ? '30s' : '180s' }),
      stable: dur('稳定运行多久', '通过后还要稳定运行多久', { default: '没有 tcp、http 时 10s，否则 0' }),
    },
  )

function hookItem(): SchemaNode {
  return obj(
    '命令',
    '一条命令，或 {run, user, timeout, optional}',
    {
      run: str('命令', '用 sh -c 执行，如 java -jar flyway.jar migrate', { required: true }),
      user: str('执行用户', '以谁的身份执行；可以写 root', { default: '应用的运行用户' }),
      timeout: dur('最长执行多久', '最多 2h', { default: '5m' }),
      optional: bool('失败也接着部署', '失败只提示、接着部署', { default: 'false（失败就中止）' }),
    },
    { type: '字符串或对象', short: 'run' },
  )
}

const buildFields = (): Record<string, SchemaNode> => ({
  command: str('构建命令', '${each:变量} 展开成引用它的各应用的值（逗号连接）', { required: true }),
  workdir: str('工作目录', '相对配置文件所在目录', { default: '配置文件所在目录' }),
  shell: pick('用哪个 shell', '执行构建命令的 shell', ['cmd', 'powershell', 'pwsh', 'bash', 'sh'], { open: true, default: 'Windows 上 cmd，其他 sh' }),
  env: strMap('环境变量', '构建时额外的环境变量'),
  lock: str('构建锁名', '同名的构建（不同窗口、命令行同时部署时）一个一个执行', { default: '按工作目录' }),
})

const build = obj('构建', '一条构建命令。多个应用引用同一条构建时，一次部署只执行一次', buildFields())

// service 里只用于 java / binary / python / node / dotnet 的字段（static 的应用只能写 unit、user、description）
const svc = ['java', 'binary', 'python', 'node', 'dotnet']

const service = obj('服务', 'systemd 服务：怎样启动程序', {
  unit: str('服务名', 'systemd 单元名（自动补 .service）；已存在且不是 Ferry 生成的会被接管', { default: '<项目名>-<应用名小写>' }),
  description: str('服务描述', '单元文件里的 Description', { default: '<项目名> <应用名>' }),
  user: str('运行用户', '不能是 root', { default: '服务器的 runAs' }),
  workdir: str('工作目录', '程序启动时所在的目录；按相对路径写的日志也就和程序在一起', { default: '${running}（运行中的版本目录）', appTypes: svc }),
  runtime: obj(
    'Ferry 提供的 JDK',
    '部署时自动下载 JDK（本机已有就不下载）、上传到服务器的 ferryDir/runtimes，服务用它的 java（启动命令里是 ${java}）',
    {
      java: pick('JDK 版本', 'JDK 大版本', ['8', '11', '17', '21', '25'], { kind: 'num', type: '数字', required: true }),
      version: str(
        '锁定完整版本',
        '如 25.0.4.1+1（java -version 里 build 后面那一段）：只用这个版本，换版本要改这里再部署，回滚时 JDK 也回到当时的版本。不写时用服务器上这个大版本当前装的 JDK，同一台服务器上不锁版本的项目共用它',
        { default: '不锁定' },
      ),
    },
    { appTypes: ['java'] },
  ),
  java: str('java 路径', 'java 可执行文件的绝对路径', { default: '/usr/bin/java', appTypes: ['java'] }),
  jvmArgs: list('JVM 参数', '启动命令是 java <JVM 参数> -jar ${artifact} <程序参数>', { appTypes: ['java'] }),
  args: list('程序参数', '加在启动命令最后', { appTypes: ['java', 'binary'] }),
  exec: list('启动命令', '完整命令行，第一项是程序（绝对路径或 node、dotnet 这类命令），后面每项一个参数；写了它 java、JVM 参数、程序参数都不用', {
    appTypes: svc,
    required: ['python', 'node', 'dotnet'],
  }),
  env: strMap('环境变量', '写进单元文件的 Environment=', { appTypes: svc }),
  envFile: list('环境变量文件', '服务器上的文件（EnvironmentFile=），- 开头表示文件不存在也不报错。密码等放这里', { appTypes: svc, single: true }),
  execStartPre: list('启动前执行', '原样写入 ExecStartPre=', { appTypes: svc }),
  conflicts: list('要替换的旧服务', '同端口的其他单元：部署前停止，成功后取消它的开机自启，失败回滚时恢复', { appTypes: svc }),
  sharedDirs: list('数据目录', 'data 下要创建的目录（归运行用户，不随版本变化）；不写时不建 data', { appTypes: svc }),
  diagLogs: list('启动失败时看的日志', '启动失败时显示这些日志文件的末尾，如 ${logs}/error.log（相对路径为相对 data）', { appTypes: svc }),
  archiveLogs: list('随版本归档的日志目录', 'data 下的日志目录，切换版本时随离开的版本归档；写 [] 表示不归档', { appTypes: svc, default: 'sharedDirs 里的 logs（有的话）' }),
  health: { ...health(), appTypes: svc },
  zeroDowntime: obj(
    '不停机发布',
    '两个端口轮流用，新版本在空着的端口启动、就绪后再停旧的（失败时旧版本一直没停过）。${port} 是这次用的端口，程序还会拿到环境变量 SERVER_PORT、PORT；切换时两个版本同时运行，内存要够两份',
    {
      ports: list('两个端口', '轮流用，如 [9201, 19201]', { numbers: true, required: true }),
      listen: str('对外端口', '直接对外的应用：Ferry 的 nginx 监听这个端口（8080 或 0.0.0.0:8080），转发到运行中的版本'),
      drain: dur('停旧版本前等多久', '新版本接上后等多久再停旧版本（还在处理的请求做完、注册中心摘掉旧版本），最多 10m', { default: '30s' }),
      nginx: str('nginx 版本', 'listen 用的 nginx 版本系列，如 1.30', { needs: 'listen', default: '服务器上 Ferry 已装的' }),
    },
    { appTypes: svc },
  ),
  unitExtra: obj(
    '单元文件追加内容',
    '追加到单元文件各段末尾的原样行（后出现的单值设置会覆盖前面的）',
    { unit: list('[Unit] 段', '原样行'), service: list('[Service] 段', '原样行'), install: list('[Install] 段', '原样行') },
    { appTypes: svc },
  ),
}, { oneOf: [['runtime', 'java']] })

const app: SchemaNode = obj('应用', '一个可部署的应用', {
  name: str('应用名', '不区分大小写，不能重复', { required: true }),
  type: pick(
    '类型',
    'java（jar）、binary（原生程序）、static（前端）、python、node（Node 服务端）、dotnet（.NET）；后三种上传整个目录，用 service.exec 启动',
    ['java', 'binary', 'static', 'python', 'node', 'dotnet'],
    { required: true },
  ),
  order: num('顺序', '部署与启动顺序（小的先），停止按反过来的顺序', { default: '0，再按配置里的先后' }),
  build: {
    ...obj('构建', 'builds 里的名字，或直接写一条构建', buildFields()),
    type: '字符串或对象',
    short: '@build',
    default: '不构建',
  },
  artifact: obj('构建产物', '要上传的文件或目录', {
    path: str('产物路径', 'java / binary 是文件（文件名就是服务器上的文件名），static、python、node、dotnet 是目录；相对配置文件所在目录', { required: true }),
    exclude: list('不上传的', '目录里不上传的：文件名或相对路径，可以用 * ?', {
      appTypes: ['static', 'python', 'node', 'dotnet'],
      default: 'python 不传 .venv、__pycache__……，node 不传 node_modules……',
    }),
    verify: obj('上传前检查', '上传前对产物的检查', {
      springBoot: obj('Spring Boot 检查', '检查是 Spring Boot 可执行 JAR；写 {} 表示只做这一项', {
        requireLibs: list('必须有的依赖', 'BOOT-INF/lib 下必须存在的依赖（通配符），如 my-common-core-*.jar'),
        nonEmptyLibs: bool('依赖不能是空包', '上面匹配到的依赖里必须有 .class'),
      }, { appTypes: ['java'] }),
      static: obj('静态目录检查', '前端目录里必须有某个文件', { index: str('必须有的文件', '相对产物目录', { default: 'index.html' }) }, { appTypes: ['static'] }),
      minSize: num('最小字节数', '产物比这个小就不上传'),
    }),
    files: {
      ...list('附带文件', '配置文件、证书目录等：和程序放在同一级目录（程序的工作目录），每次部署都上传，回滚时一起回滚；程序只能读'),
      kind: 'objList',
      appTypes: ['java', 'binary'],
      item: obj('附带文件', '本机路径（放过去同名），或 {from, to, exec}', {
        from: str('本机路径', '文件或目录，相对配置文件所在目录，可以用 ${变量}，如 deploy/${target}/application.yml', { required: true }),
        to: str('放到服务器上叫什么', '程序所在目录下的相对路径，如 config/application.yml', { default: '本机的文件名' }),
        exec: bool('可执行', '脚本要勾上'),
      }, { type: '字符串或对象', short: 'from' }),
    },
  }, { required: true }),
  deps: obj('装依赖', '部署时在服务器上装依赖（requirements.txt / package.json），依赖文件没变就用上次装好的', {
    registry: str('镜像源', 'pip 的 index-url（如 https://mirrors.aliyun.com/pypi/simple）/ npm 的 registry（如 https://registry.npmmirror.com）'),
    python: str('Python', '建 venv 用的 Python', { default: 'python3' }),
    off: bool('不装依赖', '依赖已经在上传的目录里'),
  }, { appTypes: ['python', 'node'] }),
  service: { ...service, appTypes: [...svc, 'static'] },
  static: obj('静态站点', '选择软件中已安装的 nginx 实例托管站点；旧配置仍兼容独立运行', {
    nginxInstance: str('nginx 实例', '软件中安装的实例名称，或 system:nginx.service 使用现有系统 nginx（需 http include 站点目录）；支持目标变量，如 ${nginxInstance}', { needs: 'port' }),
    domains: list('站点域名', '同一个 nginx 可按域名托管多个站点；不填为此端口的默认站点', { needs: 'port' }),
    port: str('网站端口', '如 8081，或 127.0.0.1:8081 只让本机访问；配合 nginxInstance 使用已安装的 nginx。旧配置未绑定实例时沿用独立运行环境'),
    nginx: str('nginx 版本', 'nginx 版本系列，如 1.30', { needs: 'port', default: '服务器上 Ferry 已装的（没有就装最新稳定版）' }),
    spa: bool('前端路由（history 模式）', '找不到的路径返回 index.html', { needs: 'port', default: 'true' }),
    proxy: strMap('接口转发', '路径前缀 → 后端地址，如 /prod-api/: http://127.0.0.1:8080/（末尾带 / 时去掉前缀再转发）', { needs: 'port' }),
    maxBody: str('请求体上限', '经接口转发上传文件时的大小上限', { needs: 'port', default: '100m' }),
    server: list('nginx 原样配置', '原样加到 nginx server 块里的配置行，如 "location /files/ { alias /data/files/; }"', { needs: 'port' }),
    https: obj(
      'HTTPS',
      '服务器上的 Ferry 自动申请证书（Let\'s Encrypt）、每天检查续期。默认 HTTP 验证（域名解析到这台服务器、外网能访问 80 端口，网站端口要是 80）；80 不通或要通配符证书时用 DNS 验证',
      {
        domains: list('域名', '证书里的域名，如 [www.example.com, example.com]；通配符（*.example.com）只能用 DNS 验证', { required: true }),
        email: str('联系邮箱', '证书服务发到期提醒，可以不填'),
        port: str('https 端口', '', { default: '443' }),
        dns: pick('DNS 验证', '密钥在 Ferry 的「设置 → 部署 → HTTPS 证书的 DNS 密钥」里填', ['aliyun', 'dnspod', 'cloudflare'], { default: '不用（HTTP 验证）' }),
        redirect: bool('http 跳到 https', '', { default: 'true' }),
        ca: pick('证书服务', 'letsencrypt-staging 是测试用的（浏览器不认），也可以写 ACME 目录地址', ['letsencrypt', 'letsencrypt-staging'], { open: true, default: 'letsencrypt' }),
      },
      { needs: 'port' },
    ),
    link: str('链接', '额外维护的符号链接，指向当前版本（例如 nginx 的 root）', { unless: 'port' }),
    reload: str('切换后执行', '用 sh -c 执行，失败则切回，如 nginx -t && nginx -s reload', { unless: 'port' }),
    health: health(true),
  }, { appTypes: ['static'] }),
  hooks: obj('部署时执行的命令', '在服务器上执行（sh -c），过程显示在部署输出里；手动回滚时不执行。命令里可以用 $FERRY_RELEASE、$FERRY_PREVIOUS 等环境变量，java / binary 还能拿到服务的 env、envFile', {
    beforeSwitch: { ...list('切换前', '新版本放好之后、停止旧版本之前执行（工作目录是新版本的目录），如数据库迁移；失败时不切换，线上照旧'), kind: 'objList', item: hookItem() },
    afterSwitch: { ...list('切换后', '新版本就绪之后执行（工作目录是 running/），如预热、刷新缓存；失败时回滚（optional 的只提示）'), kind: 'objList', item: hookItem() },
  }),
  keepReleases: num('保留几个版本', '至少 2', { default: '服务器的设置（5）' }),
  root: str('部署根目录', '应用自己的部署根目录（应用目录为 <root>/apps/<应用>），用于把它放到另一个分区；可以用 ${变量}（如各服务器 vars 里的数据盘路径）', { default: '服务器的 root' }),
  vars: strMap('变量', '应用自己的变量（优先于服务器、项目的变量）'),
})

const target: SchemaNode = obj('服务器', '一台要部署到的服务器', {
  title: str('显示名', '界面上显示的名字，可以是中文', { default: '服务器的名字' }),
  host: str('服务器地址', 'IP 或域名', { required: true }),
  port: num('SSH 端口', '', { default: '22' }),
  user: str('SSH 用户', '', { default: 'root' }),
  auth: pick('登录方式', 'key：私钥或 ssh-agent；password：密码（密码不写在这里，存在系统凭据库）', ['key', 'password'], { default: 'key' }),
  identityFile: str('私钥', '相对配置文件所在目录，支持 ~；用「配置免密登录」可以自动生成并写入。登录方式是 password 时不要写'),
  hostKey: str('主机公钥指纹', 'SHA256:…', { default: '用 known_hosts' }),
  sudo: bool('用 sudo', '不是 root 登录时用 sudo -n 执行（需要免密 sudo）'),
  root: str('部署根目录', '必须归 root 且其他人不可写', { default: '/opt/<项目名>' }),
  runAs: str('运行用户', '服务默认的运行用户（不能是 root；服务器上没有时部署会自动创建）'),
  arch: pick('CPU 架构', '和服务器不符时报错', ['amd64', 'arm64'], { default: '自动' }),
  keepReleases: num('保留几个版本', '每个应用保留的版本数，至少 2', { default: '5' }),
  ferryDir: str('Ferry 组件目录', 'agent、Ferry 提供的 JDK 放在服务器上的哪里；必须归 root、其他人不可写，同一台服务器上的项目共用', { default: '/usr/local/lib/ferry' }),
  uploadStreams: num('上传并行连接数', '1～32：8 MB 以上切片后多条连接同时传，VPN 等对单条连接限速时调大', { default: '设置里的值（6）' }),
  confirm: bool('部署前确认', '部署、回滚前先确认', { default: 'true' }),
  production: bool('生产服务器', 'AI 助手的每个改动都要确认（不能「同类不再询问」），界面上标出「生产」'),
  aiChanges: bool('允许 AI 助手改动', '每步都要确认；false = 只能查看', { default: 'true' }),
  deployers: list('谁能部署', '只有这些人能部署、回滚、启停（各自在设置 → 部署里填的名字，或 用户@电脑名）；防误操作的简单名单，不是权限控制', { default: '谁都可以' }),
  apps: list('只部署这些应用', '应用名', { default: '全部应用' }),
  vars: strMap('变量', '这台服务器的变量（优先于项目变量）'),
})

const db = (name: string, tool: string, port: number, user: string): SchemaNode =>
  obj(name, `${name} 数据库（用 ${tool} 导出）；密码写在 envFile 里`, {
    database: str('库名', '* 为全部库', { required: true }),
    host: str('地址', '', { default: '127.0.0.1（写了容器时在容器里连本机）' }),
    port: num('端口', '', { default: String(port) }),
    user: str('用户', '', { default: user }),
    container: str('Docker 容器', '数据库在 Docker 容器里（如 1Panel 装的 1Panel-mysql-xxxx）：在容器里执行；没写密码时用容器自己的'),
    bin: str('程序目录', `${tool} 所在的目录（服务器上的绝对路径）；不在 PATH 里时写，如 Ferry 装的 MySQL 的 <安装目录>/mysql/bin`),
  })

const backup: SchemaNode = obj('数据备份', '一项数据备份：MySQL、PostgreSQL、目录、命令写一个', {
  name: str('名字', '小写字母、数字、横线；备份文件在 <root>/backups/<名字>/', { required: true }),
  mysql: db('MySQL / MariaDB', 'mysqldump', 3306, 'root'),
  postgres: db('PostgreSQL', 'pg_dump', 5432, 'postgres'),
  paths: list('目录或文件', '要打包的目录或文件（服务器上的绝对路径，可以用 ${变量}）'),
  command: str('备份命令', '输出就是备份内容的命令（以 root 执行），如 redis-cli --rdb -'),
  restore: str('恢复命令', '备份的内容从标准输入进来', { needs: 'command' }),
  envFile: list('环境变量文件', '执行时读的环境变量文件（服务器上）：MYSQL_PWD=…、PGPASSWORD=…', { single: true }),
  schedule: pick('什么时候备份', 'daily 每天 3 点、hourly 每小时、weekly 每周，或 systemd 的 OnCalendar（如 *-*-* 02:30:00）', ['daily', 'hourly', 'weekly'], { open: true, default: 'daily' }),
  keep: num('保留几份', '最多 1000', { default: '7' }),
  beforeDeploy: bool('部署前先备份', '失败就不部署'),
  targets: list('只在这些服务器上', '服务器名', { default: '全部服务器' }),
}, { oneOf: [['mysql', 'postgres', 'paths', 'command']], oneOfRequired: true })

export const schema: SchemaNode = obj('ferry.yaml', 'ferry.yaml', {
  version: pick('配置格式版本', '固定为 1', ['1'], { kind: 'num', type: '数字', required: true }),
  kind: pick('文件类型', '不写 = 项目配置；connections = 桌面版「新建连接」保存的服务器（只有 targets）', ['connections']),
  project: str('项目名', '小写字母、数字、. _ -；默认的服务名、部署根目录会用到', { required: true }),
  vars: strMap('项目变量', '字符串里用 ${名字} 引用'),
  git: obj('Git', '部署前对本地工作区的要求', { requireClean: bool('有未提交的修改时不部署', '') }),
  builds: { label: '构建', doc: '有名字的构建命令，应用里用名字引用', kind: 'objMap', type: '映射', map: build },
  targets: { label: '服务器', doc: '要部署到的服务器（键是服务器名）', kind: 'objMap', type: '映射', map: target, required: true },
  apps: { label: '应用', doc: '要部署的应用', kind: 'objList', type: '列表', item: app, required: true },
  backups: { label: '数据备份', doc: '数据库、目录、自己写的命令：在「数据备份」里打开后服务器上按时备份，随时可以关', kind: 'objList', type: '列表', item: backup },
})

/** 内置变量（部署时按目标、应用计算）。 */
export const builtinVars: Record<string, string> = {
  project: '项目名',
  target: '目标名',
  app: '应用名',
  root: '目标的部署根目录',
  appDir: '<root>/apps/<应用>',
  running: '运行中的版本目录 <appDir>/running：程序和它的日志',
  current: '同 running（以前的名字）',
  logs: '运行中版本的日志目录 <running>/logs：日志和程序放在一起',
  versions: '各版本所在目录 <appDir>/versions',
  releases: '同 versions（以前的名字）',
  data: '不随版本变化的目录 <appDir>/data',
  shared: '同 data（以前的名字）',
  artifact: '<running>/<产物文件名>',
  java: 'service.java 的值',
  user: '运行用户',
  host: '目标主机',
  configDir: '配置文件所在目录（只在本机路径、构建命令里）',
}

/** 按路径找结构说明。路径元素：字段名；映射的任意键用实际键名；列表元素用 '[]'。 */
export function lookup(path: string[]): SchemaNode | undefined {
  let node: SchemaNode | undefined = schema
  for (const p of path) {
    if (!node) return undefined
    if (p === '[]') node = node.item
    else node = node.fields?.[p] ?? node.map
  }
  return node
}

/** 说明文字：doc 加上可选值、默认值、必填、适用的应用类型（补全和悬停提示用）。 */
export function describe(n: SchemaNode): string {
  const parts = [n.label && n.label !== n.doc ? `${n.label}${n.doc ? '：' + n.doc : ''}` : n.doc]
  if (n.values && n.kind !== 'bool') parts.push(`可选：${n.values.join(' / ')}${n.open ? ' 或其他' : ''}`)
  if (n.default) parts.push(`不写时：${n.default}`)
  if (n.required === true) parts.push('必填')
  else if (Array.isArray(n.required)) parts.push(`${n.required.join('、')} 必填`)
  if (n.appTypes) parts.push(`只用于 ${n.appTypes.join('、')} 应用`)
  if (n.needs) parts.push(`写了 ${n.needs} 才有用`)
  if (n.unless) parts.push(`写了 ${n.unless} 时不用`)
  return parts.filter(Boolean).join('\n')
}

/** 所有字段路径（映射的任意键写 *，列表元素写 []），与 Go 的配置结构对照用。 */
export function fieldPaths(node: SchemaNode = schema, prefix = ''): string[] {
  const out: string[] = []
  const walk = (n: SchemaNode, at: string) => {
    if (n.fields) {
      for (const [k, c] of Object.entries(n.fields)) {
        const p = at ? `${at}.${k}` : k
        out.push(p)
        walk(c, p)
      }
    }
    if (n.kind === 'objMap' && n.map) walk(n.map, `${at}.*`)
    if (n.kind === 'objList' && n.item) walk(n.item, `${at}.[]`)
  }
  walk(node, prefix)
  return out.sort()
}
