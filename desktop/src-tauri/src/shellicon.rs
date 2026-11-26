//! Windows 桌面、开始菜单和固定任务栏快捷方式的主题图标。

#[derive(Default, serde::Serialize)]
pub struct SyncReport {
    updated: usize,
    warnings: Vec<String>,
    /// 属于临时文件占用（Explorer / 索引器 / OneDrive 锁住）导致的失败数，前端可据此决定是否重试以及能否标为已完成。
    busy: usize,
    generation: u64,
    refresh: bool,
    /// 窗口还没显示、有这么多个快捷方式要改：这次什么都没改，等窗口显示出来后再调用一次（见 windows_shell::sync）
    pending: usize,
}

#[tauri::command]
pub async fn sync_shell_icon(
    webview: tauri::Webview,
    app: tauri::AppHandle,
    theme: Option<String>,
    dark: Option<bool>,
) -> Result<SyncReport, String> {
    crate::main_only(&webview)?;
    let resolved = match theme.as_deref() {
        Some(name) if !name.is_empty() => name.to_string(),
        _ => if dark.unwrap_or(false) { "dark" } else { "light" }.to_string(),
    };
    #[cfg(windows)]
    {
        use tauri::Manager;
        let window = app.get_webview_window("main").ok_or("主窗口不在")?;
        let executable = std::env::current_exe().map_err(|e| e.to_string())?;
        // 不放进启动时会清理的旧版 WebView 缓存，也不依赖可搬动的 Ferry 数据目录。
        let cache = app
            .path()
            .local_data_dir()
            .map_err(|e| e.to_string())?
            .join("Ferry")
            .join("icons")
            .join(&app.config().identifier);
        let identifier = app.config().identifier.clone();
        tauri::async_runtime::spawn_blocking(move || {
            windows_shell::sync(window, &executable, &cache, &identifier, &resolved)
        })
        .await
        .map_err(|e| e.to_string())?
    }
    #[cfg(not(windows))]
    {
        let _ = (app, resolved);
        Ok(SyncReport::default())
    }
}

/// 前端写好窗口图标后调用；返回 false 表示正在进行模态交互，稍后重试。
#[tauri::command]
pub async fn refresh_shell_icon(
    webview: tauri::Webview,
    app: tauri::AppHandle,
    generation: u64,
) -> Result<bool, String> {
    crate::main_only(&webview)?;
    #[cfg(windows)]
    {
        use tauri::Manager;
        let window = app.get_webview_window("main").ok_or("主窗口不在")?;
        tauri::async_runtime::spawn_blocking(move || {
            let (sender, receiver) = std::sync::mpsc::sync_channel(1);
            let target = window.clone();
            window
                .run_on_main_thread(move || {
                    let result = windows_shell::finish(&target, generation);
                    let _ = sender.send(result);
                })
                .map_err(|e| e.to_string())?;
            receiver.recv().map_err(|e| e.to_string())?
        })
        .await
        .map_err(|e| e.to_string())?
    }
    #[cfg(not(windows))]
    {
        let _ = (app, generation);
        Ok(true)
    }
}

#[cfg(windows)]
mod windows_shell {
    use super::SyncReport;
    use std::collections::hash_map::DefaultHasher;
    use std::collections::HashSet;
    use std::ffi::OsString;
    use std::hash::{Hash, Hasher};
    use std::os::windows::ffi::{OsStrExt, OsStringExt};
    use std::path::{Path, PathBuf};
    use std::sync::atomic::{AtomicU64, Ordering};
    use std::time::Duration;
    use windows::core::{Interface, GUID, PCWSTR};
    use windows::Win32::Foundation::{E_HANDLE, HWND, PROPERTYKEY, RPC_E_CHANGED_MODE};
    use windows::Win32::System::Com::StructuredStorage::PROPVARIANT;
    use windows::Win32::System::Com::{
        CoCreateInstance, CoInitializeEx, CoTaskMemFree, CoUninitialize, IPersistFile,
        CLSCTX_INPROC_SERVER, COINIT_APARTMENTTHREADED, STGM_READ, STGM_READWRITE,
    };
    use windows::Win32::System::Variant::VT_LPWSTR;
    use windows::Win32::UI::Input::KeyboardAndMouse::{GetFocus, IsWindowEnabled, SetFocus};
    use windows::Win32::UI::Shell::PropertiesSystem::{
        IPropertyStore, SHGetPropertyStoreForWindow,
    };
    use windows::Win32::UI::Shell::{
        FOLDERID_CommonPrograms, FOLDERID_Desktop, FOLDERID_Programs, FOLDERID_PublicDesktop,
        FOLDERID_RoamingAppData, IShellLinkW, SHChangeNotify, SHGetKnownFolderPath, SHStrDupW,
        SHUpdateImageW, ShellLink, Shell_GetCachedImageIndexW, KF_FLAG_DEFAULT, SHCNE_UPDATEDIR,
        SHCNE_UPDATEITEM, SHCNF_FLUSH, SHCNF_PATHW,
    };
    use windows::Win32::UI::WindowsAndMessaging::{
        GetForegroundWindow, GetGUIThreadInfo, GetWindowThreadProcessId, IsWindow, IsWindowVisible,
        SetForegroundWindow, SetWindowPos, ShowWindow, GUITHREADINFO, GUI_INMENUMODE,
        GUI_INMOVESIZE, GUI_POPUPMENUMODE, GUI_SYSTEMMENUMODE, SWP_HIDEWINDOW, SWP_NOACTIVATE,
        SWP_NOMOVE, SWP_NOSIZE, SWP_NOZORDER, SWP_SHOWWINDOW, SW_SHOWNA,
    };

    const DARK_ICON: &[u8] = include_bytes!("../icons/themes/ferry-dark.ico");
    const LIGHT_ICON: &[u8] = include_bytes!("../icons/themes/ferry-light.ico");
    const WARM_PAPER_ICON: &[u8] = include_bytes!("../icons/themes/ferry-warm-paper.ico");
    const WARM_DUSK_ICON: &[u8] = include_bytes!("../icons/themes/ferry-warm-dusk.ico");
    const DIMMED_SAGE_ICON: &[u8] = include_bytes!("../icons/themes/ferry-dimmed-sage.ico");
    const OBSIDIAN_ICON: &[u8] = include_bytes!("../icons/themes/ferry-obsidian.ico");

