//! 内置浏览器：在主窗口右侧的抽屉里（BrowserDrawer.vue）。标签、地址栏是 Ferry 自己的界面；
//! 每个标签页是一个网页视图，开发者工具是另一个网页视图（DevTools 的界面，连到这个标签），都盖在抽屉里留出的位置上。
//! 位置由界面算好传过来（browser_layout），抽屉关着、上面有别的对话框时界面让这里藏起来。
//! 一个窗口里放多个网页视图要用 Tauri 的 unstable 功能（Window::add_child），版本由 Cargo.lock 固定。
//!
//! 浏览器的网页视图用单独的数据目录（数据目录下的 browser，和 Ferry 界面的分开，能单独清理），
//! 并打开只听本机的调试端口（随机端口）：停靠在页面里的开发者工具通过它连到网页。Ferry 界面不在这个端口上。
//!
//! 安全：网页是远程地址，Tauri 不允许远程页面调用 Ferry 的命令；这里的命令也都只认主窗口。
//! 标签页不能跳到 Ferry 界面的地址（tauri.localhost 等）和调试端口。

use std::collections::HashSet;
use std::path::{Path, PathBuf};
use std::sync::Mutex;

use serde::{Deserialize, Serialize};
use tauri::webview::{NewWindowResponse, PageLoadEvent, WebviewBuilder};
use tauri::window::Color;
use tauri::{
    AppHandle, Emitter, LogicalPosition, LogicalSize, Manager, Rect, Url, Webview, WebviewUrl,
};

const MAIN: &str = "main";
const TAB_PREFIX: &str = "browser-tab-";
const DEVTOOLS_PREFIX: &str = "browser-dt-";

/// 点 target=_blank 的链接（以及 Ctrl / Shift + 点击）时，opener、shell 插件放进每个网页视图的脚本会拦下来，
/// 改成「用系统浏览器打开」，而网页没有权限调用它，结果什么都不发生。
/// 这里在捕获阶段先处理（比插件早）：改成 window.open，由 on_new_window 开成新标签；中键点击也一样
const LINK_SCRIPT: &str = r#"(() => {
  const open = (e, force) => {
    if (e.defaultPrevented) return;
    const a = e.composedPath().find((n) => n instanceof HTMLAnchorElement || n instanceof SVGAElement);
    if (!a) return;
    const href = typeof a.href === 'string' ? a.href : a.href && a.href.baseVal;
    const target = a.getAttribute('target') || '';
    if (!href || !/^https?:/i.test(new URL(href, location.href).protocol) || a.hasAttribute('download')) return;
    if (!force && target.toLowerCase() !== '_blank') return;
    e.preventDefault();
    // shell 插件的脚本不看 defaultPrevented，只看 target：这次点击处理完之前先去掉，免得它再去调用（会在控制台报错）
    if (target) {
      a.removeAttribute('target');
      setTimeout(() => a.setAttribute('target', target));
    }
    window.open(new URL(href, location.href).href, '_blank', 'noopener');
  };
  addEventListener('click', (e) => { if (e.button === 0 && !e.altKey) open(e, e.ctrlKey || e.metaKey || e.shiftKey); }, true);
  addEventListener('auxclick', (e) => { if (e.button === 1) open(e, true); }, true);
})();"#;

/// 浏览器网页视图的数据目录（数据目录下的 browser）；None = 用系统默认位置（macOS）
pub struct BrowserDir(pub Option<PathBuf>);

#[derive(Default)]
pub struct Browser(Mutex<Tabs>);

#[derive(Default)]
struct Tabs {
    next: u32,
    active: u32,
    list: Vec<Tab>,
    /// 开发者工具连接用的本机调试端口，第一次建网页视图时选好
    port: Option<u16>,
    /// 界面给的位置：网页、开发者工具；None = 藏起来
    page: Option<Bounds>,
    devtools: Option<Bounds>,
    /// 藏起来、已经挂起 / 降低内存的网页视图（标签）
    parked: HashSet<String>,
}

#[derive(Clone, Serialize)]
#[serde(rename_all = "camelCase")]
pub struct Tab {
    id: u32,
    /// 空 = 新标签页（还没打开网页，也还没建网页视图，界面显示起始页）
    url: String,
    title: String,
    loading: bool,
    /// 开发者工具开着
    devtools: bool,
    /// 网页图标（data:image/png;base64,…），没有时为空
    icon: String,
}

