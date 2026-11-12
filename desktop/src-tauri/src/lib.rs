//! Ferry 桌面版：界面在 WebView 里，所有部署操作交给 sidecar（ferry 命令行，--json 输出）。
//! Rust 这边只负责窗口、插件，读写 ferry.yaml 的两个受限命令，以及内置浏览器（browser.rs）。

mod secretclip;
mod browser;
mod uicache;
mod shellicon;
#[cfg(windows)]
mod windowchrome;

use std::path::Path;
#[cfg(not(target_os = "macos"))]
use std::path::PathBuf;

use tauri::{WebviewUrl, WebviewWindowBuilder};

/// Ferry 的数据目录，与 ferry 命令行（sidecar）的规则一致：
/// 环境变量 FERRY_HOME → 手动设置的位置（安装目录 data/location.json）→ 安装目录下的 data。
/// 安装目录不可写时返回 None（WebView 用系统默认位置，ferry 用用户目录）。
/// macOS 上不用：数据不能放进 .app 包，ferry 用 ~/Library/Application Support/Ferry，WebView 用系统默认位置。
#[cfg(not(target_os = "macos"))]
fn data_dir() -> Option<PathBuf> {
    if let Ok(h) = std::env::var("FERRY_HOME") {
        if !h.is_empty() {
            return Some(PathBuf::from(h));
        }
    }
    let exe = std::env::current_exe().ok()?;
    let exe = std::fs::canonicalize(&exe).unwrap_or(exe);
    let data = exe.parent()?.join("data");
    std::fs::create_dir_all(&data).ok()?;
    let probe = data.join(".write-test");
    std::fs::write(&probe, b"").ok()?;
    let _ = std::fs::remove_file(&probe);
    if let Ok(text) = std::fs::read_to_string(data.join("location.json")) {
        if let Ok(v) = serde_json::from_str::<serde_json::Value>(&text) {
            if let Some(d) = v
                .get("dataDir")
                .and_then(|d| d.as_str())
                .filter(|d| !d.is_empty())
            {
                return Some(PathBuf::from(d));
            }
        }
    }
    Some(data)
}

/// 只允许读写 .yaml / .yml 文件（配置编辑器用），不开放通用文件系统权限给前端。
fn check_config_path(path: &str) -> Result<&Path, String> {
    let p = Path::new(path);
    if !p.is_absolute() {
        return Err(format!("必须是绝对路径：{path}"));
    }
    match p
        .extension()
        .and_then(|e| e.to_str())
        .map(|e| e.to_ascii_lowercase())
    {
        Some(ref e) if e == "yaml" || e == "yml" => Ok(p),
        _ => Err(format!("只能读写 YAML 配置文件：{path}")),
    }
}

/// 读写配置只给 Ferry 的界面用（内置浏览器里的网页不行）
fn main_only(webview: &tauri::Webview) -> Result<(), String> {
    if webview.label() == "main" {
        Ok(())
    } else {
        Err("不允许从这里调用".into())
    }
}

#[tauri::command]
fn read_config(webview: tauri::Webview, path: String) -> Result<String, String> {
    main_only(&webview)?;
    let p = check_config_path(&path)?;
    std::fs::read_to_string(p).map_err(|e| format!("读取 {path} 失败：{e}"))
}

#[tauri::command]
fn write_config(webview: tauri::Webview, path: String, content: String) -> Result<(), String> {
    main_only(&webview)?;
    let p = check_config_path(&path)?;
    // 先写临时文件再改名，避免写到一半损坏配置
    let tmp = p.with_extension("yaml.tmp");
    std::fs::write(&tmp, content).map_err(|e| format!("写入 {path} 失败：{e}"))?;
    std::fs::rename(&tmp, p).map_err(|e| {
        let _ = std::fs::remove_file(&tmp);
        format!("保存 {path} 失败：{e}")
    })
}

/// 用系统的浏览器打开网址（「软件」页「打开网页」、AI 助手回复里的链接）：只认 Ferry 自己的界面、只开 http / https。
/// 不放宽 opener 插件的 open-url 权限：那个权限只给 GitHub（更新、项目主页）
#[tauri::command]
fn open_external(webview: tauri::Webview, app: tauri::AppHandle, url: String) -> Result<(), String> {
    main_only(&webview)?;
    let u = tauri::Url::parse(&url).map_err(|e| format!("网址不对：{e}"))?;
    if !matches!(u.scheme(), "http" | "https") {
        return Err("只能打开 http / https 的网址".into());
    }
    use tauri_plugin_opener::OpenerExt;
    app.opener()
        .open_url(u.as_str(), None::<&str>)
        .map_err(|e| format!("打不开系统的浏览器：{e}"))
}

