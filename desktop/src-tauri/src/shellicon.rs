//! Windows 桌面、开始菜单和固定任务栏快捷方式的主题图标。

#[derive(Default, serde::Serialize)]
pub struct SyncReport {
    updated: usize,
    warnings: Vec<String>,
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
        let hwnd = window.hwnd().map_err(|e| e.to_string())?.0 as isize;
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
            windows_shell::sync(hwnd, &executable, &cache, &identifier, dark)
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

#[cfg(windows)]
mod windows_shell {
    use super::SyncReport;
    use std::collections::hash_map::DefaultHasher;
    use std::ffi::OsString;
    use std::hash::{Hash, Hasher};
    use std::os::windows::ffi::{OsStrExt, OsStringExt};
    use std::path::{Path, PathBuf};
    use std::sync::atomic::{AtomicU64, Ordering};
    use std::sync::Mutex;
    use std::time::Duration;
    use windows::core::{Interface, GUID, PCWSTR};
    use windows::Win32::Foundation::{HWND, PROPERTYKEY, RPC_E_CHANGED_MODE};
    use windows::Win32::System::Com::StructuredStorage::PROPVARIANT;
    use windows::Win32::System::Com::{
        CoCreateInstance, CoInitializeEx, CoTaskMemFree, CoUninitialize, IPersistFile,
        CLSCTX_INPROC_SERVER, COINIT_APARTMENTTHREADED, STGM_READ, STGM_READWRITE,
    };
    use windows::Win32::System::Variant::VT_LPWSTR;
    use windows::Win32::UI::Shell::PropertiesSystem::{
        IPropertyStore, SHGetPropertyStoreForWindow,
    };
    use windows::Win32::UI::Shell::{
        FOLDERID_CommonPrograms, FOLDERID_Desktop, FOLDERID_Programs, FOLDERID_PublicDesktop,
        FOLDERID_RoamingAppData, IShellLinkW, ITaskbarList, SHChangeNotify, SHGetKnownFolderPath,
        SHStrDupW, SHUpdateImageW, ShellLink, Shell_GetCachedImageIndexW, TaskbarList,
        KF_FLAG_DEFAULT, SHCNE_UPDATEITEM, SHCNF_FLUSH, SHCNF_PATHW,
    };
    use windows::Win32::UI::WindowsAndMessaging::{GetForegroundWindow, IsWindowVisible};

    const DARK_ICON: &[u8] = include_bytes!("../icons/themes/ferry-dark.ico");
    const LIGHT_ICON: &[u8] = include_bytes!("../icons/themes/ferry-light.ico");
    /// 删掉任务栏按钮后隔多久再加回来（毫秒），见 refresh_taskbar
    const TASKBAR_GAP_MS: u64 = 600;
    /// 改完快捷方式后等多久再重建任务栏按钮（毫秒），见 schedule_taskbar
    const TASKBAR_SETTLE_MS: u64 = 2500;
    /// 同步的次数：补重建任务栏前看一眼，期间又切过主题就不补
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
        hwnd: isize,
        executable: &Path,
        cache: &Path,
        identifier: &str,
        dark: bool,
    ) -> Result<SyncReport, String> {
        let generation = SYNCS.fetch_add(1, Ordering::SeqCst) + 1;
        let _com = ComApartment::enter().map_err(|e| e.to_string())?;
        let icon = export_icon(cache, dark).map_err(|e| e.to_string())?;
        let mut report = SyncReport::default();
        for (root, recursive) in shortcut_roots() {
            visit_shortcuts(&root, recursive, 0, executable, &icon, &mut report);
        }
        // 先存好快捷方式，再改窗口属性（前端也是先同步这里、再换窗口图标）：任务栏看到窗口变了会重新取
        // 这一组的图标记下来，那时快捷方式还是旧的就记成旧图标，之后重建按钮也用这个旧的
        if let Err(error) = update_window(HWND(hwnd as _), executable, &icon, identifier) {
            report.warnings.push(format!("更新任务栏图标失败：{error}"));
        }
        // 合并任务栏按钮按分组缓存图标；更新 WM_SETICON 和快捷方式不会刷新已显示的按钮，要重建按钮。
        // 启动时窗口还没显示，按钮还没建，不用重建
        if unsafe { IsWindowVisible(HWND(hwnd as _)) }.as_bool() {
            schedule_taskbar(hwnd, generation);
        }
        Ok(report)
    }

    /// 同一时间只有一个重建在做：两个重建的删、加交错时，按钮会拿到错的图标（实测连着快速切主题时出现）
    static TASKBAR: Mutex<()> = Mutex::new(());