#[derive(Clone, Serialize)]
pub struct Snapshot {
    tabs: Vec<Tab>,
    active: u32,
}

/// 抽屉里的位置（主窗口里的逻辑像素，和网页的 CSS 像素一样）
#[derive(Clone, Copy, Deserialize, PartialEq)]
pub struct Bounds {
    x: f64,
    y: f64,
    width: f64,
    height: f64,
}

impl Bounds {
    fn rect(&self) -> Rect {
        Rect {
            position: LogicalPosition::new(self.x, self.y).into(),
            size: LogicalSize::new(self.width.max(1.0), self.height.max(1.0)).into(),
        }
    }
}

fn tab_label(id: u32) -> String {
    format!("{TAB_PREFIX}{id}")
}

fn devtools_label(id: u32) -> String {
    format!("{DEVTOOLS_PREFIX}{id}")
}

fn tab_id(label: &str) -> Option<u32> {
    label
        .strip_prefix(TAB_PREFIX)
        .or_else(|| label.strip_prefix(DEVTOOLS_PREFIX))?
        .parse()
        .ok()
}

fn main_only(webview: &Webview) -> Result<(), String> {
    if webview.label() == MAIN {
        Ok(())
    } else {
        Err("不允许从这里调用".into())
    }
}

fn lock(app: &AppHandle) -> std::sync::MutexGuard<'_, Tabs> {
    app.state::<Browser>().inner().0.lock().unwrap()
}

/// 标签页可以打开的地址：http / https，以及页面自己用到的 about:、data:、blob:；
/// Ferry 界面自己的地址、开发者工具的调试端口不行
fn allowed(url: &Url, port: Option<u16>) -> bool {
    match url.scheme() {
        "http" | "https" => {
            let host = url.host_str().unwrap_or_default();
            if matches!(
                host,
                "tauri.localhost" | "ipc.localhost" | "asset.localhost"
            ) {
                return false;
            }
            let local = matches!(host, "127.0.0.1" | "localhost" | "[::1]");
            !(local && port.is_some() && url.port_or_known_default() == port)
        }
        "about" | "data" | "blob" => true,
        _ => false,
    }
}

fn parse(url: &str, port: Option<u16>) -> Result<Url, String> {
    let u = Url::parse(url.trim()).map_err(|e| format!("网址不对：{e}"))?;
    if !matches!(u.scheme(), "http" | "https") || !allowed(&u, port) {
        return Err(format!("只能打开 http / https 网址：{url}"));
    }
    Ok(u)
}

fn snapshot(app: &AppHandle) -> Snapshot {
    let t = lock(app);
    Snapshot {
        tabs: t.list.clone(),
        active: t.active,
    }
}

fn changed(app: &AppHandle) {
    let _ = app.emit_to(MAIN, "browser-tabs", snapshot(app));
}

fn update(app: &AppHandle, label: &str, f: impl FnOnce(&mut Tab)) {
    let Some(id) = tab_id(label) else { return };
    {
        let mut t = lock(app);
        match t.list.iter_mut().find(|t| t.id == id) {
            Some(tab) => f(tab),
            None => return,
        }
    }
    changed(app);
}

/// 按界面给的位置摆放：只显示当前标签的网页和它的开发者工具，其他的藏起来
fn layout(app: &AppHandle) {
    let Some(w) = app.get_window(MAIN) else {
        return;
    };
    let (active, page, devtools, show_devtools) = {
        let t = lock(app);
        let tab = t.list.iter().find(|x| x.id == t.active);
        (
            t.active,
            t.page,
            t.devtools,
            tab.is_some_and(|x| x.devtools),
        )
    };
    for v in w.webviews() {
        let label = v.label();
        let Some(id) = tab_id(label) else { continue };
        let target = if label.starts_with(TAB_PREFIX) {
            page
        } else if show_devtools {
            devtools
        } else {
            None
        };
        match target.filter(|_| id == active) {
            Some(b) => {
                let _ = v.set_bounds(b.rect());
                let _ = v.show();
                if lock(app).parked.remove(label) {
                    park(&v, false);
                }
            }
            None => {
                let _ = v.hide();
                if lock(app).parked.insert(label.to_string()) {
                    park(&v, true);
                }
            }
        }
    }
}

