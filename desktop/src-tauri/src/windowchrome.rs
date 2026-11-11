//! 只隐藏 Windows 标题栏里的品牌图标和文字，窗口名称及任务栏图标照常保留。

use std::mem::size_of;
use windows::Win32::Foundation::HWND;
use windows::Win32::UI::Controls::{
    SetWindowThemeAttribute, WTA_NONCLIENT, WTA_OPTIONS, WTNCA_NODRAWCAPTION,
    WTNCA_NODRAWICON,
};

pub fn hide_branding(window: &tauri::WebviewWindow) -> Result<(), Box<dyn std::error::Error>> {
    let hwnd = HWND(window.hwnd()?.0);
    let flags = WTNCA_NODRAWCAPTION | WTNCA_NODRAWICON;
    let options = WTA_OPTIONS {
        dwFlags: flags,
        dwMask: flags,
    };
    // 不清空窗口标题或 WM_SETICON：任务切换、无障碍名称和主题图标仍使用 Ferry。
    // https://learn.microsoft.com/windows/win32/controls/wtnca
    unsafe {
        SetWindowThemeAttribute(
            hwnd,
            WTA_NONCLIENT,
            (&options as *const WTA_OPTIONS).cast(),
            size_of::<WTA_OPTIONS>() as u32,
        )?;
    }
    Ok(())
}
