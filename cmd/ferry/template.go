package main

const initTemplate = `# ferry 部署配置。字段说明见 docs/config.md。
# 字符串里可以用 ${变量}：vars（项目 / 目标 / 应用三级）、内置变量（root appDir current shared artifact java …）、
# ${env:NAME}（本机环境变量）、构建命令里的 ${each:变量}（同一条构建的各应用的值，逗号连接）。
version: 1
project: myapp

vars:
  profile: prod

# 构建：多个应用引用同一条构建时，一次部署只执行一次
builds:
  maven:
    command: mvn -pl ${each:module} -am clean package -DskipTests
    lock: maven            # 同名构建锁跨进程串行（避免两个部署同时打包）

targets:
  prod:
    host: 10.0.0.10
    port: 22
    user: root             # 非 root 时设置 sudo: true（需要免密 sudo）
    identityFile: ~/.ssh/id_ed25519
    root: /opt/myapp       # 部署根目录：必须归 root、其他用户不可写
    runAs: myapp           # 服务运行用户（需先在服务器上创建）
    keepReleases: 5

apps:
  - name: api
    type: java             # java | binary | static
    order: 10
    build: maven
    vars: {module: api, port: "8080"}
    artifact:
      path: api/target/api.jar
      verify:
        springBoot:
          requireLibs: ["myapp-common-*.jar"]
          nonEmptyLibs: true
    service:
      java: /usr/lib/jvm/java-21/bin/java
      jvmArgs: [-Xms256m, -Xmx512m, -Dfile.encoding=UTF-8]
      args: ["--spring.profiles.active=${profile}"]    # [ ] { } 里的 ${…} 要加引号
      envFile: -/etc/myapp/myapp.env          # - 开头：文件不存在也不报错
      sharedDirs: [logs]
      diagLogs: [logs/error.log]              # 相对 shared
      health: {tcp: "${port}", timeout: 180s}

  - name: web
    type: static
    order: 20
    build: {command: npm run build, workdir: web}
    artifact: {path: web/dist}
    static:
      link: /usr/share/nginx/html/myapp       # nginx root 指向这里（由 ferry 维护成指向 current 的链接）
      reload: nginx -t && nginx -s reload
`
