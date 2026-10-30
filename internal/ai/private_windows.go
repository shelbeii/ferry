package ai

import (
	"sync"

	"golang.org/x/sys/windows"
)

var privateDone sync.Map

// makePrivate Windows 上 0600 / 0700 不起作用：给目录设只有当前用户（和 SYSTEM）能访问的权限，里面的文件继承它。
// 同一个目录只设一次；失败不影响使用（数据仍然写得进去）。
func makePrivate(dir string) {
	if _, done := privateDone.LoadOrStore(dir, true); done {
		return
	}
	u, err := windows.GetCurrentProcessToken().GetTokenUser()
	if err != nil {
		return
	}
	// P = 不继承上级目录的权限；OICI = 子目录和文件都继承；FA = 完全控制
	sd, err := windows.SecurityDescriptorFromString("D:P(A;OICI;FA;;;" + u.User.Sid.String() + ")(A;OICI;FA;;;SY)")
	if err != nil {
		return
	}
	dacl, _, err := sd.DACL()
	if err != nil {
		return
	}
	windows.SetNamedSecurityInfo(dir, windows.SE_FILE_OBJECT,
		windows.DACL_SECURITY_INFORMATION|windows.PROTECTED_DACL_SECURITY_INFORMATION, nil, nil, dacl, nil)
}
