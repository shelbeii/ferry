//! Windows 桌面、开始菜单和固定任务栏快捷方式的主题图标。

#[derive(Default, serde::Serialize)]
pub struct SyncReport {
    updated: usize,
    warnings: Vec<String>,
    generation: u64,
    refresh: bool,
    /// 窗口还没显示、有这么多个快捷方式要改：这次什么都没改，等窗口显示出来后再调用一次（见 windows_shell::sync）
    pending: usize,
}

#[tauri::command]
pub async fn sync_shell_icon(
    webview: tauri::Webview,
    app: tauri::AppHandle,
    dark: bool,
) -> Result<SyncReport, String> {
    crate::main_only(&webview)?;
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
            windows_shell::sync(window, &executable, &cache, &identifier, dark)
        })
        .await
        .map_err(|e| e.to_string())?
    }
    #[cfg(not(windows))]
    {
        let _ = (app, dark);
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
    use std::ffi::OsString;
    use std::hash::{Hash, Hasher};
    use std::os::windows::ffi::{OsStrExt, OsStringExt};
    use std::path::{Path, PathBuf};
    use std::sync::atomic::{AtomicU64, Ordering};
    use std::time::Duration;
    use windows::core::{Interface, GUID, PCWSTR};
    use windows::Win32::Foundation::{E_FAIL, E_HANDLE, HWND, PROPERTYKEY, RPC_E_CHANGED_MODE};
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
        SHUpdateImageW, ShellLink, Shell_GetCachedImageIndexW, KF_FLAG_DEFAULT, SHCNE_UPDATEITEM,
        SHCNF_FLUSH, SHCNF_PATHW,
    };
    use windows::Win32::UI::WindowsAndMessaging::{
        GetForegroundWindow, GetGUIThreadInfo, GetWindowThreadProcessId, IsWindow, IsWindowVisible,
        SetForegroundWindow, SetWindowPos, ShowWindow, GUITHREADINFO, GUI_INMENUMODE,
        GUI_INMOVESIZE, GUI_POPUPMENUMODE, GUI_SYSTEMMENUMODE, SWP_HIDEWINDOW, SWP_NOACTIVATE,
        SWP_NOMOVE, SWP_NOSIZE, SWP_NOZORDER, SWP_SHOWWINDOW, SW_SHOWNA,
    };

    const DARK_ICON: &[u8] = include_bytes!("../icons/themes/ferry-dark.ico");
    const LIGHT_ICON: &[u8] = include_bytes!("../icons/themes/ferry-light.ico");
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
        dark: bool,
    ) -> Result<SyncReport, String> {
        let generation = SYNCS.fetch_add(1, Ordering::SeqCst) + 1;
        let _com = ComApartment::enter().map_err(|e| e.to_string())?;
        let icon = export_icon(cache, dark).map_err(|e| e.to_string())?;
        let mut report = SyncReport {
            generation,
            ..SyncReport::default()
        };
        let hwnd = HWND(window.hwnd().map_err(|e| e.to_string())?.0);
        let visible = unsafe { IsWindowVisible(hwnd) }.as_bool();
        let mut change = true;
        if !visible {
            // 启动时窗口还没显示：先只看有没有要改的快捷方式。有就这次什么都不改，前端等窗口显示出来再调用一次。
            // 任务栏只在「按钮已经在任务栏上、这时快捷方式变了」的情况下才可靠地换图标（平时切主题就是这样）；
            // 窗口显示前就改好的话，任务栏建按钮时用的还是它记着的旧图标，之后一直不变。实测试过晚点显示窗口、
            // 显示后刷新任务栏、显示后再同步一次再刷新，都不稳；窗口显示后再改，8 次全对
            let mut check = SyncReport::default();
            for (root, recursive) in shortcut_roots() {
                visit_shortcuts(&root, recursive, 0, executable, &icon, false, &mut check);
            }
            if check.updated > 0 {
                report.pending = check.updated;
                return Ok(report);
            }
            change = false; // 都是对的，不用再扫一遍
        }
        if change {
            for (root, recursive) in shortcut_roots() {
                visit_shortcuts(&root, recursive, 0, executable, &icon, true, &mut report);
            }
        }
        // 先存好快捷方式，再改窗口属性（前端也是先同步这里、再换窗口图标）：任务栏看到窗口变了会重新取
        // 这一组的图标记下来，那时快捷方式还是旧的就记成旧图标，之后重建按钮也用这个旧的
        if let Err(error) = update_window(hwnd, executable, &icon, identifier) {
            report.warnings.push(format!("更新任务栏图标失败：{error}"));
        }
        // 窗口显示着：前端等一会儿再让任务栏重新取图标（refresh_shell_icon）
        report.refresh = visible;
        Ok(report)
    }

    pub(super) fn finish(window: &tauri::WebviewWindow, generation: u64) -> Result<bool, String> {
        if SYNCS.load(Ordering::SeqCst) != generation {
            return Ok(true);
        }
        let hwnd = window.hwnd().map_err(|e| e.to_string())?;
        refresh_taskbar(HWND(hwnd.0)).map_err(|e| e.to_string())
    }

    fn export_icon(cache: &Path, dark: bool) -> std::io::Result<PathBuf> {
        let bytes = if dark { DARK_ICON } else { LIGHT_ICON };
        let mut hash = DefaultHasher::new();
        bytes.hash(&mut hash);
        let path = cache.join(format!(
            "ferry-{}-{:016x}.ico",
            if dark { "dark" } else { "light" },
            hash.finish()
        ));
        std::fs::create_dir_all(cache)?;
        // 两种主题和每次设计修订使用不同路径，避免覆盖同一路径后 Windows 继续显示旧缓存。
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
            let flags = SWP_NOACTIVATE | SWP_NOMOVE | SWP_NOSIZE | SWP_NOZORDER;
            SetWindowPos(hwnd, None, 0, 0, 0, 0, flags | SWP_HIDEWINDOW)?;
            if let Err(error) = SetWindowPos(hwnd, None, 0, 0, 0, 0, flags | SWP_SHOWWINDOW) {
                // 显示失败也必须恢复窗口可见性，再把失败交给前端重试。
                let _ = ShowWindow(hwnd, SW_SHOWNA);
                return Err(error);
            }
            if was_foreground && GetForegroundWindow() != hwnd {
                if !SetForegroundWindow(hwnd).as_bool() {
                    return Err(windows::core::Error::new(E_FAIL, "无法恢复窗口前台状态"));
                }
            }
            if was_foreground && !gui.hwndFocus.is_invalid() && GetFocus() != gui.hwndFocus {
                let _ = SetFocus(Some(gui.hwndFocus));
                if GetFocus() != gui.hwndFocus {
                    return Err(windows::core::Error::new(E_FAIL, "无法恢复窗口输入焦点"));
                }
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
        let mut roots = Vec::new();
        for (id, recursive) in [
            (FOLDERID_Desktop, false),
            (FOLDERID_PublicDesktop, false),
            (FOLDERID_Programs, true),
            (FOLDERID_CommonPrograms, true),
        ] {
            if let Some(path) = known_folder(&id) {
                roots.push((path, recursive));
            }
        }
        if let Some(roaming) = known_folder(&FOLDERID_RoamingAppData) {
            roots.push((
                roaming.join("Microsoft/Internet Explorer/Quick Launch/User Pinned/TaskBar"),
                false,
            ));
        }
        roots
    }
    fn visit_shortcuts(
        root: &Path,
        recursive: bool,
        depth: usize,
        executable: &Path,
        icon: &Path,
        write: bool,
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
                visit_shortcuts(&path, true, depth + 1, executable, icon, write, report);
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
                            refresh_shortcut(&path, &old);
                        }
                    }
                    Ok(None) => {}
                    Err(error) => report
                        .warnings
                        .push(format!("更新 {} 失败：{error}", path.display())),
                }
            }
        }
    }

    /// 资源管理器、搜索索引刚收到改动通知时会读这个快捷方式（映射到内存），这时写入会失败：
    /// 0x800704C8（文件被用户映射区域打开）或 0x80070020（被另一个进程使用）。
    /// 实测连着切主题时会碰上，失败后开始菜单快捷方式停在旧图标，任务栏合并按钮跟着是旧的。稍等再试几次
    fn update_shortcut_retrying(
        path: &Path,
        executable: &Path,
        icon: &Path,
        write: bool,
    ) -> windows::core::Result<Option<OldIcon>> {
        const BUSY: [windows::core::HRESULT; 2] = [
            windows::core::HRESULT(0x8007_04C8_u32 as i32),
            windows::core::HRESULT(0x8007_0020_u32 as i32),
        ];
        let mut attempt = 0;
        loop {
            match update_shortcut(path, executable, icon, write) {
                Err(error) if attempt < 8 && BUSY.contains(&error.code()) => {
                    attempt += 1;
                    std::thread::sleep(Duration::from_millis(60 * attempt));
                }
                result => return result,
            }
        }
    }

    /// 快捷方式改之前的图标：通知 Shell 时要给旧的（见 refresh_shortcut）
    struct OldIcon {
        file: Vec<u16>,
        index: i32,
    }

    /// 快捷方式现在的图标是不是 icon
    fn has_icon(link: &IShellLinkW, icon: &Path) -> windows::core::Result<(bool, OldIcon)> {
        let mut file = vec![0u16; 32768];
        let mut index = 0;
        unsafe {
            link.GetIconLocation(&mut file, &mut index)?;
        }
        let same = index == 0 && path_key(&from_wide(&file)) == path_key(icon);
        Ok((same, OldIcon { file, index }))
    }

    /// 把指向本程序的快捷方式换成 icon；改了返回改之前的图标，不用改（不是本程序的、已经是这个图标）返回 None。
    /// write 为 false 时只看要不要改，不动文件：要改时也返回现在的图标
    fn update_shortcut(
        path: &Path,
        executable: &Path,
        icon: &Path,
        write: bool,
    ) -> windows::core::Result<Option<OldIcon>> {
        let link: IShellLinkW =
            unsafe { CoCreateInstance(&ShellLink, None, CLSCTX_INPROC_SERVER)? };
        let persist: IPersistFile = link.cast()?;
        let file = wide(path);
        // 不 Resolve：避免扫描无关快捷方式时访问网络或弹出寻找目标的提示。
        if unsafe { persist.Load(PCWSTR(file.as_ptr()), STGM_READ) }.is_err() {
            return Ok(None);
        }
        let mut target = vec![0u16; 32768];
        if unsafe { link.GetPath(&mut target, std::ptr::null_mut(), 0) }.is_err() {
            return Ok(None);
        }
        // 必须指向当前安装的完整 exe 路径；其他安装、同名快捷方式和其他程序不改动。
        if path_key(&from_wide(&target)) != path_key(executable) {
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
        if path_key(&from_wide(&target)) != path_key(executable) {
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

    /// 告诉 Shell 快捷方式的图标换了。SHUpdateImage 要的是换之前的图标（文件、序号）和它在系统图标列表里的位置，
    /// Shell 据此丢掉旧图标的缓存，用到它的地方（桌面、任务栏）重新取。
    /// 以前给的是新图标和保存后按快捷方式取的位置，旧缓存有时没被清掉，任务栏重建按钮后还是旧图标
    /// （实测 12 次里 1 次）。位置按旧图标文件本身查（Shell_GetCachedImageIndex），连着快速切时也准
    fn refresh_shortcut(path: &Path, old: &OldIcon) {
        let file = wide(path);
        unsafe {
            SHChangeNotify(
                SHCNE_UPDATEITEM,
                SHCNF_PATHW | SHCNF_FLUSH,
                Some(file.as_ptr().cast()),
                None,
            );
            // 没缓存过（返回 -1）就没有要丢的
            let image = Shell_GetCachedImageIndexW(PCWSTR(old.file.as_ptr()), old.index, 0);
            if image >= 0 {
                SHUpdateImageW(PCWSTR(old.file.as_ptr()), old.index, 0, image);
            }
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
            let shortcut = dir.root.join("Desktop/renamed deploy tool.lnk");
            create_link(&shortcut, &exe);
            let cache = dir.root.join("cache");
            let light = export_icon(&cache, false).unwrap();
            let dark = export_icon(&cache, true).unwrap();
            assert_ne!(light, dark);
            assert_eq!(std::fs::read(&light).unwrap(), LIGHT_ICON);
            assert_eq!(std::fs::read(&dark).unwrap(), DARK_ICON);
            for icon in [&light, &dark, &light] {
                // 只检查：说要改，但文件不动
                let before = std::fs::read(&shortcut).unwrap();
                assert!(update_shortcut(&shortcut, &exe, icon, false)
                    .unwrap()
                    .is_some());
                assert_eq!(std::fs::read(&shortcut).unwrap(), before);
                assert!(update_shortcut(&shortcut, &exe, icon, true)
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
                assert!(update_shortcut(&shortcut, &exe, icon, false)
                    .unwrap()
                    .is_none());
                assert!(update_shortcut(&shortcut, &exe, icon, true)
                    .unwrap()
                    .is_none());
            }
            assert_eq!(export_icon(&cache, false).unwrap(), light);
        }

        #[test]
        fn scan_updates_desktop_menu_and_pins_only_for_the_exact_executable() {
            let _com = ComApartment::enter().unwrap();
            let dir = TestDirectory::new();
            let exe = dir.executable("current");
            let other_exe = dir.executable("different install");
            let desktop = dir.root.join("Desktop");
            let programs = dir.root.join("Programs");
            let pins = dir.root.join("TaskBar");
            let own = [
                desktop.join("renamed.lnk"),
                programs.join("Ferry/subfolder/Ferry.lnk"),
                pins.join("Ferry.lnk"),
            ];
            for path in &own {
                create_link(path, &exe);
            }
            let foreign = desktop.join("Ferry.lnk");
            create_link(&foreign, &other_exe);
            let original = std::fs::read(&foreign).unwrap();
            std::fs::write(desktop.join("broken.lnk"), b"not a shell link").unwrap();
            let icon = export_icon(&dir.root.join("cache"), true).unwrap();
            let roots = [(desktop, false), (programs, true), (pins, false)];
            // 只检查：数出 3 个要改，一个文件都不动
            let before: Vec<_> = own.iter().map(|p| std::fs::read(p).unwrap()).collect();
            let mut check = SyncReport::default();
            for (root, recursive) in &roots {
                visit_shortcuts(root, *recursive, 0, &exe, &icon, false, &mut check);
            }
            assert_eq!(check.updated, 3);
            assert!(check.warnings.is_empty(), "{:?}", check.warnings);
            let after: Vec<_> = own.iter().map(|p| std::fs::read(p).unwrap()).collect();
            assert_eq!(after, before);
            let mut report = SyncReport::default();
            for (root, recursive) in &roots {
                visit_shortcuts(root, *recursive, 0, &exe, &icon, true, &mut report);
            }
            assert_eq!(report.updated, 3);
            assert!(report.warnings.is_empty(), "{:?}", report.warnings);
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
        fn taskbar_window_properties_follow_both_themes() {
            let _com = ComApartment::enter().unwrap();
            let dir = TestDirectory::new();
            let exe = dir.executable("current");
            let window = TestWindow::new(WINDOW_STYLE(0));
            for dark in [false, true, false] {
                let icon = export_icon(&dir.root.join("cache"), dark).unwrap();
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