/// 藏起来的网页省资源：网页挂起（脚本、定时器停下，不占 CPU），并让浏览器尽量少占内存；再显示时恢复。
/// 开发者工具只降内存，不挂起（它和网页之间连着调试连接）
#[cfg(windows)]
fn park(v: &Webview, on: bool) {
    use webview2_com::Microsoft::Web::WebView2::Win32::{
        ICoreWebView2_19, ICoreWebView2_3, COREWEBVIEW2_MEMORY_USAGE_TARGET_LEVEL_LOW,
        COREWEBVIEW2_MEMORY_USAGE_TARGET_LEVEL_NORMAL,
    };
    use webview2_com::TrySuspendCompletedHandler;
    use windows::core::Interface;

    let page = v.label().starts_with(TAB_PREFIX);
    let _ = v.with_webview(move |pw| unsafe {
        let Ok(core) = pw.controller().CoreWebView2() else {
            return;
        };
        if let Ok(c) = core.cast::<ICoreWebView2_19>() {
            let _ = c.SetMemoryUsageTargetLevel(if on {
                COREWEBVIEW2_MEMORY_USAGE_TARGET_LEVEL_LOW
            } else {
                COREWEBVIEW2_MEMORY_USAGE_TARGET_LEVEL_NORMAL
            });
        }
        if !page {
            return;
        }
        if let Ok(c) = core.cast::<ICoreWebView2_3>() {
            if on {
                // 放音乐、视频时等情况 WebView2 会不挂起，那就照样跑
                let _ = c.TrySuspend(&TrySuspendCompletedHandler::create(Box::new(|_, _| Ok(()))));
            } else {
                let _ = c.Resume();
            }
        }
    });
}

#[cfg(not(windows))]
fn park(_: &Webview, _: bool) {}

/// 选一个本机空闲的端口给开发者工具用
fn port(app: &AppHandle) -> Option<u16> {
    if !cfg!(windows) {
        return None;
    }
    let mut t = lock(app);
    if t.port.is_none() {
        t.port = std::net::TcpListener::bind("127.0.0.1:0")
            .and_then(|l| l.local_addr())
            .map(|a| a.port())
            .ok();
    }
    t.port
}

/// 浏览器网页视图共用的设置（同一个数据目录的网页视图，启动参数必须一样）
fn configure(app: &AppHandle, mut b: WebviewBuilder<tauri::Wry>) -> WebviewBuilder<tauri::Wry> {
    if let Some(d) = &app.state::<BrowserDir>().0 {
        b = b.data_directory(d.clone());
    }
    if let Some(p) = port(app) {
        // 前两项是 Tauri 默认的参数（指定了就不再加默认的）
        b = b.additional_browser_args(&format!(
            "--disable-features=msWebOOUI,msPdfOOUI,msSmartScreenProtection --remote-debugging-address=127.0.0.1 --remote-debugging-port={p} --remote-allow-origins=http://127.0.0.1:{p}"
        ));
    }
    // F12 等不打开 WebView2 自己的开发者工具窗口，由这里转给界面（hook_keys），在抽屉里打开
    b.devtools(!cfg!(windows))
}

fn create_view(app: &AppHandle, id: u32, url: Url) -> Result<(), String> {
    let win = app.get_window(MAIN).ok_or("主窗口不在")?;
    let (a1, a2, a3, a4) = (app.clone(), app.clone(), app.clone(), app.clone());
    let b = WebviewBuilder::new(tab_label(id), WebviewUrl::External(url))
        .zoom_hotkeys_enabled(true)
        .initialization_script(LINK_SCRIPT)
        .background_color(Color(255, 255, 255, 255))
        .on_navigation(move |u| {
            let port = lock(&a1).port;
            if !allowed(u, port) {
                return false;
            }
            let u = u.to_string();
            update(&a1, &tab_label(id), |t| {
                t.url = u;
                t.loading = true;
            });
            true
        })
        .on_page_load(move |v, p| {
            let url = p.url().to_string();
            let done = p.event() == PageLoadEvent::Finished;
            update(&a2, v.label(), |t| {
                t.url = url;
                t.loading = !done;
            });
        })
        .on_document_title_changed(move |v, title| {
            let url = v.url().map(|u| u.to_string()).ok();
            update(&a3, v.label(), |t| {
                t.title = title;
                if let Some(u) = url {
                    t.url = u;
                }
            });
        })
        .on_new_window(move |u, _| new_window(&a4, u));
    let v = win
        .add_child(
            configure(app, b),
            LogicalPosition::new(0.0, 0.0),
            LogicalSize::new(1.0, 1.0),
        )
        .map_err(|e| format!("打开网页失败：{e}"))?;
    let _ = v.hide();
    hook_keys(app, &v);
    hook_favicon(app, &v);
    Ok(())
}

