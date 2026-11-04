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
