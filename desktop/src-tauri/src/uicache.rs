//! Ferry 界面自己的缓存（WebView2 的网页缓存、脚本编译缓存、显卡着色器缓存）。
//! 在数据目录的 webview 里，用久了、升级多了会越攒越多。只清缓存，不动界面保存的设置。

use std::path::{Path, PathBuf};

use tauri::{Manager, Webview};

/// 界面的 WebView2 数据目录；None = 用的系统默认位置（macOS），不能单独统计、清理
pub struct UiDir(pub Option<PathBuf>);

/// WebView2 清缓存时会清掉的子目录（网页缓存、脚本编译缓存、显卡缓存），用到时会重新生成。
/// 实测（2026-11）：这三个清完只剩索引文件；着色器缓存（GrShaderCache 等）不清，也不大，不算在里面
const CACHE_DIRS: &[&str] = &[
    "EBWebView/Default/Cache",
    "EBWebView/Default/Code Cache",
    "EBWebView/Default/GPUCache",
];

const VERSION_MARKER: &str = ".ferry-version";

/// 应用版本变化时（升级 / 降级），在 WebView2 启动前自动清理磁盘缓存、脚本编译缓存与显卡缓存，
/// 不触碰设置、Cookie 和本地存储；文件此时未被占用，可直接删除。返回 true 表示实际执行了清理。
#[cfg(any(windows, test))]
pub fn clear_if_upgraded(dir: &Path, current_version: &str) -> bool {
    let version = current_version.trim();
    if version.is_empty() {
        return false;
    }
    let marker = dir.join(VERSION_MARKER);
    if let Ok(saved) = std::fs::read_to_string(&marker) {
        if saved.trim() == version {
            return false;
        }
    }
    let mut cleared = false;
    if dir.join("EBWebView").exists() {
        for sub in CACHE_DIRS {
            let p = dir.join(sub);
            if p.exists() && std::fs::remove_dir_all(&p).is_ok() {
                cleared = true;
            }
        }
    }
    if std::fs::create_dir_all(dir).is_ok() {
        let _ = std::fs::write(&marker, version);
    }
    cleared
}

fn usage(dir: &Path) -> u64 {
    CACHE_DIRS
        .iter()
        .map(|d| crate::browser::dir_size(&dir.join(d)))
        .sum()
}

fn ui_dir(app: &tauri::AppHandle) -> Option<PathBuf> {
    if !cfg!(windows) {
        return None;
    }
    app.state::<UiDir>().0.clone()
}

/// 界面缓存占用多少字节；None = 不能单独统计、清理
#[tauri::command]
pub async fn ui_cache_usage(
    app: tauri::AppHandle,
    webview: Webview,
) -> Result<Option<u64>, String> {
    crate::main_only(&webview)?;
    let Some(d) = ui_dir(&app) else {
        return Ok(None);
    };
    tauri::async_runtime::spawn_blocking(move || Some(usage(&d)))
        .await
        .map_err(|e| e.to_string())
}

/// 清除界面缓存，返回清完后还占用多少字节
#[tauri::command]
pub async fn ui_cache_clear(app: tauri::AppHandle, webview: Webview) -> Result<u64, String> {
    crate::main_only(&webview)?;
    let Some(d) = ui_dir(&app) else {
        return Err("这个系统上不能单独清理界面缓存".into());
    };
    tauri::async_runtime::spawn_blocking(move || {
        clear(&webview)?;
        Ok(usage(&d))
    })
    .await
    .map_err(|e| e.to_string())?
}