/// 网页要开新窗口（target=_blank、window.open）：开成新标签。这里在界面线程上，建网页视图要换个线程
fn new_window(app: &AppHandle, u: Url) -> NewWindowResponse<tauri::Wry> {
    let port = lock(app).port;
    if allowed(&u, port) && matches!(u.scheme(), "http" | "https") {
        let app = app.clone();
        tauri::async_runtime::spawn(async move {
            let _ = new_tab(&app, Some(u));
        });
    }
    NewWindowResponse::Deny
}

fn new_tab(app: &AppHandle, url: Option<Url>) -> Result<(), String> {
    let id = {
        let mut t = lock(app);
        t.next += 1;
        let id = t.next;
        t.list.push(Tab {
            id,
            url: url.as_ref().map(|u| u.to_string()).unwrap_or_default(),
            title: String::new(),
            loading: url.is_some(),
            devtools: false,
            icon: String::new(),
        });
        t.active = id;
        id
    };
    if let Some(u) = url {
        if let Err(e) = create_view(app, id, u) {
            lock(app).list.retain(|t| t.id != id);
            changed(app);
            return Err(e);
        }
    }
    activate(app, id);
    Ok(())
}

fn activate(app: &AppHandle, id: u32) {
    let (blank, url) = {
        let mut t = lock(app);
        let Some(tab) = t.list.iter().find(|x| x.id == id) else {
            return;
        };
        let blank = tab.url.is_empty();
        let url = tab.url.clone();
        t.active = id;
        (blank, url)
    };
    // 网页视图释放掉了（抽屉收起太久，browser_release）：按记住的地址重新打开
    if !blank && app.get_webview(&tab_label(id)).is_none() {
        if let Ok(u) = Url::parse(&url) {
            let _ = create_view(app, id, u);
        }
    }
    layout(app);
    changed(app);
    // 键盘焦点：有网页给网页，新标签页给界面（地址栏）
    let target = if blank { MAIN.into() } else { tab_label(id) };
    if let Some(v) = app.get_webview(&target) {
        let _ = v.set_focus();
    }
}

fn active_view(app: &AppHandle) -> Option<Webview> {
    let id = lock(app).active;
    app.get_webview(&tab_label(id))
}

// ---------- 开发者工具 ----------

/// 网页视图在调试端口上的编号（WebView2 的 Target.getTargetInfo）
#[cfg(windows)]
fn target_id(v: &Webview) -> Result<String, String> {
    use webview2_com::CallDevToolsProtocolMethodCompletedHandler;
    use windows::core::w;

    let (tx, rx) = std::sync::mpsc::channel::<Result<String, String>>();
    let tx2 = tx.clone();
    v.with_webview(move |pw| unsafe {
        let r = pw.controller().CoreWebView2().and_then(|core| {
            core.CallDevToolsProtocolMethod(
                w!("Target.getTargetInfo"),
                w!("{}"),
                &CallDevToolsProtocolMethodCompletedHandler::create(Box::new(move |hr, json| {
                    let _ = tx2.send(hr.map(|_| json).map_err(|e| e.to_string()));
                    Ok(())
                })),
            )
        });
        if let Err(e) = r {
            let _ = tx.send(Err(e.to_string()));
        }
    })
    .map_err(|e| e.to_string())?;
    let json = rx
        .recv_timeout(std::time::Duration::from_secs(5))
        .map_err(|_| "等网页回应超时".to_string())??;
    let v: serde_json::Value = serde_json::from_str(&json).map_err(|e| e.to_string())?;
    v["targetInfo"]["targetId"]
        .as_str()
        .map(str::to_string)
        .ok_or_else(|| "没拿到网页的调试编号".into())
}