    /// 等快捷方式改动在任务栏那边生效后，只重建一次任务栏按钮（按钮只闪一下）。
    /// 任务栏重建按钮时用的是它自己记的这一组的图标，快捷方式刚改完时它记的有时还是旧的，而且之后一直不变。
    /// 实测（截图逐帧看按钮）：等 1.5 秒再重建约 3% 停在旧图标，等 2.5 秒 60 次全对，快速连着切 10 轮全对。
    /// 期间又切了主题就不重建，由最后那次来做：快速连着切，只在最后闪一下，显示最后的主题
    fn schedule_taskbar(hwnd: isize, generation: u64) {
        std::thread::spawn(move || {
            std::thread::sleep(Duration::from_millis(TASKBAR_SETTLE_MS));
            if SYNCS.load(Ordering::SeqCst) != generation {
                return;
            }
            let _turn = TASKBAR.lock().unwrap_or_else(|e| e.into_inner());
            // 等前一个重建做完的这段时间里也可能又切了
            if SYNCS.load(Ordering::SeqCst) != generation {
                return;
            }
            if let Ok(_com) = ComApartment::enter() {
                let _ = refresh_taskbar(HWND(hwnd as _));
            }
        });
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

    /// 删掉再加回这个窗口的任务栏按钮，让任务栏重新取图标。窗口还没显示时不动，返回 false
    fn refresh_taskbar(hwnd: HWND) -> windows::core::Result<bool> {
        unsafe {
            // 启动时窗口尚未显示，不提前创建任务栏按钮；最小化窗口仍有 WS_VISIBLE。
            if !IsWindowVisible(hwnd).as_bool() {
                return Ok(false);
            }
            let foreground = GetForegroundWindow() == hwnd;
            let taskbar: ITaskbarList = CoCreateInstance(&TaskbarList, None, CLSCTX_INPROC_SERVER)?;
            taskbar.HrInit()?;
            taskbar.DeleteTab(hwnd)?;
            // 删掉后任务栏的按钮要播完消失动画才真正没了，这之前马上加回来会接着用这个旧按钮和旧图标，
            // 之后一直不变（实测马上加回 16 次里 8 次停在旧图标，隔 0.6 秒 2～3 次）
            std::thread::sleep(Duration::from_millis(TASKBAR_GAP_MS));
            // 即使第一次添加遇到瞬时错误，也再尝试恢复任务栏项。
            taskbar.AddTab(hwnd).or_else(|_| taskbar.AddTab(hwnd))?;
            if foreground && GetForegroundWindow() == hwnd {
                // 仅还原原本处于前台的按钮状态，不激活窗口或抢走焦点。
                taskbar.ActivateTab(hwnd)?;
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
                visit_shortcuts(&path, true, depth + 1, executable, icon, report);
            } else if kind.is_file()
                && path
                    .extension()
                    .is_some_and(|e| e.eq_ignore_ascii_case("lnk"))
            {
                match update_shortcut_retrying(&path, executable, icon) {
                    Ok(Some(old)) => {
                        report.updated += 1;
                        refresh_shortcut(&path, &old);
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
    ) -> windows::core::Result<Option<OldIcon>> {
        const BUSY: [windows::core::HRESULT; 2] = [
            windows::core::HRESULT(0x8007_04C8_u32 as i32),
            windows::core::HRESULT(0x8007_0020_u32 as i32),
        ];
        let mut attempt = 0;
        loop {
            match update_shortcut(path, executable, icon) {
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

    /// 把指向本程序的快捷方式换成 icon；改了返回改之前的图标，不用改（不是本程序的、已经是这个图标）返回 None
    fn update_shortcut(
        path: &Path,
        executable: &Path,
        icon: &Path,
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
        // 只对确认属于当前程序的链接请求写权限，并在重新加载后复核目标。
        unsafe {
            persist.Load(PCWSTR(file.as_ptr()), STGM_READWRITE)?;
            link.GetPath(&mut target, std::ptr::null_mut(), 0)?;
        }
        if path_key(&from_wide(&target)) != path_key(executable) {
            return Ok(None);
        }
        let mut current_icon = vec![0u16; 32768];
        let mut index = 0;
        unsafe {
            link.GetIconLocation(&mut current_icon, &mut index)?;
        }
        if index == 0 && path_key(&from_wide(&current_icon)) == path_key(icon) {
            return Ok(None);
        }
        let old = OldIcon {
            file: current_icon,
            index,
        };
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
        use windows::Win32::UI::WindowsAndMessaging::{
            CreateWindowExW, DestroyWindow, WINDOW_EX_STYLE, WS_OVERLAPPEDWINDOW,
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
                assert!(update_shortcut(&shortcut, &exe, icon).unwrap().is_some());
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
                assert!(update_shortcut(&shortcut, &exe, icon).unwrap().is_none());
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
            let mut report = SyncReport::default();
            for (root, recursive) in [(desktop, false), (programs, true), (pins, false)] {
                visit_shortcuts(&root, recursive, 0, &exe, &icon, &mut report);
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
            struct TestWindow(HWND);
            impl Drop for TestWindow {
                fn drop(&mut self) {
                    let _ = unsafe { DestroyWindow(self.0) };
                }
            }
            let window = TestWindow(unsafe {
                CreateWindowExW(
                    WINDOW_EX_STYLE(0),
                    w!("STATIC"),
                    w!("Ferry icon test"),
                    WS_OVERLAPPEDWINDOW,
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
            });
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
    }
}