/// 界面正开着，缓存文件被占用，不能直接删：交给 WebView2 清（只清缓存，不动界面保存的数据）
#[cfg(windows)]
fn clear(v: &Webview) -> Result<(), String> {
    use webview2_com::ClearBrowsingDataCompletedHandler;
    use webview2_com::Microsoft::Web::WebView2::Win32::{
        ICoreWebView2Profile2, ICoreWebView2_13, COREWEBVIEW2_BROWSING_DATA_KINDS_DISK_CACHE,
    };
    use windows::core::Interface;

    let (tx, rx) = std::sync::mpsc::channel::<Result<(), String>>();
    let tx2 = tx.clone();
    v.with_webview(move |pw| unsafe {
        let r = pw
            .controller()
            .CoreWebView2()
            .and_then(|c| c.cast::<ICoreWebView2_13>())
            .and_then(|c| c.Profile())
            .and_then(|p| p.cast::<ICoreWebView2Profile2>())
            .and_then(|p| {
                p.ClearBrowsingData(
                    COREWEBVIEW2_BROWSING_DATA_KINDS_DISK_CACHE,
                    &ClearBrowsingDataCompletedHandler::create(Box::new(move |hr| {
                        let _ = tx2.send(hr.map_err(|e| e.to_string()));
                        Ok(())
                    })),
                )
            });
        if let Err(e) = r {
            let _ = tx.send(Err(e.to_string()));
        }
    })
    .map_err(|e| e.to_string())?;
    rx.recv_timeout(std::time::Duration::from_secs(60))
        .map_err(|_| "清理超时".to_string())?
        .map_err(|e| format!("清理失败：{e}"))
}

#[cfg(not(windows))]
fn clear(_: &Webview) -> Result<(), String> {
    Err("这个系统上不能单独清理界面缓存".into())
}

#[cfg(test)]
mod tests {
    use super::*;
    use std::sync::atomic::{AtomicU64, Ordering};

    static NEXT_TEST: AtomicU64 = AtomicU64::new(0);

    fn temp_ui_dir() -> PathBuf {
        let dir = std::env::temp_dir().join(format!(
            "ferry-uicache-test-{}-{}",
            std::process::id(),
            NEXT_TEST.fetch_add(1, Ordering::Relaxed)
        ));
        let _ = std::fs::remove_dir_all(&dir);
        std::fs::create_dir_all(&dir).unwrap();
        dir
    }

    fn seed_webview(dir: &Path) {
        for sub in CACHE_DIRS {
            let p = dir.join(sub);
            std::fs::create_dir_all(&p).unwrap();
            std::fs::write(p.join("entry.bin"), b"cached").unwrap();
        }
        let keep = dir.join("EBWebView/Default/Local Storage");
        std::fs::create_dir_all(&keep).unwrap();
        std::fs::write(keep.join("prefs.ldb"), b"keep-me").unwrap();
    }

    #[test]
    fn clears_cache_on_first_upgrade_and_version_change_only() {
        let dir = temp_ui_dir();
        seed_webview(&dir);

        // 从未记录版本号（从旧版升级上来）：清掉 3 个缓存目录，保留本地存储，写下当前版本
        assert!(clear_if_upgraded(&dir, "0.18.9"));
        for sub in CACHE_DIRS {
            assert!(!dir.join(sub).exists(), "{sub} 应被清理");
        }
        assert_eq!(
            std::fs::read(dir.join("EBWebView/Default/Local Storage/prefs.ldb")).unwrap(),
            b"keep-me"
        );
        assert_eq!(
            std::fs::read_to_string(dir.join(VERSION_MARKER)).unwrap(),
            "0.18.9"
        );

        // 相同版本再次启动：不再清理缓存
        seed_webview(&dir);
        assert!(!clear_if_upgraded(&dir, "0.18.9"));
        for sub in CACHE_DIRS {
            assert!(dir.join(sub).join("entry.bin").exists(), "同版本不应清理 {sub}");
        }

        // 升级到新版本：再次清理缓存并更新版本标记
        assert!(clear_if_upgraded(&dir, "0.19.0"));
        for sub in CACHE_DIRS {
            assert!(!dir.join(sub).exists(), "升级后 {sub} 应被清理");
        }
        assert_eq!(
            std::fs::read(dir.join("EBWebView/Default/Local Storage/prefs.ldb")).unwrap(),
            b"keep-me"
        );
        assert_eq!(
            std::fs::read_to_string(dir.join(VERSION_MARKER)).unwrap(),
            "0.19.0"
        );

        let _ = std::fs::remove_dir_all(&dir);
    }

    #[test]
    fn fresh_install_writes_marker_without_error() {
        let dir = temp_ui_dir().join("fresh");
        assert!(!clear_if_upgraded(&dir, "0.18.9"));
        assert_eq!(
            std::fs::read_to_string(dir.join(VERSION_MARKER)).unwrap(),
            "0.18.9"
        );
        let _ = std::fs::remove_dir_all(dir.parent().unwrap());
    }
}