/// 当前标签的开发者工具：开着就关，关着就开（停靠在抽屉里网页的右边）
fn toggle_devtools(app: &AppHandle) -> Result<(), String> {
    let id = lock(app).active;
    let Some(page) = app.get_webview(&tab_label(id)) else {
        return Ok(()); // 新标签页还没有网页
    };
    if !cfg!(windows) {
        // 其他系统：系统自带的开发者工具窗口
        if page.is_devtools_open() {
            page.close_devtools();
        } else {
            page.open_devtools();
        }
        return Ok(());
    }
    let open = {
        let mut t = lock(app);
        let tab = t.list.iter_mut().find(|x| x.id == id).ok_or("没有标签页")?;
        tab.devtools = !tab.devtools;
        tab.devtools
    };
    if open && app.get_webview(&devtools_label(id)).is_none() {
        if let Err(e) = create_devtools(app, id, &page) {
            if let Some(tab) = lock(app).list.iter_mut().find(|x| x.id == id) {
                tab.devtools = false;
            }
            changed(app);
            return Err(e);
        }
    }
    layout(app);
    changed(app);
    let focus = if open {
        devtools_label(id)
    } else {
        tab_label(id)
    };
    if let Some(v) = app.get_webview(&focus) {
        let _ = v.set_focus();
    }
    Ok(())
}

#[cfg(windows)]
fn create_devtools(app: &AppHandle, id: u32, page: &Webview) -> Result<(), String> {
    let port = lock(app).port.ok_or("没有调试端口")?;
    let target = target_id(page)?;
    let base = format!("http://127.0.0.1:{port}/devtools/");
    let url = Url::parse(&format!(
        "{base}devtools_app.html?ws=127.0.0.1:{port}/devtools/page/{target}"
    ))
    .map_err(|e| e.to_string())?;
    let win = app.get_window(MAIN).ok_or("主窗口不在")?;
    let a = app.clone();
    let b = WebviewBuilder::new(devtools_label(id), WebviewUrl::External(url))
        // 开发者工具自己只在调试端口上的页面里跳转；里面的链接（文档等）开成新标签
        .on_navigation(move |u| u.as_str().starts_with(&base))
        .on_new_window(move |u, _| new_window(&a, u));
    let v = win
        .add_child(
            configure(app, b),
            LogicalPosition::new(0.0, 0.0),
            LogicalSize::new(1.0, 1.0),
        )
        .map_err(|e| format!("打开开发者工具失败：{e}"))?;
    let _ = v.hide();
    hook_keys(app, &v);
    Ok(())
}

#[cfg(not(windows))]
fn create_devtools(_: &AppHandle, _: u32, _: &Webview) -> Result<(), String> {
    Ok(())
}

/// 焦点在网页里时的快捷键：F12 / Ctrl+Shift+I 开关开发者工具，Ctrl+T / Ctrl+W / Ctrl+L 和在工具栏里一样。
/// 拦下来交给界面（browser-key 事件），WebView2 自己不再处理
#[cfg(windows)]
fn hook_keys(app: &AppHandle, v: &Webview) {
    use webview2_com::AcceleratorKeyPressedEventHandler;
    use webview2_com::Microsoft::Web::WebView2::Win32::{
        COREWEBVIEW2_KEY_EVENT_KIND, COREWEBVIEW2_KEY_EVENT_KIND_KEY_DOWN,
        COREWEBVIEW2_KEY_EVENT_KIND_SYSTEM_KEY_DOWN, COREWEBVIEW2_PHYSICAL_KEY_STATUS,
    };
    use windows::Win32::UI::Input::KeyboardAndMouse::{GetKeyState, VK_CONTROL, VK_F12, VK_SHIFT};

    let app = app.clone();
    let _ = v.with_webview(move |pw| unsafe {
        let handler = AcceleratorKeyPressedEventHandler::create(Box::new(move |_, args| {
            let Some(args) = args else { return Ok(()) };
            let mut kind = COREWEBVIEW2_KEY_EVENT_KIND::default();
            args.KeyEventKind(&mut kind)?;
            if kind != COREWEBVIEW2_KEY_EVENT_KIND_KEY_DOWN
                && kind != COREWEBVIEW2_KEY_EVENT_KIND_SYSTEM_KEY_DOWN
            {
                return Ok(());
            }
            let mut vk = 0u32;
            args.VirtualKey(&mut vk)?;
            let ctrl = GetKeyState(VK_CONTROL.0 as i32) < 0;
            let shift = GetKeyState(VK_SHIFT.0 as i32) < 0;
            let key = match vk {
                k if k == VK_F12.0 as u32 => Some("devtools"),
                0x49 | 0x4A | 0x43 if ctrl && shift => Some("devtools"), // I J C
                0x54 if ctrl && !shift => Some("new-tab"),               // T
                0x57 if ctrl && !shift => Some("close-tab"),             // W
                0x4C if ctrl && !shift => Some("address"),               // L
                _ => None,
            };
            if let Some(key) = key {
                args.SetHandled(true)?;
                let mut status = COREWEBVIEW2_PHYSICAL_KEY_STATUS::default();
                let _ = args.PhysicalKeyStatus(&mut status);
                if !status.WasKeyDown.as_bool() {
                    // 要在地址栏里输入：把键盘焦点还给界面
                    // （换个线程再切：在 WebView2 的事件回调里直接切焦点会重入）
                    if matches!(key, "new-tab" | "address") {
                        if let Some(m) = app.get_webview(MAIN) {
                            tauri::async_runtime::spawn(async move {
                                let _ = m.set_focus();
                            });
                        }
                    }
                    let _ = app.emit_to(MAIN, "browser-key", key);
                }
            }
            Ok(())
        }));
        let mut token = Default::default();
        let _ = pw
            .controller()
            .add_AcceleratorKeyPressed(&handler, &mut token);
    });
}

