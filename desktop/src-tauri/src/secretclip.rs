//! 复制密码（「打开网页」用系统的浏览器打开、要用户粘贴密码时）：
//! Windows 上标记成不进剪贴板历史（Win+V）、不同步到云剪贴板，30 秒后剪贴板还是它的话清掉。
//! 其他系统由界面用普通的复制。

#[cfg(windows)]
#[tauri::command]
pub fn copy_secret(webview: tauri::Webview, app: tauri::AppHandle, text: String) -> Result<(), String> {
    use tauri::Manager;
    use windows::Win32::System::DataExchange::GetClipboardSequenceNumber;

    if webview.label() != "main" {
        return Err("不允许从这里调用".into());
    }
    let w = app.get_webview_window("main").ok_or("主窗口不在")?;
    let hwnd = w.hwnd().map_err(|e| e.to_string())?;
    unsafe { set_secret(hwnd, &text) }?;
    let seq = unsafe { GetClipboardSequenceNumber() };
    let raw = hwnd.0 as isize;
    std::thread::spawn(move || {
        std::thread::sleep(std::time::Duration::from_secs(30));
        unsafe { clear_if_unchanged(raw, seq) };
    });
    Ok(())
}

#[cfg(not(windows))]
#[tauri::command]
pub fn copy_secret(_webview: tauri::Webview, _text: String) -> Result<(), String> {
    Err("unsupported".into())
}

#[cfg(windows)]
unsafe fn open(hwnd: windows::Win32::Foundation::HWND) -> Result<(), String> {
    use windows::Win32::System::DataExchange::OpenClipboard;
    // 别的程序正开着剪贴板时会失败：等一下再试
    for _ in 0..10 {
        if unsafe { OpenClipboard(Some(hwnd)) }.is_ok() {
            return Ok(());
        }
        std::thread::sleep(std::time::Duration::from_millis(30));
    }
    Err("剪贴板被别的程序占着，复制失败".into())
}

#[cfg(windows)]
unsafe fn put(format: u32, bytes: &[u8]) -> Result<(), String> {
    use windows::Win32::Foundation::HANDLE;
    use windows::Win32::System::DataExchange::SetClipboardData;
    use windows::Win32::System::Memory::{GlobalAlloc, GlobalLock, GlobalUnlock, GMEM_MOVEABLE};
    unsafe {
        let h = GlobalAlloc(GMEM_MOVEABLE, bytes.len().max(1)).map_err(|e| e.to_string())?;
        let p = GlobalLock(h) as *mut u8;
        if p.is_null() {
            return Err("复制失败".into());
        }
        std::ptr::copy_nonoverlapping(bytes.as_ptr(), p, bytes.len());
        let _ = GlobalUnlock(h);
        // 交给剪贴板后由系统释放
        SetClipboardData(format, Some(HANDLE(h.0))).map_err(|e| e.to_string())?;
    }
    Ok(())
}

#[cfg(windows)]
unsafe fn set_secret(hwnd: windows::Win32::Foundation::HWND, text: &str) -> Result<(), String> {
    use windows::core::w;
    use windows::Win32::System::DataExchange::{CloseClipboard, EmptyClipboard, RegisterClipboardFormatW};
    const CF_UNICODETEXT: u32 = 13;
    unsafe {
        open(hwnd)?;
        let r = (|| {
            EmptyClipboard().map_err(|e| e.to_string())?;
            let wide: Vec<u8> = text.encode_utf16().chain(std::iter::once(0)).flat_map(|c| c.to_le_bytes()).collect();
            put(CF_UNICODETEXT, &wide)?;
            // 剪贴板历史、云剪贴板、剪贴板监视程序（密码管理器的做法）
            let zero = 0u32.to_le_bytes();
            put(RegisterClipboardFormatW(w!("ExcludeClipboardContentFromMonitorProcessing")), &zero)?;
            put(RegisterClipboardFormatW(w!("CanIncludeInClipboardHistory")), &zero)?;
            put(RegisterClipboardFormatW(w!("CanUploadToCloudClipboard")), &zero)?;
            Ok(())
        })();
        let _ = CloseClipboard();
        r
    }
}

#[cfg(windows)]
unsafe fn clear_if_unchanged(raw: isize, seq: u32) {
    use windows::Win32::Foundation::HWND;
    use windows::Win32::System::DataExchange::{CloseClipboard, EmptyClipboard, GetClipboardSequenceNumber};
    unsafe {
        if GetClipboardSequenceNumber() != seq {
            return; // 用户之后复制了别的：不动
        }
        if open(HWND(raw as _)).is_ok() {
            let _ = EmptyClipboard();
            let _ = CloseClipboard();
        }
    }
}
