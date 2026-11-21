# ferry.yaml configuration reference

[简体中文](../config.md) · [Quickstart](quickstart.md)

Ferry searches for `ferry.yaml` from the current directory upward. Use `-c path/to/ferry.yaml` to choose a file explicitly. Local paths such as artifacts, build directories, and SSH identities are relative to the configuration directory and support `~`. Run `ferry validate` to catch unknown fields, undefined variables, duplicate applications and duplicate service units. Ferry warns when a recognized field does not apply to an app type.

```yaml
version: 1
project: shop
targets:
  prod:
    host: 192.0.2.10
    user: deployer
    sudo: true
    runAs: shop
apps:
  - name: api
    type: java
    build: {command: mvn -q clean package}
    artifact: {path: target/shop.jar, verify: {springBoot: {}}}
    service:
      java: /usr/bin/java
      health: {http: "http://127.0.0.1:8080/health"}
```

This example uses Java installed on the server. The [runnable project](../../examples/quickstart/) includes the application source and build configuration. Run `ferry plan prod --units` to preview builds and generated systemd units.

## Variables

Use `${name}` in string fields. Ferry resolves built-ins first, then `apps[].vars`, `targets.<name>.vars`, and the top-level `vars`. `${env:NAME}` reads a required local environment variable. In a build command, `${each:name}` joins the unique values from the apps that use that build with commas. Use `$${` to keep a literal `${`.