#[cfg(not(windows))]
fn hook_keys(_: &AppHandle, _: &Webview) {}

/// 图标太大就不要了（每次标签变化都会把图标发给界面）
const MAX_ICON: usize = 48 * 1024;

/// 网页图标：WebView2 的 FaviconChanged 通知时取 PNG，转成 data: 地址放到标签上
/// （界面的内容安全策略只允许 data: 图片，不能直接用网页里的图标地址）
#[cfg(windows)]
fn hook_favicon(app: &AppHandle, v: &Webview) {
    use base64::Engine;
    use webview2_com::Microsoft::Web::WebView2::Win32::{
        ICoreWebView2_15, COREWEBVIEW2_FAVICON_IMAGE_FORMAT_PNG,
    };
    use webview2_com::{FaviconChangedEventHandler, GetFaviconCompletedHandler};
    use windows::core::{Interface, PWSTR};

    let app = app.clone();
    let label = v.label().to_string();
    let _ = v.with_webview(move |pw| unsafe {
        let Ok(core) = pw.controller().CoreWebView2() else {
            return;
        };
        let Ok(core) = core.cast::<ICoreWebView2_15>() else {
            return; // WebView2 太旧，没有图标接口
        };
        let handler = FaviconChangedEventHandler::create(Box::new(move |sender, _| {
            let Some(core) = sender.and_then(|s| s.cast::<ICoreWebView2_15>().ok()) else {
                return Ok(());
            };
            let mut uri = PWSTR::null();
            let has = core.FaviconUri(&mut uri).is_ok() && !uri.is_null() && {
                let s = uri.to_string().unwrap_or_default();
                windows::Win32::System::Com::CoTaskMemFree(Some(uri.0 as _));
                !s.is_empty()
            };
            if !has {
                update(&app, &label, |t| t.icon.clear());
                return Ok(());
            }
            let (app, label) = (app.clone(), label.clone());
            let done = GetFaviconCompletedHandler::create(Box::new(move |hr, stream| {
                let png = hr
                    .ok()
                    .and(stream)
                    .and_then(|s| read_stream(&s))
                    .filter(|b| !b.is_empty() && b.len() <= MAX_ICON);
                let icon = png
                    .map(|b| {
                        "data:image/png;base64,".to_string()
                            + &base64::engine::general_purpose::STANDARD.encode(b)
                    })
                    .unwrap_or_default();
                update(&app, &label, |t| t.icon = icon);
                Ok(())
            }));
            let _ = core.GetFavicon(COREWEBVIEW2_FAVICON_IMAGE_FORMAT_PNG, &done);
            Ok(())
        }));
        let mut token = Default::default();
        let _ = core.add_FaviconChanged(&handler, &mut token);
    });
}