/// 退出 Ferry（更新：安装程序已经启动，由它装好后重新打开）。不经过关闭窗口的确认。
#[tauri::command]
fn exit_app(webview: tauri::Webview, app: tauri::AppHandle) -> Result<(), String> {
    main_only(&webview)?;
    app.exit(0);
    Ok(())
}

#[cfg_attr(mobile, tauri::mobile_entry_point)]
pub fn run() {
    tauri::Builder::default()
        // 只开一个 Ferry：再次打开时切到已经开着的窗口（两个窗口各有一个 ferry serve，任务、确认框会分散在两边）。要放在最前面
        .plugin(tauri_plugin_single_instance::init(|app, _args, _cwd| {
            use tauri::Manager;
            if let Some(w) = app.get_webview_window("main") {
                let _ = w.unminimize();
                let _ = w.show();
                let _ = w.set_focus();
            }
        }))
        .plugin(tauri_plugin_shell::init())
        .plugin(tauri_plugin_dialog::init())
        .plugin(tauri_plugin_store::Builder::new().build())
        .plugin(tauri_plugin_opener::init())
        .plugin(tauri_plugin_notification::init())
        .manage(browser::Browser::default())
        .invoke_handler(tauri::generate_handler![
            read_config,
            write_config,
            exit_app,
            shellicon::sync_shell_icon,
            shellicon::refresh_shell_icon,
            open_external,
            secretclip::copy_secret,
            browser::browser_allow_cert,
            browser::browser_tabs,
            browser::browser_new_tab,
            browser::browser_select_tab,
            browser::browser_close_tab,
            browser::browser_navigate,
            browser::browser_action,
            browser::browser_layout,
            browser::browser_release,
            browser::browser_sync,
            browser::browser_data_usage,
            browser::browser_clear_data,
            uicache::ui_cache_usage,
            uicache::ui_cache_clear
        ])
        .setup(|app| {
            // 主窗口在这里创建：WebView2 的缓存放在 Ferry 的数据目录里（默认安装目录下），不放到系统盘的用户目录
            #[cfg_attr(target_os = "macos", allow(unused_mut))] // macOS 上不改 WebView 数据目录
            let mut w = WebviewWindowBuilder::new(app, "main", WebviewUrl::default())
                .title("Ferry")
                .inner_size(1320.0, 840.0)
                .min_inner_size(1000.0, 620.0)
                // 开着 devtools 功能是给内置浏览器用的（macOS）；Ferry 自己的界面照旧只在开发版里能打开
                .devtools(cfg!(debug_assertions))
                // 先不显示：界面画好、主题切好后由页面显示（main.ts），免得先闪白底或浅色
                .visible(false);
            #[cfg(not(target_os = "macos"))]
            let redirected = data_dir();
            #[cfg(not(target_os = "macos"))]
            if let Some(d) = &redirected {
                w = w.data_directory(d.join("webview"));
            }
            let win = w.build()?;
            #[cfg(windows)]
            windowchrome::hide_branding(&win)?;
            {
                use tauri::Manager;
                // 内置浏览器的网页用单独的目录，能单独清理
                #[cfg(not(target_os = "macos"))]
                let dir = redirected.as_ref().map(|d| d.join("browser"));
                #[cfg(target_os = "macos")]
                let dir = None;
                app.manage(browser::BrowserDir(dir));
                #[cfg(not(target_os = "macos"))]
                let ui = redirected.as_ref().map(|d| d.join("webview"));
                #[cfg(target_os = "macos")]
                let ui = None;
                app.manage(uicache::UiDir(ui));
            }
            // 页面出错没能显示窗口时：3 秒后照样显示
            std::thread::spawn(move || {
                std::thread::sleep(std::time::Duration::from_secs(3));
                if !win.is_visible().unwrap_or(true) {
                    let _ = win.show();
                }
            });
            // 旧版本把界面缓存（EBWebView）放在系统盘的 %LOCALAPPDATA%\<identifier>，已经不用了：清掉。
            // 只在 Windows：其他系统上这个目录也是插件保存数据的地方
            #[cfg(windows)]
            // 显式隔离的数据目录不清理其他用户配置目录（也用于独立的桌面验收）。
            if redirected.is_some() && std::env::var_os("FERRY_HOME").is_none() {
                use tauri::Manager;
                if let Ok(old) = app.path().app_local_data_dir() {
                    std::thread::spawn(move || {
                        let _ = std::fs::remove_dir_all(old);
                    });
                }
            }
            Ok(())
        })
        .run(tauri::generate_context!())
        .expect("启动 Ferry 失败");
}