| Built-in | Value |
| --- | --- |
| `project`, `target`, `app` | Project, target, and app names. |
| `root`, `appDir` | Target deployment root and the app directory under `apps/<app>`. An app's own `root` takes precedence for `appDir`. |
| `running`, `logs` | Active release directory and its `logs/` directory. `current` is an alias for `running`. |
| `versions`, `data` | Saved versions and persistent data. `releases` and `shared` are aliases. |
| `artifact` | Remote artifact under `running`. For directory-based apps this is the directory. |
| `user`, `host`, `java` | Service account, target host, and Java command. |
| `configDir` | Local directory containing `ferry.yaml`. Available in local paths and build commands. |
| `port`, `slot` | Active port and slot for [zero-downtime deployments](#zero-downtime). |

Quote variable references in inline YAML, for example `args: ["--profile=${profile}"]`. YAML anchors and merge keys are supported. Maps such as `env` use a shallow merge, so overriding the map replaces its contents.

## Top-level fields

| Field | Purpose |
| --- | --- |
| `version` | Set to `1`. |
| `project` | Project identifier using lowercase letters, digits, dots, underscores, or hyphens. Used in default paths and unit names. |
| `vars` | Project variables. |
| `git.requireClean` | Reject deployment when a source repository used by the selected apps has uncommitted changes. |
| `builds` | Named [builds](#builds). |
| `targets` | Map of [deployment targets](#targets). |
| `apps` | List of [applications](#applications). |
| `backups` | List of [backup definitions](#backups). |
| `rollout` | [Multi-server rollout policy](#multi-server-rollouts). |

Ferry records source revisions per app using the repository of its build directory, then its artifact. This allows one configuration to deploy apps from different repositories.

## Builds

`builds.<name>` defines a shared local build. An app can also supply the same fields inline under `build`.

| Field | Purpose |
| --- | --- |
| `command` | Local build command. |
| `workdir` | Build directory. Defaults to the configuration directory. |
| `shell` | `cmd`, `powershell`, `pwsh`, `bash`, or `sh`. Defaults to `cmd` on Windows and `sh` elsewhere. |
| `env` | Additional build environment variables. |
| `lock` | Lock name for serializing builds across Ferry processes. Defaults to the working directory. |

Ferry runs a build once per deployment even when several apps use it. Build logs are saved in `.ferry/logs/` next to the configuration. Use `-v` to stream output.

## Targets

| Field | Default | Purpose |
| --- | --- | --- |
| `host`, `port` | Required host, port `22` | SSH address and port. |
| `user`, `sudo` | `root`, `false` | Login user and whether Ferry uses sudo for privileged operations. |
| `sudoAuth` | `nopasswd` | `nopasswd` uses non-interactive sudo, `password` uses a separate sudo credential, and `disabled` disables elevation. |
| `auth` | `key` | `key` uses a private key or SSH agent. `password` also supports keyboard-interactive. |
| `identityFile` | Default SSH identities | Private key on the local machine, for example `~/.ssh/id_ed25519`. |
| `hostKey` | Known-hosts records | Pin an SSH fingerprint such as `SHA256:…`. Use `ferry hostkey prod` to read the presented fingerprint and verify it independently. |
| `root` | `/opt/<project>` | Deployment root. This directory and its parents must be root-owned and writable only to root. |
| `runAs` | Set a non-root account | Default runtime user. Ferry can create it during deployment. |
| `arch` | Detected | `amd64` or `arm64`. Ferry checks the configured value against the server. |
| `keepReleases` | `5` | Versions to retain per app. Ferry retains the current and previous working versions. |
| `ferryDir` | `/usr/local/lib/ferry` | Shared agent and JDK directory on the server. It follows the same root ownership and write restrictions. |
| `uploadStreams` | Settings default, normally `6` | Parallel upload streams, from 1 to 32. |
| `secretsFrom` | `keyring` | `keyring` or `file:secrets/prod.age`. See [application secrets](#application-secrets). |
| `confirm`, `production` | `true`, `false` | Require confirmation and mark a production target. On production, interactive deploy, rollback, stop, and restart require the target name. `--yes` is available for CI. |
| `aiChanges` | `true` | Allow AI actions on this target after approval. Production targets open in read-only mode and reject write requests from MCP clients. |
| `deployers` | All operators | Allowlist of operator names for deployment, rollback, and service controls. Enforce server access separately with SSH and sudo permissions. |
| `apps`, `vars` | All apps, empty variables | Limit the apps available on this target and override project variables. |
| `title`, `appRuntimes` | | Display name and per-app runtime overrides written by **切换运行环境 (Change runtime)**. See the [nginx guide](../software-config.md) (Chinese). |

### SSH credentials

Store passwords and passphrases in the system credential store or supply them to a CI job. Ferry first checks `FERRY_PASSWORD_<TARGET>` or `FERRY_PASSPHRASE_<12-HEX-CHARS>`, then the system credential store, and can prompt in an interactive terminal. Normalize the target to uppercase with punctuation replaced by underscores, as in `FERRY_PASSWORD_PROD_1` for `prod-1`. The passphrase suffix is the first 12 hexadecimal characters of the SHA-256 of the absolute identity path. Ferry's JSON error includes the exact variable name. Use `ferry secret set prod` or `ferry secret set --passphrase ~/.ssh/id_ed25519` to save credentials.

Use `ferry trust prod` to record a verified host key. `ferry keygen prod --local` can install a public key using a one-time password login and save the private key reference in this computer's settings. Use `--write` when you intend to write the reference to the shared configuration. In CI, pin `hostKey` and use `FERRY_SSH_KEY` or a target-specific `FERRY_SSH_KEY_<TARGET>`. [CI variables and examples](../ci.md) (Chinese)

## Applications

| Field | Purpose |
| --- | --- |
| `name`, `type` | Unique app name, case insensitive. Types are `java`, `binary`, `static`, `python`, `node`, and `dotnet`. |
| `order` | Lower values deploy and start first. Ferry stops in reverse order. |
| `build` | A name from `builds` or an inline build object. Omit for a prebuilt artifact. |
| `artifact.path` | A file for Java or native binaries. Use a directory for static sites and Python, Node.js, or .NET apps. |
| `artifact.exclude` | Names and relative path globs excluded from a directory artifact. Source-control and dependency caches are excluded for applicable app types. |
| `artifact.verify` | `springBoot: {}` checks the executable JAR layout. `springBoot.requireLibs` matches required dependency JARs and `springBoot.nonEmptyLibs` checks for classes in them. `static.index` sets the required file for a static site. `minSize` sets a byte minimum. |
| `artifact.files` | Files and directories shipped alongside a Java or native artifact. Each entry is a local path or `{from, to, exec}`. `to` is relative to the release and defaults to the source name. |
| `artifact.keep` | Carry files from the active version into the next. Use `{path: uploads, shared: true}` to keep one persistent copy in `data/` across deployments and rollbacks. |
| `deps` | Runtime dependency options for Python and Node.js. `registry` selects a package source, `python` selects the interpreter for the virtual environment, and `off: true` skips dependency installation. |
| `root`, `keepReleases`, `vars` | Override the deployment root, retention, or variables for one app. Changing an existing app's root leaves its old versions at the previous location. |
| `service`, `static` | Systemd [service](#services) or [static site](#static-sites) settings. |
| `hooks` | Remote commands during [deployment](#deployment-hooks). |
| `https`, `job` | [Backend HTTPS](#backend-https) or a [scheduled job](#scheduled-jobs). |

Ferry checks the ELF format and target architecture for a native executable. Extra files travel with the app version and are restored with it. Put application-writable paths under `${data}`, initialized with `service.sharedDirs`.

Python builds can export `requirements.txt` and run an entry point such as `${running}/.venv/bin/uvicorn`. Ferry prepares and reuses the virtual environment on the server as dependencies and run times allow. For Node.js use `service.exec: [node, "${running}/dist/main.js"]`. Ferry uses the lockfile's package manager and caches the production dependencies. For .NET you can publish a self-contained Linux executable and use it in `service.exec`, or run the DLL with `dotnet` installed on the server.

### Deployment hooks

Use `hooks.beforeSwitch` for commands that need the new release on disk before the old service stops. Use `hooks.afterSwitch` for commands that need the new service to be healthy. Each item is a shell command or `{run, user, timeout, optional}`. The default user is the service account, with `root` available explicitly. The timeout defaults to `5m` and accepts up to `2h`. Set `optional: true` to continue after an error.

A required failure before the switch leaves the current release running. A required failure after the switch triggers rollback. Hooks receive `service.env`, `service.envFile`, and metadata such as `FERRY_RELEASE`, `FERRY_PREVIOUS`, `FERRY_PHASE`, and `FERRY_DATA_DIR`. Manual rollback and ordinary start or stop operations omit deployment hooks. Plan database migrations so that the previous app can still run after a rollback.

## Services

| Field | Purpose |
| --- | --- |
| `unit`, `description`, `user` | Systemd unit name, description, and runtime user. Defaults to `<project>-<app>.service`, a generated description, and the target's `runAs`. |
| `workdir` | Service working directory. Defaults to `${running}`. |
| `java`, `runtime` | Java at an absolute server path (default `/usr/bin/java`), or a Ferry-managed JDK such as `runtime: {java: 21}`. |
| `jvmArgs`, `args`, `exec` | JVM arguments, app arguments, or the complete command as a list. `exec` is required for Python, Node.js, and .NET and accepts an executable from the systemd PATH for those types. |
| `env`, `envFile` | Systemd environment and one or more server-side environment files. A leading hyphen makes a file optional. |
| `secrets` | Names of managed environment variables. See [application secrets](#application-secrets). |
| `health` | `tcp` accepts a port or `host:port`. `http` accepts a URL with optional `status` (normally HTTP 2xx or 3xx) and `contains`. `timeout` defaults to `180s`. `stable` controls the healthy period before success and defaults to `10s` for a service without a probe. |
| `execStartPre`, `conflicts` | Raw `ExecStartPre=` commands and other units to stop during replacement. Ferry restores conflicting units if the switch fails. |
| `sharedDirs`, `diagLogs`, `archiveLogs` | Persistent subdirectories, diagnostic log paths, and legacy log directories to archive when switching versions. Logs written to `${logs}` are kept by version. |
| `restart` | Policy string or `{when, delay, max, within}`. Defaults to restarting on errors after `5s`. Set `when` to `always`, `on-failure`, or `no`. `max` and `within` limit repeated restart attempts. |
| `autostart`, `autostartDelay`, `after` | Enable at boot (default true), optional boot delay (1 second to 1 hour), and units to wait for. |
| `unitExtra.unit`, `unitExtra.service`, `unitExtra.install` | Additional lines appended to the matching systemd unit sections. |
| `zeroDowntime` | [Two-slot startup and switching](#zero-downtime). |

Run `ferry preflight prod api` to inspect the target. It reports blocked service paths and permission fixes that can be applied on deployment. Ferry checks the JAR's required Java version before uploading. Set `service.runtime: {java: 21}` to let Ferry obtain and reuse a compatible JDK, or import a Linux JDK archive with `ferry runtime import`. [Permission checks and runtime details](../config.md#servicejava--binary--python--node--dotnet) (Chinese)

### Application secrets

```yaml
service:
  secrets:
    - DB_PASSWORD
    - {name: SMS_KEY, optional: true, doc: SMS provider key}
```

Names follow `^[A-Z_][A-Z0-9_]*$`. Each value is a single UTF-8 line. Names must be unique and distinct from `service.env`.

CI variables like `FERRY_SECRET_API_DB_PASSWORD` take precedence. With `secretsFrom: keyring`, Ferry checks the credential for the current target, then the project's shared default. Use **应用密钥 (Application secrets)** or `ferry secrets prod set api DB_PASSWORD` to enter a value. Use `--all-targets` when saving a keyring default. Ferry stores the applied values beside the app version for systemd. A rollback restores the values saved for that version.

For `secretsFrom: file:secrets/prod.age`, keep the encrypted file in the project repository. Use forward slashes in the project-relative `.age` path.

1. Each team member runs `ferry secrets prod keygen` or uses **生成本机公钥 (Generate local public key)** under **团队共享 (Team sharing)**. The private key stays in the OS credential store.
2. The first member runs `ferry secrets prod team init` and saves the app values. An existing recipient adds more keys with `ferry secrets prod team add <public-key>` and commits the updated ciphertext.
3. Recipients deploy or use `ferry secrets prod push api` to update secrets in the running version. Add `--no-restart` to apply them on the next start. An existing recipient can revoke a key from the latest ciphertext with `team remove <public-key>`. Rotate values that were accessible from older commits when revoking access.

CI can supply an independent X25519 identity through `FERRY_AGE_IDENTITY`. Supplying every declared value, including optional ones, through per-variable CI settings bypasses file decryption.

### Zero downtime

```yaml
service:
  health: {http: "http://127.0.0.1:${port}/health"}
  zeroDowntime:
    ports: [18080, 18081]
    listen: "0.0.0.0:8080"
    drain: 30s
```

Ferry starts the new release on the unused port while the old one serves traffic. After the new release is healthy, Ferry switches the nginx entry point if `listen` is set, waits for `drain`, and stops the old release. Omit `listen` for apps reached through service discovery. `zeroDowntime.nginx` can select an nginx series. Applications receive `SERVER_PORT`, `PORT`, and can use `${port}` and `${slot}` when rendering settings. Size the server for two app instances during the switch.

## Static sites

| Field | Purpose |
| --- | --- |
| `static.nginxInstance`, `domains` | A named instance installed through **软件 (Software)** or `system:nginx.service`. Set domains to host multiple sites on one listener. |
| `static.port`, `nginx` | Listening port or address. Without a selected instance, Ferry can provide nginx and use the requested version series. |
| `static.spa`, `maxBody`, `server` | SPA index fallback (default true), request body limit (default `100m`), and raw lines appended to the nginx server block. |
| `static.proxy` | Map a path prefix to an HTTP upstream, for example `/api/: http://127.0.0.1:8080/`. An upstream URL with a path replaces the matched prefix. |
| `static.health`, `https` | Site health probe and TLS. `static.https` accepts `domains`, `email`, `port`, `dns`, `redirect`, and `ca`. |
| `static.link`, `reload` | Manage a symlink to the active release and optionally reload an existing web server after switching files. |
| `static.siteDir` | Update the files of an existing site in place. Use independently of `static.port` and `static.link`. |

For HTTP certificate validation, point the domains at the server and make port 80 reachable. Use DNS validation for wildcard certificates. Ferry requests the certificate on the server and checks it periodically for renewal. [Static nginx and HTTPS options](../config.md#static) (Chinese)

## Backend HTTPS

Set `apps[].https.domains` on a long-running Java, native, Python, Node.js, or .NET application to configure an nginx entry point. Ferry activates it after the application is ready and follows the current slot during a zero-downtime deployment. TLS lives outside the app release.

| Field | Purpose |
| --- | --- |
| `domains`, `upstream` | Certificate names and backend address. Ferry can infer the address from the health check or active slot. |
| `nginxInstance`, `port` | Existing nginx instance or Ferry-provided nginx. HTTPS defaults to port 443. |
| `dns`, `ca`, `email` | Optional DNS provider, ACME directory or `letsencrypt-staging`, and contact email. DNS supports `aliyun`, `dnspod`, `cloudflare`, `huaweicloud`, and `route53`. |
| `cert`, `key` | Absolute paths to an existing PEM chain and private key on the server. Supply both. |
| `redirect`, `maxBody`, `websocket` | HTTP redirect policy, body size limit (default `100m`), and WebSocket proxying (enabled by default). |

Default HTTP validation requires public ports 80 and 443. Use `ferry https-renew prod api` to check renewal. Removing `https` and redeploying removes the Ferry-managed backend entry. [Backend HTTPS examples](../config.md#后端-httpshttps) (Chinese)

## Multi-server rollouts

`ferry deploy a,b,c` preflights all targets, builds once, uploads in parallel, then switches the targets. By default it switches one at a time and stops when a target fails.

| `rollout` field | Purpose |
| --- | --- |
| `batch` | Targets switched concurrently per batch, from 1 to 50. Default 1. |
| `canary` | Number of initial targets in command order. Default 0. Ferry can ask for confirmation after the canary. |
| `observe` | Health observation period for the canary, up to 1 hour. |
| `onFailure` | `stop` leaves already switched targets at the new version. `rollback` restores those targets to the versions they ran before the rollout when those versions exist. |

Override the policy for one run with `ferry deploy a,b,c --batch 2 --canary 1 --observe 2m --on-failure rollback`. A first deployment may have no previous version to restore.

## Scheduled jobs

Add `job` to a Java, native, Python, Node.js, or .NET app that runs to completion. Ferry deploys a systemd oneshot unit with a timer. The unit and timer are saved with the release. Job success is determined by the exit status.

| Field | Purpose |
| --- | --- |
| `job.schedule` | One or more systemd OnCalendar expressions, for example `daily` or `"Mon..Fri 02:30"`. |
| `job.timeout` | Maximum run time, 1 second to 168 hours. Default `6h`. |
| `job.catchUp` | Run once after missing a scheduled run. Default true. |
| `job.runOnDeploy` | Run after deploy or rollback and require a successful exit. Default false. |

Manage jobs with `ferry run-job prod report` or **现在运行 (Run now)**. Job definitions exclude service health probes, restart policies, boot options, and zero-downtime settings.

## Backups

Define entries under `backups` and enable their timers with `ferry backup prod on` or **数据备份 (Data backups)**. Run one manually with `ferry backup prod run db`. Ferry saves an extra backup before a restore by default.

| Field | Purpose |
| --- | --- |
| `name`, `targets` | Name and optional list of targets. |
| `mysql`, `postgres` | `database` (required, `*` for all databases), `host`, `port`, `user`, `container`, and `bin`. Set `container` to run the database tool inside the appropriate container. |
| `paths` | Absolute server paths for a compressed file backup. |
| `command`, `restore` | Commands run as root. Backup data goes to stdout and enters the restore command on stdin. |
| `envFile` | Server-side environment file for credentials such as `MYSQL_PWD` or `PGPASSWORD`. |
| `schedule`, `keep` | Schedule and retention. Defaults to daily at 03:00 and seven backups. |
| `beforeDeploy` | If enabled, make a successful backup before deployment. |
| `upload` | Off-site storage for scheduled and manual backups. |

`upload.s3` accepts `endpoint`, `bucket`, `region`, `prefix`, and `pathStyle`. Use `upload.keep` for remote retention. Encryption defaults to enabled and uses age with `FERRY_BACKUP_PASSWORD`. Keep the password and credentials `S3_ACCESS_KEY_ID` and `S3_SECRET_ACCESS_KEY` in the server's backup environment file. An upload error preserves the local backup. Use `ferry backup prod remote db` to list off-site copies and `ferry backup prod fetch db <file>` to retrieve one before restoring it. [Backup examples](../config.md#数据备份backups) (Chinese)

## Server monitoring and teams

Set up a notification channel in **设置 → 通知 (Settings → Notifications)**. Run `ferry monitor prod on` or open **服务器告警 (Server alerts)** to enable checks for service health, memory, load, OOM events, disk space, certificates, firewall changes, exposed ports, and backup failures. Supply `--ping URL` to report heartbeat checks to an external monitor. Use `ferry monitor prod check` for an immediate check. [Alert settings](../config.md#服务器告警ferry-monitor) (Chinese)

Use **设置 → 部署 → 你的名字 (Settings → Deployment → Your name)** or `FERRY_ACTOR` to label deployment records. Each target's `production` and `deployers` settings govern interaction and approvals. Server-side deployment history is available to the team through **部署历史 (Deployment history)** or `ferry history prod api`. [Team guide](../guides/team.md) (Chinese)