    fn icon_asset(theme: &str) -> (&'static str, &'static [u8]) {
        match theme {
            "dark" => ("dark", DARK_ICON),
            "warm-paper" => ("warm-paper", WARM_PAPER_ICON),
            "warm-dusk" => ("warm-dusk", WARM_DUSK_ICON),
            "dimmed-sage" => ("dimmed-sage", DIMMED_SAGE_ICON),
            "obsidian" => ("obsidian", OBSIDIAN_ICON),
            _ => ("light", LIGHT_ICON),
        }
    }
    /// 快速切换时，只刷新最后一次同步对应的主题。
    static SYNCS: AtomicU64 = AtomicU64::new(0);
    const APP_MODEL: GUID = GUID::from_u128(0x9f4c2855_9f79_4b39_a8d0_e1d42de1d5f3);
    const RELAUNCH_COMMAND: PROPERTYKEY = PROPERTYKEY {
        fmtid: APP_MODEL,
        pid: 2,
    };
    const RELAUNCH_ICON: PROPERTYKEY = PROPERTYKEY {
        fmtid: APP_MODEL,
        pid: 3,
    };
    const RELAUNCH_NAME: PROPERTYKEY = PROPERTYKEY {
        fmtid: APP_MODEL,
        pid: 4,
    };
    const APP_ID: PROPERTYKEY = PROPERTYKEY {
        fmtid: APP_MODEL,
        pid: 5,
    };

    struct ComApartment(bool);
    impl ComApartment {
        fn enter() -> windows::core::Result<Self> {
            let result = unsafe { CoInitializeEx(None, COINIT_APARTMENTTHREADED) };
            if result == RPC_E_CHANGED_MODE {
                return Ok(Self(false)); // 已有 COM apartment，不能由这里释放。
            }
            result.ok()?;
            Ok(Self(true))
        }
    }
    impl Drop for ComApartment {
        fn drop(&mut self) {
            if self.0 {
                unsafe { CoUninitialize() };
            }
        }
    }

    pub(super) fn sync(
        window: tauri::WebviewWindow,
        executable: &Path,
        cache: &Path,
        identifier: &str,
        theme: &str,
    ) -> Result<SyncReport, String> {
        let generation = SYNCS.fetch_add(1, Ordering::SeqCst) + 1;
        let _com = ComApartment::enter().map_err(|e| e.to_string())?;
        let icon = export_icon(cache, theme).map_err(|e| e.to_string())?;
        let mut report = SyncReport {
            generation,
            ..SyncReport::default()
        };
        let hwnd = HWND(window.hwnd().map_err(|e| e.to_string())?.0);
        let exec_id = ExecutableIdentity::new(executable);
        let roots = shortcut_roots();
        let visible = unsafe { IsWindowVisible(hwnd) }.as_bool();
        let mut change = true;
        if !visible {
            // 启动时窗口还没显示：先只看有没有要改的快捷方式。有就这次什么都不改，前端等窗口显示出来再调用一次。
            // 任务栏只在「按钮已经在任务栏上、这时快捷方式变了」的情况下才可靠地换图标（平时切主题就是这样）；
            // 窗口显示前就改好的话，任务栏建按钮时用的还是它记着的旧图标，之后一直不变。实测试过晚点显示窗口、
            // 显示后刷新任务栏、显示后再同步一次再刷新，都不稳；窗口显示后再改，8 次全对
            let mut check = SyncReport::default();
            let mut dry_changed = Vec::new();
            for (root, recursive) in &roots {
                visit_shortcuts(
                    root,
                    *recursive,
                    0,
                    &exec_id,
                    &icon,
                    false,
                    &mut dry_changed,
                    &mut check,
                );
            }
            if check.updated > 0 || check.busy > 0 {
                // 扫描期间窗口可能刚好被显示出来（3 秒兜底或单实例唤起）；只有仍然隐藏时才推迟。
                if !unsafe { IsWindowVisible(hwnd) }.as_bool() {
                    report.pending = check.updated.max(check.busy);
                    return Ok(report);
                }
            } else {
                change = false; // 都是对的，不用再扫一遍
            }
        }
        if change {
            // 两阶段提交：先把所有目录的 .lnk 全部写完落盘，再统一向 Shell 发变更通知与图标缓存失效。
            // 以前每写完一个（如桌面）就立刻发 SHUpdateImageW，会唤醒 Explorer 去内存映射尚未改完的开始菜单 .lnk，
            // 既造成 0x800704C8 占用锁，又让任务栏抢先缓存回旧图标。
            let mut changed = Vec::new();
            for (root, recursive) in &roots {
                visit_shortcuts(
                    root,
                    *recursive,
                    0,
                    &exec_id,
                    &icon,
                    true,
                    &mut changed,
                    &mut report,
                );
            }
            if !changed.is_empty() {
                notify_shortcuts(&changed, executable);
            }
        }
        // 先存好快捷方式，再在窗口所属主线程改窗口属性并原子读取最终可见态：
        // 任务栏看到窗口变了会重新取这一组的图标记下来，那时快捷方式还是旧的就记成旧图标。
        match update_window_on_main(
            &window,
            executable.to_path_buf(),
            icon.clone(),
            identifier.to_string(),
        ) {
            Ok(visible_now) => {
                // 窗口显示着：前端等一会儿再让任务栏重新取图标（refresh_shell_icon）
                report.refresh = visible_now;
            }
            Err(error) => {
                report.warnings.push(format!("更新任务栏图标失败：{error}"));
                report.refresh = unsafe { IsWindowVisible(hwnd) }.as_bool();
            }
        }
        Ok(report)
    }

    fn update_window_on_main(
        window: &tauri::WebviewWindow,
        executable: PathBuf,
        icon: PathBuf,
        identifier: String,
    ) -> Result<bool, String> {
        let (sender, receiver) = std::sync::mpsc::sync_channel(1);
        let target = window.clone();
        window
            .run_on_main_thread(move || {
                let result = (|| -> Result<bool, String> {
                    let _com = ComApartment::enter().map_err(|e| e.to_string())?;
                    let hwnd = HWND(target.hwnd().map_err(|e| e.to_string())?.0);
                    update_window(hwnd, &executable, &icon, &identifier)
                        .map_err(|e| e.to_string())?;
                    Ok(unsafe { IsWindowVisible(hwnd) }.as_bool())
                })();
                let _ = sender.send(result);
            })
            .map_err(|e| e.to_string())?;
        receiver.recv().map_err(|e| e.to_string())?
    }