/// 把 COM 流读完（最多读 MAX_ICON 多一点，超过的不要）
#[cfg(windows)]
unsafe fn read_stream(s: &windows::Win32::System::Com::IStream) -> Option<Vec<u8>> {
    let mut out = Vec::new();
    let mut buf = [0u8; 8192];
    loop {
        let mut n = 0u32;
        let hr = s.Read(buf.as_mut_ptr() as _, buf.len() as u32, Some(&mut n));
        if hr.is_err() {
            return None;
        }
        if n == 0 {
            return Some(out);
        }
        out.extend_from_slice(&buf[..n as usize]);
        if out.len() > MAX_ICON {
            return None;
        }
    }
}

#[cfg(not(windows))]
fn hook_favicon(_: &AppHandle, _: &Webview) {}

// ---------- 数据 ----------

pub(crate) fn dir_size(p: &Path) -> u64 {
    let Ok(rd) = std::fs::read_dir(p) else {
        return 0;
    };
    rd.flatten()
        .map(|e| match e.file_type() {
            Ok(t) if t.is_dir() => dir_size(&e.path()),
            Ok(t) if t.is_file() => e.metadata().map(|m| m.len()).unwrap_or(0),
            _ => 0,
        })
        .sum()
}

// ---------- 命令 ----------
// 都是 async：同步命令跑在界面线程上，add_child 要等界面线程，会卡死

#[tauri::command]
pub async fn browser_tabs(app: AppHandle, webview: Webview) -> Result<Snapshot, String> {
    main_only(&webview)?;
    Ok(snapshot(&app))
}

#[tauri::command]
pub async fn browser_new_tab(
    app: AppHandle,
    webview: Webview,
    url: Option<String>,
) -> Result<(), String> {
    main_only(&webview)?;
    let port = lock(&app).port;
    let url = url.as_deref().map(|u| parse(u, port)).transpose()?;
    new_tab(&app, url)
}

#[tauri::command]
pub async fn browser_select_tab(app: AppHandle, webview: Webview, id: u32) -> Result<(), String> {
    main_only(&webview)?;
    activate(&app, id);
    Ok(())
}

/// 关掉一个标签：关的是当前标签时切到旁边那个
#[tauri::command]
pub async fn browser_close_tab(app: AppHandle, webview: Webview, id: u32) -> Result<(), String> {
    main_only(&webview)?;
    let next = {
        let mut t = lock(&app);
        let Some(i) = t.list.iter().position(|x| x.id == id) else {
            return Ok(());
        };
        t.list.remove(i);
        match t.list.len() {
            0 => 0,
            n if t.active == id => t.list[i.min(n - 1)].id,
            _ => t.active,
        }
    };
    for label in [devtools_label(id), tab_label(id)] {
        lock(&app).parked.remove(&label);
        if let Some(v) = app.get_webview(&label) {
            let _ = v.close();
        }
    }
    if next == 0 {
        lock(&app).active = 0;
        changed(&app);
    } else {
        activate(&app, next);
    }
    Ok(())
}

/// 当前标签打开网址（新标签页这时才建网页视图）
#[tauri::command]
pub async fn browser_navigate(app: AppHandle, webview: Webview, url: String) -> Result<(), String> {
    main_only(&webview)?;
    let port = lock(&app).port;
    let u = parse(&url, port)?;
    if let Some(v) = active_view(&app) {
        v.navigate(u).map_err(|e| e.to_string())?;
        let _ = v.set_focus();
        return Ok(());
    }
    let id = {
        let mut t = lock(&app);
        let id = t.active;
        let tab = t.list.iter_mut().find(|x| x.id == id).ok_or("没有标签页")?;
        tab.url = u.to_string();
        tab.loading = true;
        id
    };
    create_view(&app, id, u)?;
    activate(&app, id);
    if let Some(v) = active_view(&app) {
        let _ = v.set_focus();
    }
    Ok(())
}

/// 后退、前进、刷新、停止、开发者工具、把焦点给网页
#[tauri::command]
pub async fn browser_action(
    app: AppHandle,
    webview: Webview,
    action: String,
) -> Result<(), String> {
    main_only(&webview)?;
    if action == "devtools" {
        return toggle_devtools(&app);
    }
    let Some(v) = active_view(&app) else {
        return Ok(());
    };
    match action.as_str() {
        "back" => v.eval("history.back()").map_err(|e| e.to_string())?,
        "forward" => v.eval("history.forward()").map_err(|e| e.to_string())?,
        "reload" => v.reload().map_err(|e| e.to_string())?,
        "stop" => v.eval("window.stop()").map_err(|e| e.to_string())?,
        "focus" => v.set_focus().map_err(|e| e.to_string())?,
        _ => return Err(format!("不认识的操作：{action}")),
    }
    Ok(())
}