    pub(super) fn finish(window: &tauri::WebviewWindow, generation: u64) -> Result<bool, String> {
        if SYNCS.load(Ordering::SeqCst) != generation {
            return Ok(true);
        }
        let hwnd = window.hwnd().map_err(|e| e.to_string())?;
        refresh_taskbar(HWND(hwnd.0)).map_err(|e| e.to_string())
    }

    fn export_icon(cache: &Path, theme: &str) -> std::io::Result<PathBuf> {
        let (name, bytes) = icon_asset(theme);
        let mut hash = DefaultHasher::new();
        bytes.hash(&mut hash);
        let path = cache.join(format!("ferry-{name}-{:016x}.ico", hash.finish()));
        std::fs::create_dir_all(cache)?;
        // 每套主题和每次设计修订使用不同路径，避免覆盖同一路径后 Windows 继续显示旧缓存。
        if std::fs::read(&path).ok().as_deref() != Some(bytes) {
            std::fs::write(&path, bytes)?;
        }
        Ok(path)
    }

    fn wide(path: &Path) -> Vec<u16> {
        path.as_os_str().encode_wide().chain(Some(0)).collect()
    }
    fn from_wide(buffer: &[u16]) -> PathBuf {
        let end = buffer.iter().position(|&c| c == 0).unwrap_or(buffer.len());
        PathBuf::from(OsString::from_wide(&buffer[..end]))
    }
    fn path_key(path: &Path) -> Option<String> {
        if !path.is_absolute() {
            return None;
        }
        Some(
            path.to_string_lossy()
                .replace('/', "\\")
                .trim_start_matches(r"\\?\")
                .to_lowercase(),
        )
    }

    struct ExecutableIdentity {
        raw_key: Option<String>,
        canonical_key: Option<String>,
        file_name: Option<String>,
    }

    impl ExecutableIdentity {
        fn new(executable: &Path) -> Self {
            let raw_key = path_key(executable);
            let canonical_key = std::fs::canonicalize(executable)
                .ok()
                .and_then(|p| path_key(&p));
            let file_name = executable
                .file_name()
                .map(|n| n.to_string_lossy().to_lowercase());
            Self {
                raw_key,
                canonical_key,
                file_name,
            }
        }

        fn matches(&self, target: &Path) -> bool {
            let Some(target_key) = path_key(target) else {
                return false;
            };
            if self.raw_key.as_deref() == Some(target_key.as_str()) {
                return true;
            }
            if self.canonical_key.as_deref() == Some(target_key.as_str()) {
                return true;
            }
            // 仅在文件名相同时才 canonicalize，避免扫描无关快捷方式触发额外磁盘/网络 I/O。
            let same_name = self.file_name.is_some()
                && target
                    .file_name()
                    .map(|n| n.to_string_lossy().to_lowercase())
                    == self.file_name;
            if !same_name {
                return false;
            }
            let Some(ref canonical) = self.canonical_key else {
                return false;
            };
            std::fs::canonicalize(target)
                .ok()
                .and_then(|p| path_key(&p))
                .is_some_and(|k| k == *canonical)
        }
    }

    fn is_busy(error: &windows::core::Error) -> bool {
        const BUSY: [windows::core::HRESULT; 4] = [
            windows::core::HRESULT(0x8007_04C8_u32 as i32), // ERROR_USER_MAPPED_FILE
            windows::core::HRESULT(0x8007_0020_u32 as i32), // ERROR_SHARING_VIOLATION
            windows::core::HRESULT(0x8003_0020_u32 as i32), // STG_E_SHAREVIOLATION
            windows::core::HRESULT(0x8003_0021_u32 as i32), // STG_E_LOCKVIOLATION
        ];
        BUSY.contains(&error.code())
    }

    fn set_string(
        store: &IPropertyStore,
        key: &PROPERTYKEY,
        text: &str,
    ) -> windows::core::Result<()> {
        let buffer: Vec<u16> = text.encode_utf16().chain(Some(0)).collect();
        let mut value = PROPVARIANT::default();
        unsafe {
            let owned = SHStrDupW(PCWSTR(buffer.as_ptr()))?;
            let inner = &mut *value.Anonymous.Anonymous;
            inner.vt = VT_LPWSTR;
            inner.Anonymous.pwszVal = owned;
            // PROPVARIANT 在 Drop 时释放字符串，必须使用 COM 分配的内存，不能借用 Vec。
            store.SetValue(key, &value)
        }
    }

    fn update_window(
        hwnd: HWND,
        executable: &Path,
        icon: &Path,
        identifier: &str,
    ) -> windows::core::Result<()> {
        let store: IPropertyStore = unsafe { SHGetPropertyStoreForWindow(hwnd)? };
        // 显式的窗口 AppUserModelID 让固定任务栏分组也采用主题图标，新固定的快捷方式继承这些属性。
        // https://learn.microsoft.com/windows/win32/properties/props-system-appusermodel-relaunchiconresource
        set_string(
            &store,
            &RELAUNCH_COMMAND,
            &format!("\"{}\"", executable.display()),
        )?;
        set_string(&store, &RELAUNCH_NAME, "Ferry")?;
        set_string(&store, &RELAUNCH_ICON, &format!("{},0", icon.display()))?;
        set_string(&store, &APP_ID, identifier)
        // 窗口的 property store 立即生效，不调用不受支持的 Commit。
    }

    /// 通过窗口可见性让 Explorer 重新登记原生按钮，保留单击按钮时的窗口操作。
    fn refresh_taskbar(hwnd: HWND) -> windows::core::Result<bool> {
        unsafe {
            if !IsWindow(Some(hwnd)).as_bool() {
                return Err(windows::core::Error::new(E_HANDLE, "窗口已关闭"));
            }
            if !IsWindowVisible(hwnd).as_bool() {
                return Ok(true);
            }
            // 文件选择框会禁用主窗口；菜单、拖动和鼠标捕获结束后再刷新。
            if !IsWindowEnabled(hwnd).as_bool() {
                return Ok(false);
            }
            let mut gui = GUITHREADINFO {
                cbSize: std::mem::size_of::<GUITHREADINFO>() as u32,
                ..Default::default()
            };
            GetGUIThreadInfo(GetWindowThreadProcessId(hwnd, None), &mut gui)?;
            let interactive =
                GUI_INMENUMODE | GUI_INMOVESIZE | GUI_POPUPMENUMODE | GUI_SYSTEMMENUMODE;
            if (gui.flags & interactive).0 != 0 || !gui.hwndCapture.is_invalid() {
                return Ok(false);
            }
            let was_foreground = GetForegroundWindow() == hwnd;
            // 在同一轮窗口消息里恢复可见性，保留最小化、最大化以及窗口位置。
            // 原先位于前台时显示阶段不再加 SWP_NOACTIVATE，避免失去前台后被系统锁住无法切回；
            // 原先不在前台时保持 SWP_NOACTIVATE，不抢占用户正在使用的其他窗口。
            let base = SWP_NOMOVE | SWP_NOSIZE | SWP_NOZORDER;
            SetWindowPos(
                hwnd,
                None,
                0,
                0,
                0,
                0,
                base | SWP_NOACTIVATE | SWP_HIDEWINDOW,
            )?;
            let show_flags = if was_foreground {
                base | SWP_SHOWWINDOW
            } else {
                base | SWP_NOACTIVATE | SWP_SHOWWINDOW
            };
            if let Err(error) = SetWindowPos(hwnd, None, 0, 0, 0, 0, show_flags) {
                // 显示失败也必须恢复窗口可见性，再把失败交给前端重试。
                let _ = ShowWindow(hwnd, SW_SHOWNA);
                return Err(error);
            }
            // 前台与输入焦点尽力恢复，但不把跨进程 WebView2 焦点转发视为任务栏刷新失败，
            // 否则会触发前端整轮 3 次重复重建任务栏。
            if was_foreground && GetForegroundWindow() != hwnd {
                let _ = SetForegroundWindow(hwnd);
            }
            if was_foreground && !gui.hwndFocus.is_invalid() && GetFocus() != gui.hwndFocus {
                let _ = SetFocus(Some(gui.hwndFocus));
            }
        }
        Ok(true)
    }

    fn known_folder(id: &GUID) -> Option<PathBuf> {
        unsafe {
            let value = SHGetKnownFolderPath(id, KF_FLAG_DEFAULT, None).ok()?;
            let path = PathBuf::from(OsString::from_wide(value.as_wide()));
            CoTaskMemFree(Some(value.0.cast()));
            Some(path)
        }
    }
    fn shortcut_roots() -> Vec<(PathBuf, bool)> {
        let mut raw = Vec::new();
        for (id, recursive) in [
            (FOLDERID_Desktop, false),
            (FOLDERID_PublicDesktop, false),
            (FOLDERID_Programs, true),
            (FOLDERID_CommonPrograms, true),
        ] {
            if let Some(path) = known_folder(&id) {
                raw.push((path, recursive));
            }
        }
        if let Some(roaming) = known_folder(&FOLDERID_RoamingAppData) {
            let pinned = roaming.join("Microsoft/Internet Explorer/Quick Launch/User Pinned");
            raw.push((pinned.join("TaskBar"), false));
            // 显式设置了 AppUserModelID + Relaunch* 的窗口，Windows 会在此目录下自动维护隐式快捷方式锚点。
            raw.push((pinned.join("ImplicitAppShortcuts"), true));
            raw.push((pinned.join("StartMenu"), false));
        }
        let mut seen = HashSet::new();
        let mut roots = Vec::with_capacity(raw.len());
        for (path, recursive) in raw {
            if let Some(key) = path_key(&path) {
                if !seen.insert(key) {
                    continue;
                }
            }
            roots.push((path, recursive));
        }
        roots
    }
    #[allow(clippy::too_many_arguments)]
    fn visit_shortcuts(
        root: &Path,
        recursive: bool,
        depth: usize,
        executable: &ExecutableIdentity,
        icon: &Path,
        write: bool,
        changed: &mut Vec<(PathBuf, OldIcon)>,
        report: &mut SyncReport,
    ) {
        let Ok(entries) = std::fs::read_dir(root) else {
            return;
        };
        for entry in entries.flatten() {
            let Ok(kind) = entry.file_type() else {
                continue;
            };
            let path = entry.path();
            if kind.is_dir() && recursive && depth < 12 {
                visit_shortcuts(
                    &path,
                    true,
                    depth + 1,
                    executable,
                    icon,
                    write,
                    changed,
                    report,
                );
            } else if kind.is_file()
                && path
                    .extension()
                    .is_some_and(|e| e.eq_ignore_ascii_case("lnk"))
            {
                match update_shortcut_retrying(&path, executable, icon, write) {
                    Ok(Some(old)) => {
                        // write 为 false 时只是数一数有几个要改
                        report.updated += 1;
                        if write {
                            changed.push((path, old));
                        }
                    }
                    Ok(None) => {}
                    Err(error) => {
                        if is_busy(&error) {
                            report.busy += 1;
                        }
                        report
                            .warnings
                            .push(format!("更新 {} 失败：{error}", path.display()));
                    }
                }
            }
        }
    }

    /// 资源管理器、搜索索引或 OneDrive 同步正在读取或映射快捷方式时会返回临时占用错误
    ///（0x800704C8 / 0x80070020 / 0x80030020 / 0x80030021）。稍等再试几次，避免漏改单个快捷方式。
    fn update_shortcut_retrying(
        path: &Path,
        executable: &ExecutableIdentity,
        icon: &Path,
        write: bool,
    ) -> windows::core::Result<Option<OldIcon>> {
        let mut attempt = 0;
        loop {
            match update_shortcut(path, executable, icon, write) {
                Err(error) if attempt < 8 && is_busy(&error) => {
                    attempt += 1;
                    std::thread::sleep(Duration::from_millis(60 * attempt));
                }
                result => return result,
            }
        }
    }

    /// 快捷方式改之前的图标：通知 Shell 时要给旧的（见 notify_shortcuts）
    struct OldIcon {
        file: Vec<u16>,
        index: i32,
    }

    fn same_relaunch_icon(current: &str, icon: &Path) -> bool {
        let Some(path_part) = current.trim().strip_suffix(",0") else {
            return false;
        };
        path_key(Path::new(path_part)) == path_key(icon)
    }

    /// 快捷方式现在的图标和 RelaunchIconResource 是否都已经是 icon
    fn has_icon(link: &IShellLinkW, icon: &Path) -> windows::core::Result<(bool, OldIcon)> {
        let mut file = vec![0u16; 32768];
        let mut index = 0;
        unsafe {
            link.GetIconLocation(&mut file, &mut index)?;
        }
        let icon_same = index == 0 && path_key(&from_wide(&file)) == path_key(icon);
        let relaunch_same = if icon_same {
            let properties: IPropertyStore = link.cast()?;
            let current = unsafe { properties.GetValue(&RELAUNCH_ICON) }
                .ok()
                .map(|v| v.to_string())
                .unwrap_or_default();
            same_relaunch_icon(&current, icon)
        } else {
            false
        };
        Ok((icon_same && relaunch_same, OldIcon { file, index }))
    }

    /// 把指向本程序的快捷方式换成 icon；改了返回改之前的图标，不用改（不是本程序的、已经是这个图标）返回 None。
    /// write 为 false 时只看要不要改，不动文件：要改时也返回现在的图标
    fn update_shortcut(
        path: &Path,
        executable: &ExecutableIdentity,
        icon: &Path,
        write: bool,
    ) -> windows::core::Result<Option<OldIcon>> {
        let link: IShellLinkW =
            unsafe { CoCreateInstance(&ShellLink, None, CLSCTX_INPROC_SERVER)? };
        let persist: IPersistFile = link.cast()?;
        let file = wide(path);
        // 不 Resolve：避免扫描无关快捷方式时访问网络或弹出寻找目标的提示。
        // 注意：只跳过损坏/无权限的无关快捷方式，临时文件占用（BUSY）必须往上抛以触发重试，不能静默漏改。
        if let Err(error) = unsafe { persist.Load(PCWSTR(file.as_ptr()), STGM_READ) } {
            if is_busy(&error) {
                return Err(error);
            }
            return Ok(None);
        }
        let mut target = vec![0u16; 32768];
        if unsafe { link.GetPath(&mut target, std::ptr::null_mut(), 0) }.is_err() {
            return Ok(None);
        }
        // 必须指向当前安装的完整 exe 路径；其他安装、同名快捷方式和其他程序不改动。
        if !executable.matches(&from_wide(&target)) {
            return Ok(None);
        }
        if !write {
            let (same, old) = has_icon(&link, icon)?;
            return Ok(if same { None } else { Some(old) });
        }
        // 只对确认属于当前程序的链接请求写权限，并在重新加载后复核目标。
        unsafe {
            persist.Load(PCWSTR(file.as_ptr()), STGM_READWRITE)?;
            link.GetPath(&mut target, std::ptr::null_mut(), 0)?;
        }
        if !executable.matches(&from_wide(&target)) {
            return Ok(None);
        }
        let (same, old) = has_icon(&link, icon)?;
        if same {
            return Ok(None);
        }
        let icon_path = wide(icon);
        unsafe {
            link.SetIconLocation(PCWSTR(icon_path.as_ptr()), 0)?;
        }
        let properties: IPropertyStore = link.cast()?;
        set_string(
            &properties,
            &RELAUNCH_ICON,
            &format!("{},0", icon.display()),
        )?;
        unsafe {
            properties.Commit()?;
            persist.Save(PCWSTR(file.as_ptr()), true)?;
        }
        Ok(Some(old))
    }

    /// 所有快捷方式都保存落盘后，再统一通知 Shell 刷新：
    /// 1. 逐个发 SHCNE_UPDATEITEM 刷新快捷方式项；
    /// 2. 对改动过的父目录各发一次 SHCNE_UPDATEDIR，促使开始菜单 AppResolver / 桌面目录监听及时重扫；
    /// 3. 对每个去重后的旧图标（NSIS 新装快捷方式旧图标为空时回退为可执行文件自身图标）发一次 SHUpdateImageW。
    fn notify_shortcuts(changed: &[(PathBuf, OldIcon)], executable: &Path) {
        let mut notified_dirs = HashSet::new();
        let mut notified_icons = HashSet::new();
        for (path, _) in changed {
            let file = wide(path);
            unsafe {
                SHChangeNotify(
                    SHCNE_UPDATEITEM,
                    SHCNF_PATHW | SHCNF_FLUSH,
                    Some(file.as_ptr().cast()),
                    None,
                );
            }
        }
        for (path, _) in changed {
            let Some(parent) = path.parent() else {
                continue;
            };
            let Some(key) = path_key(parent) else {
                continue;
            };
            if !notified_dirs.insert(key) {
                continue;
            }
            let dir = wide(parent);
            unsafe {
                SHChangeNotify(
                    SHCNE_UPDATEDIR,
                    SHCNF_PATHW | SHCNF_FLUSH,
                    Some(dir.as_ptr().cast()),
                    None,
                );
            }
        }
        for (_, old) in changed {
            let (old_path, old_index) = resolve_old_icon(old, executable);
            let Some(key) = path_key(&old_path) else {
                continue;
            };
            if !notified_icons.insert((key, old_index)) {
                continue;
            }
            let icon_wide = wide(&old_path);
            unsafe {
                // 没缓存过（返回 -1）就没有要丢的
                let image = Shell_GetCachedImageIndexW(PCWSTR(icon_wide.as_ptr()), old_index, 0);
                if image >= 0 {
                    SHUpdateImageW(PCWSTR(icon_wide.as_ptr()), old_index, 0, image);
                }
            }
        }
    }

    fn resolve_old_icon(old: &OldIcon, executable: &Path) -> (PathBuf, i32) {
        let path = from_wide(&old.file);
        if path.as_os_str().is_empty() {
            (executable.to_path_buf(), 0)
        } else {
            (path, old.index)
        }
    }

    #[cfg(test)]
    mod tests {
        use super::*;
        use std::sync::atomic::{AtomicU64, Ordering};
        use windows::core::w;
        use windows::Win32::Foundation::RECT;
        use windows::Win32::UI::Input::KeyboardAndMouse::EnableWindow;
        use windows::Win32::UI::WindowsAndMessaging::{
            CreateWindowExW, DestroyWindow, GetWindowRect, IsIconic, IsZoomed, ShowWindow,
            SW_SHOWNA, WINDOW_EX_STYLE, WINDOW_STYLE, WS_CHILD, WS_MAXIMIZE, WS_MINIMIZE,
            WS_OVERLAPPEDWINDOW, WS_VISIBLE,
        };

        static NEXT_TEST: AtomicU64 = AtomicU64::new(0);
        struct TestDirectory {
            base: PathBuf,
            root: PathBuf,
        }
        impl TestDirectory {
            fn new() -> Self {
                let base = std::env::temp_dir().join("ferry-shell-icon-tests");
                let root = base.join(format!(
                    "{}-{}-图标测试",
                    std::process::id(),
                    NEXT_TEST.fetch_add(1, Ordering::Relaxed)
                ));
                std::fs::create_dir_all(&root).unwrap();
                Self { base, root }
            }
            fn executable(&self, name: &str) -> PathBuf {
                let path = self.root.join(name).join("ferry-desktop.exe");
                std::fs::create_dir_all(path.parent().unwrap()).unwrap();
                std::fs::write(&path, b"test executable placeholder").unwrap();
                path
            }
        }
        impl Drop for TestDirectory {
            fn drop(&mut self) {
                let root = self.root.canonicalize().unwrap();
                let base = self.base.canonicalize().unwrap();
                assert_eq!(root.parent(), Some(base.as_path()));
                let _ = std::fs::remove_dir_all(&root);
            }
        }
        fn create_link(path: &Path, target: &Path) {
            std::fs::create_dir_all(path.parent().unwrap()).unwrap();
            let link: IShellLinkW =
                unsafe { CoCreateInstance(&ShellLink, None, CLSCTX_INPROC_SERVER).unwrap() };
            unsafe {
                link.SetPath(PCWSTR(wide(target).as_ptr())).unwrap();
                link.SetArguments(w!("--example \"keep me\"")).unwrap();
                link.SetWorkingDirectory(PCWSTR(wide(target.parent().unwrap()).as_ptr()))
                    .unwrap();
                link.SetDescription(w!("custom shortcut name")).unwrap();
                let properties: IPropertyStore = link.cast().unwrap();
                set_string(&properties, &APP_ID, "ferry.test.identity").unwrap();
                properties.Commit().unwrap();
                let persist: IPersistFile = link.cast().unwrap();
                persist.Save(PCWSTR(wide(path).as_ptr()), true).unwrap();
            }
        }
        fn load_link(path: &Path) -> IShellLinkW {
            let link: IShellLinkW =
                unsafe { CoCreateInstance(&ShellLink, None, CLSCTX_INPROC_SERVER).unwrap() };
            let persist: IPersistFile = link.cast().unwrap();
            unsafe {
                persist
                    .Load(PCWSTR(wide(path).as_ptr()), STGM_READ)
                    .unwrap();
            }
            link
        }
        fn property(store: &IPropertyStore, key: &PROPERTYKEY) -> String {
            unsafe { store.GetValue(key).unwrap().to_string() }
        }

        struct TestWindow(HWND);
        impl TestWindow {
            fn new(style: WINDOW_STYLE) -> Self {
                Self(unsafe {
                    CreateWindowExW(
                        WINDOW_EX_STYLE(0),
                        w!("STATIC"),
                        w!("Ferry icon test"),
                        WS_OVERLAPPEDWINDOW | style,
                        0,
                        0,
                        100,
                        100,
                        None,
                        None,
                        None,
                        None,
                    )
                    .unwrap()
                })
            }
        }
        impl Drop for TestWindow {
            fn drop(&mut self) {
                let _ = unsafe { DestroyWindow(self.0) };
            }
        }

        #[test]
        fn theme_round_trip_preserves_shortcut_launch_details() {
            let _com = ComApartment::enter().unwrap();
            let dir = TestDirectory::new();
            let exe = dir.executable("current install");
            let exec_id = ExecutableIdentity::new(&exe);
            let shortcut = dir.root.join("Desktop/renamed deploy tool.lnk");
            create_link(&shortcut, &exe);
            let cache = dir.root.join("cache");
            let light = export_icon(&cache, "light").unwrap();
            let dark = export_icon(&cache, "dark").unwrap();
            let warm_paper = export_icon(&cache, "warm-paper").unwrap();
            let warm_dusk = export_icon(&cache, "warm-dusk").unwrap();
            let dimmed_sage = export_icon(&cache, "dimmed-sage").unwrap();
            let obsidian = export_icon(&cache, "obsidian").unwrap();
            let all_paths: HashSet<_> = [&light, &dark, &warm_paper, &warm_dusk, &dimmed_sage, &obsidian]
                .into_iter()
                .collect();
            assert_eq!(all_paths.len(), 6);
            assert_eq!(std::fs::read(&light).unwrap(), LIGHT_ICON);
            assert_eq!(std::fs::read(&dark).unwrap(), DARK_ICON);
            assert_eq!(std::fs::read(&warm_paper).unwrap(), WARM_PAPER_ICON);
            assert_eq!(std::fs::read(&warm_dusk).unwrap(), WARM_DUSK_ICON);
            assert_eq!(std::fs::read(&dimmed_sage).unwrap(), DIMMED_SAGE_ICON);
            assert_eq!(std::fs::read(&obsidian).unwrap(), OBSIDIAN_ICON);
            for icon in [&light, &dark, &warm_dusk, &light] {
                // 只检查：说要改，但文件不动
                let before = std::fs::read(&shortcut).unwrap();
                assert!(update_shortcut(&shortcut, &exec_id, icon, false)
                    .unwrap()
                    .is_some());
                assert_eq!(std::fs::read(&shortcut).unwrap(), before);
                assert!(update_shortcut(&shortcut, &exec_id, icon, true)
                    .unwrap()
                    .is_some());
                let link = load_link(&shortcut);
                let mut buffer = vec![0u16; 32768];
                let mut index = -1;
                unsafe {
                    link.GetIconLocation(&mut buffer, &mut index).unwrap();
                    assert_eq!(from_wide(&buffer), *icon);
                    assert_eq!(index, 0);
                    link.GetArguments(&mut buffer).unwrap();
                    assert_eq!(
                        from_wide(&buffer).to_string_lossy(),
                        "--example \"keep me\""
                    );
                    link.GetWorkingDirectory(&mut buffer).unwrap();
                    assert_eq!(from_wide(&buffer), exe.parent().unwrap());
                    link.GetPath(&mut buffer, std::ptr::null_mut(), 0).unwrap();
                    assert_eq!(path_key(&from_wide(&buffer)), path_key(&exe));
                }
                let store: IPropertyStore = link.cast().unwrap();
                assert_eq!(property(&store, &APP_ID), "ferry.test.identity");
                assert_eq!(
                    property(&store, &RELAUNCH_ICON),
                    format!("{},0", icon.display())
                );
                assert!(update_shortcut(&shortcut, &exec_id, icon, false)
                    .unwrap()
                    .is_none());
                assert!(update_shortcut(&shortcut, &exec_id, icon, true)
                    .unwrap()
                    .is_none());
            }
            assert_eq!(export_icon(&cache, "light").unwrap(), light);
        }

        #[test]
        fn scan_updates_desktop_menu_pins_and_implicit_only_for_exact_executable() {
            let _com = ComApartment::enter().unwrap();
            let dir = TestDirectory::new();
            let exe = dir.executable("current");
            let exec_id = ExecutableIdentity::new(&exe);
            let other_exe = dir.executable("different install");
            let desktop = dir.root.join("Desktop");
            let programs = dir.root.join("Programs");
            let pins = dir.root.join("TaskBar");
            let implicit = dir.root.join("ImplicitAppShortcuts");
            let own = [
                desktop.join("renamed.lnk"),
                programs.join("Ferry/subfolder/Ferry.lnk"),
                pins.join("Ferry.lnk"),
                implicit.join("a1b2c3d4/Ferry.lnk"),
            ];
            for path in &own {
                create_link(path, &exe);
            }
            let foreign = desktop.join("Ferry.lnk");
            create_link(&foreign, &other_exe);
            let original = std::fs::read(&foreign).unwrap();
            std::fs::write(desktop.join("broken.lnk"), b"not a shell link").unwrap();
            let icon = export_icon(&dir.root.join("cache"), "dark").unwrap();
            let roots = [
                (desktop, false),
                (programs, true),
                (pins, false),
                (implicit, true),
            ];
            // 只检查：数出 4 个要改，一个文件都不动
            let before: Vec<_> = own.iter().map(|p| std::fs::read(p).unwrap()).collect();
            let mut check = SyncReport::default();
            let mut dry_changed = Vec::new();
            for (root, recursive) in &roots {
                visit_shortcuts(
                    root,
                    *recursive,
                    0,
                    &exec_id,
                    &icon,
                    false,
                    &mut dry_changed,
                    &mut check,
                );
            }
            assert_eq!(check.updated, 4);
            assert_eq!(check.busy, 0);
            assert!(dry_changed.is_empty());
            assert!(check.warnings.is_empty(), "{:?}", check.warnings);
            let after: Vec<_> = own.iter().map(|p| std::fs::read(p).unwrap()).collect();
            assert_eq!(after, before);
            let mut report = SyncReport::default();
            let mut changed = Vec::new();
            for (root, recursive) in &roots {
                visit_shortcuts(
                    root,
                    *recursive,
                    0,
                    &exec_id,
                    &icon,
                    true,
                    &mut changed,
                    &mut report,
                );
            }
            assert_eq!(report.updated, 4);
            assert_eq!(report.busy, 0);
            assert_eq!(changed.len(), 4);
            assert!(report.warnings.is_empty(), "{:?}", report.warnings);
            notify_shortcuts(&changed, &exe);
            assert_eq!(std::fs::read(foreign).unwrap(), original);
            for path in own {
                let store: IPropertyStore = load_link(&path).cast().unwrap();
                assert_eq!(
                    property(&store, &RELAUNCH_ICON),
                    format!("{},0", icon.display())
                );
            }
        }

        #[test]
        fn read_lock_busy_error_is_retried_and_reported_as_busy() {
            use std::os::windows::fs::OpenOptionsExt;

            let _com = ComApartment::enter().unwrap();
            let dir = TestDirectory::new();
            let exe = dir.executable("current");
            let exec_id = ExecutableIdentity::new(&exe);
            let desktop = dir.root.join("Desktop");
            let shortcut = desktop.join("Ferry.lnk");
            create_link(&shortcut, &exe);
            let icon = export_icon(&dir.root.join("cache"), "dark").unwrap();

            // 独占锁模拟 OneDrive / Explorer / SearchIndexer 在 STGM_READ 阶段占用快捷方式：
            // 必须返回 BUSY 错误并计入 busy，而不能静默当成无关快捷方式返回 Ok(None)。
            let exclusive = std::fs::OpenOptions::new()
                .read(true)
                .write(true)
                .share_mode(0)
                .open(&shortcut)
                .unwrap();
            let err = update_shortcut(&shortcut, &exec_id, &icon, false)
                .err()
                .expect("独占占用时 STGM_READ 不得静默跳过");
            assert!(is_busy(&err), "应识别为临时占用错误，实际：{err:?}");

            // 在重试窗口内释放锁，update_shortcut_retrying 应自动恢复成功。
            let releaser = std::thread::spawn(move || {
                std::thread::sleep(Duration::from_millis(120));
                drop(exclusive);
            });
            let updated = update_shortcut_retrying(&shortcut, &exec_id, &icon, true).unwrap();
            releaser.join().unwrap();
            assert!(updated.is_some());
            let store: IPropertyStore = load_link(&shortcut).cast().unwrap();
            assert_eq!(
                property(&store, &RELAUNCH_ICON),
                format!("{},0", icon.display())
            );
        }

        #[test]
        fn repairs_shortcut_when_icon_location_matches_but_relaunch_icon_differs() {
            let _com = ComApartment::enter().unwrap();
            let dir = TestDirectory::new();
            let exe = dir.executable("current");
            let exec_id = ExecutableIdentity::new(&exe);
            let shortcut = dir.root.join("Desktop/Ferry.lnk");
            create_link(&shortcut, &exe);
            let cache = dir.root.join("cache");
            let light = export_icon(&cache, "light").unwrap();
            let dark = export_icon(&cache, "dark").unwrap();

            // 只把 IconLocation 设成 dark，RelaunchIconResource 留在旧值 light：应判定为未对齐并修复。
            let link: IShellLinkW =
                unsafe { CoCreateInstance(&ShellLink, None, CLSCTX_INPROC_SERVER).unwrap() };
            let persist: IPersistFile = link.cast().unwrap();
            unsafe {
                persist
                    .Load(PCWSTR(wide(&shortcut).as_ptr()), STGM_READWRITE)
                    .unwrap();
                link.SetIconLocation(PCWSTR(wide(&dark).as_ptr()), 0)
                    .unwrap();
                let store: IPropertyStore = link.cast().unwrap();
                set_string(&store, &RELAUNCH_ICON, &format!("{},0", light.display())).unwrap();
                store.Commit().unwrap();
                persist
                    .Save(PCWSTR(wide(&shortcut).as_ptr()), true)
                    .unwrap();
            }

            assert!(update_shortcut(&shortcut, &exec_id, &dark, false)
                .unwrap()
                .is_some());
            assert!(update_shortcut(&shortcut, &exec_id, &dark, true)
                .unwrap()
                .is_some());
            let store: IPropertyStore = load_link(&shortcut).cast().unwrap();
            assert_eq!(
                property(&store, &RELAUNCH_ICON),
                format!("{},0", dark.display())
            );
            assert!(update_shortcut(&shortcut, &exec_id, &dark, false)
                .unwrap()
                .is_none());
        }

        #[test]
        fn resolve_old_icon_falls_back_to_executable_when_empty() {
            let exe = PathBuf::from(r"C:\Apps\Ferry\ferry-desktop.exe");
            let empty = OldIcon {
                file: vec![0u16; 16],
                index: 0,
            };
            assert_eq!(resolve_old_icon(&empty, &exe), (exe.clone(), 0));

            let custom_path = PathBuf::from(r"C:\Icons\ferry-light.ico");
            let custom = OldIcon {
                file: wide(&custom_path),
                index: 2,
            };
            assert_eq!(resolve_old_icon(&custom, &exe), (custom_path, 2));
        }

        #[test]
        fn shortcut_roots_includes_implicit_app_shortcuts_without_duplicates() {
            let roots = shortcut_roots();
            let keys: Vec<_> = roots.iter().filter_map(|(p, _)| path_key(p)).collect();
            let unique: HashSet<_> = keys.iter().collect();
            assert_eq!(keys.len(), unique.len());
            assert!(roots.iter().any(|(p, recursive)| {
                *recursive
                    && p.to_string_lossy()
                        .to_ascii_lowercase()
                        .ends_with("user pinned\\implicitappshortcuts")
            }));
        }

        #[test]
        fn executable_identity_matches_normalized_paths_and_rejects_other_installs() {
            let dir = TestDirectory::new();
            let exe = dir.executable("current");
            let other = dir.executable("other");
            let id = ExecutableIdentity::new(&exe);
            let upper_slash =
                PathBuf::from(exe.to_string_lossy().replace('\\', "/").to_uppercase());
            let verbatim = PathBuf::from(format!(r"\\?\{}", exe.display()));
            assert!(id.matches(&exe));
            assert!(id.matches(&upper_slash));
            assert!(id.matches(&verbatim));
            assert!(!id.matches(&other));
            assert!(!id.matches(Path::new("relative/ferry-desktop.exe")));
        }

        #[test]
        fn taskbar_window_properties_follow_both_themes() {
            let _com = ComApartment::enter().unwrap();
            let dir = TestDirectory::new();
            let exe = dir.executable("current");
            let window = TestWindow::new(WINDOW_STYLE(0));
            for theme in ["light", "dark", "warm-paper", "warm-dusk", "dimmed-sage", "obsidian", "light"] {
                let icon = export_icon(&dir.root.join("cache"), theme).unwrap();
                update_window(window.0, &exe, &icon, "ferry.test.identity").unwrap();
                refresh_taskbar(window.0).unwrap();
                assert!(!unsafe { IsWindowVisible(window.0) }.as_bool());
                let store: IPropertyStore =
                    unsafe { SHGetPropertyStoreForWindow(window.0).unwrap() };
                assert_eq!(property(&store, &APP_ID), "ferry.test.identity");
                assert_eq!(property(&store, &RELAUNCH_NAME), "Ferry");
                assert_eq!(
                    property(&store, &RELAUNCH_COMMAND),
                    format!("\"{}\"", exe.display())
                );
                assert_eq!(
                    property(&store, &RELAUNCH_ICON),
                    format!("{},0", icon.display())
                );
            }
        }

        #[test]
        fn refreshing_taskbar_preserves_window_placement() {
            for (style, minimized, maximized) in [
                (WINDOW_STYLE(0), false, false),
                (WS_MINIMIZE, true, false),
                (WS_MAXIMIZE, false, true),
            ] {
                let window = TestWindow::new(style);
                unsafe {
                    let _ = ShowWindow(window.0, SW_SHOWNA);
                    assert_eq!(IsIconic(window.0).as_bool(), minimized);
                    assert_eq!(IsZoomed(window.0).as_bool(), maximized);
                    let mut before = RECT::default();
                    GetWindowRect(window.0, &mut before).unwrap();

                    refresh_taskbar(window.0).unwrap();

                    let mut after = RECT::default();
                    GetWindowRect(window.0, &mut after).unwrap();
                    assert_eq!(after, before);
                    assert!(IsWindowVisible(window.0).as_bool());
                    assert_eq!(IsIconic(window.0).as_bool(), minimized);
                    assert_eq!(IsZoomed(window.0).as_bool(), maximized);
                    assert_ne!(GetForegroundWindow(), window.0);
                }
            }
        }

        #[test]
        fn taskbar_refresh_waits_for_modal_dialog_and_reports_closed_window() {
            let window = TestWindow::new(WINDOW_STYLE(0));
            unsafe {
                let _ = ShowWindow(window.0, SW_SHOWNA);
                let _ = EnableWindow(window.0, false);
                assert!(!refresh_taskbar(window.0).unwrap());
                assert!(IsWindowVisible(window.0).as_bool());
                assert!(!IsWindowEnabled(window.0).as_bool());
                let _ = EnableWindow(window.0, true);
                assert!(refresh_taskbar(window.0).unwrap());
            }
            let handle = window.0;
            drop(window);
            assert!(refresh_taskbar(handle).is_err());
        }

        #[test]
        fn taskbar_refresh_preserves_focused_input() {
            let window = TestWindow::new(WINDOW_STYLE(0));
            unsafe {
                let input = CreateWindowExW(
                    WINDOW_EX_STYLE(0),
                    w!("EDIT"),
                    w!(""),
                    WS_CHILD | WS_VISIBLE,
                    0,
                    0,
                    80,
                    20,
                    Some(window.0),
                    None,
                    None,
                    None,
                )
                .unwrap();
                let _ = ShowWindow(window.0, SW_SHOWNA);
                let _ = SetFocus(Some(input));
                assert_eq!(GetFocus(), input);
                assert!(refresh_taskbar(window.0).unwrap());
                assert_eq!(GetFocus(), input);
            }
        }
    }
}