/// 网页和开发者工具放在哪里（主窗口里的位置）；page 为空 = 都藏起来（抽屉关着、上面有对话框、新标签页）
#[tauri::command]
pub async fn browser_layout(
    app: AppHandle,
    webview: Webview,
    page: Option<Bounds>,
    devtools: Option<Bounds>,
) -> Result<(), String> {
    main_only(&webview)?;
    {
        let mut t = lock(&app);
        let devtools = devtools.filter(|_| page.is_some());
        if t.page == page && t.devtools == devtools {
            return Ok(());
        }
        t.page = page;
        t.devtools = devtools;
    }
    layout(&app);
    Ok(())
}

/// 抽屉收起很久了：关掉所有网页和开发者工具（浏览器的进程随之退出，不再占内存），标签和地址留着，
/// 再切到这个标签时重新打开（activate）
#[tauri::command]
pub async fn browser_release(app: AppHandle, webview: Webview) -> Result<(), String> {
    main_only(&webview)?;
    let views: Vec<Webview> = app
        .get_window(MAIN)
        .map(|w| w.webviews())
        .unwrap_or_default()
        .into_iter()
        .filter(|v| tab_id(v.label()).is_some())
        .collect();
    if views.is_empty() {
        return Ok(());
    }
    {
        let mut t = lock(&app);
        t.parked.clear();
        for tab in &mut t.list {
            tab.devtools = false;
            tab.loading = false;
        }
    }
    for v in views {
        let _ = v.close();
    }
    changed(&app);
    Ok(())
}

/// 单页网站换页面时不触发加载事件：界面每秒问一次当前网页的地址
#[tauri::command]
pub async fn browser_sync(app: AppHandle, webview: Webview) -> Result<(), String> {
    main_only(&webview)?;
    let Some(v) = active_view(&app) else {
        return Ok(());
    };
    let Ok(url) = v.url() else { return Ok(()) };
    let url = url.to_string();
    let same = {
        let t = lock(&app);
        t.list.iter().any(|x| x.id == t.active && x.url == url)
    };
    if !same {
        update(&app, v.label(), |t| t.url = url);
    }
    Ok(())
}

/// 浏览器的数据（缓存、Cookie、登录状态、网站存的数据）占用多少字节；None = 不在 Ferry 的数据目录里（不能单独清理）
#[tauri::command]
pub async fn browser_data_usage(app: AppHandle, webview: Webview) -> Result<Option<u64>, String> {
    main_only(&webview)?;
    let Some(d) = app.state::<BrowserDir>().0.clone() else {
        return Ok(None);
    };
    tauri::async_runtime::spawn_blocking(move || Some(dir_size(&d)))
        .await
        .map_err(|e| e.to_string())
}

/// 清除浏览器的数据。开着网页时由 WebView2 清（文件被占用，不能直接删），没开时删掉整个目录
#[tauri::command]
pub async fn browser_clear_data(app: AppHandle, webview: Webview) -> Result<(), String> {
    main_only(&webview)?;
    let Some(d) = app.state::<BrowserDir>().0.clone() else {
        return Err("这个系统上浏览器的数据和 Ferry 界面的在一起，不能单独清理".into());
    };
    let open = app
        .get_window(MAIN)
        .map(|w| w.webviews())
        .unwrap_or_default()
        .into_iter()
        .find(|v| v.label().starts_with(TAB_PREFIX));
    if let Some(v) = open {
        return v
            .clear_all_browsing_data()
            .map_err(|e| format!("清除失败：{e}"));
    }
    tauri::async_runtime::spawn_blocking(move || {
        // 刚关掉最后一个网页时浏览器进程可能还没退出，等一会儿再试
        for i in 0..10 {
            match std::fs::remove_dir_all(&d) {
                Ok(()) => return Ok(()),
                Err(e) if e.kind() == std::io::ErrorKind::NotFound => return Ok(()),
                Err(e) if i == 9 => return Err(format!("清除失败：{e}")),
                Err(_) => std::thread::sleep(std::time::Duration::from_millis(500)),
            }
        }
        Ok(())
    })
    .await
    .map_err(|e| e.to_string())?
}
