;  Ferry 安装程序的钩子（tauri.conf.json → bundle.windows.nsis.installerHooks）。
;
;  Ferry 的数据（设置、项目列表、密钥、主机公钥记录、构建日志、JDK 缓存、界面缓存）默认放在安装目录下的 data 里。
;  升级、卸载只删除安装程序自己装的文件，data 保留；卸载时勾选「删除应用程序数据」并确认后才删除。

;  ferry.exe 可能正被别的程序运行着（编程软件启动的 ferry mcp、终端里的 ferry logs -f），这时无法覆盖。
;  Windows 允许给运行中的程序改名：先把它改名放一边，运行中的进程不受影响，下次启动就用新版本。
;  改名后复制一份放回原处（没人占用，后面能覆盖）：之后安装中途取消（如提示 Ferry 正在运行时），旧版本照样能用。
;  以前改名留下的文件不再被占用时在这里删掉（还被占用的删不掉，留到下一次）。
;  改名、复制后再确认能写：有时别的程序短暂占着（改名失败、刚复制的文件被打开），最多等 10 秒；
;  还是不行就说明可能是谁占着，关掉后继续（再不行才是 NSIS 的「重试 / 忽略」）。
;  每一步记在 %TEMP%\ferry-install.log，出问题时好查。
!macro FERRY_LOG text
  FileOpen $6 "$TEMP\ferry-install.log" a
  FileSeek $6 0 END
  FileWrite $6 "${text}$\r$\n"
  FileClose $6
!macroend

!macro NSIS_HOOK_PREINSTALL
  Delete "$INSTDIR\ferry.exe.old-*"
  Push $R9
  Push $9
  Push $8
  Push $7
  Push $6
  StrCpy $8 0 ; 试了几次
  StrCpy $7 "" ; 改名放一边的文件
  System::Call "kernel32::GetTickCount()i.r9"
  !insertmacro FERRY_LOG "[$9] 安装 ${VERSION} 到 $INSTDIR"
  ${If} ${FileExists} "$INSTDIR\ferry.exe"
    ferry_pre_check:
    ClearErrors
    FileOpen $R9 "$INSTDIR\ferry.exe" a
    ${IfNot} ${Errors}
      FileClose $R9
      !insertmacro FERRY_LOG "ferry.exe 可以写（第 $8 次重试）"
      Goto ferry_pre_done
    ${EndIf}
    ${If} $7 == ""
      System::Call "kernel32::GetTickCount()i.r9"
      ClearErrors
      Rename "$INSTDIR\ferry.exe" "$INSTDIR\ferry.exe.old-$9"
      ${If} ${Errors}
        !insertmacro FERRY_LOG "ferry.exe 被占用，改名失败"
      ${Else}
        StrCpy $7 "$INSTDIR\ferry.exe.old-$9"
        ClearErrors
        CopyFiles /SILENT "$7" "$INSTDIR\ferry.exe"
        ${If} ${Errors}
          !insertmacro FERRY_LOG "ferry.exe 被占用，已改名为 $7，复制回原处失败"
        ${Else}
          !insertmacro FERRY_LOG "ferry.exe 被占用，已改名为 $7 并复制回原处"
        ${EndIf}
        Goto ferry_pre_check ; 复制回来的通常马上能写
      ${EndIf}
    ${Else}
      !insertmacro FERRY_LOG "复制回来的 ferry.exe 还不能写"
    ${EndIf}
    IntOp $8 $8 + 1
    ${If} $8 < 20
      Sleep 500
      Goto ferry_pre_check
    ${EndIf}
    !insertmacro FERRY_LOG "等了 10 秒还被占用"
    MessageBox MB_OK|MB_ICONEXCLAMATION "$INSTDIR\ferry.exe 正被别的程序占用，无法更新。$\r$\n$\r$\n可能是：编程软件（Claude Code、Cursor、VS Code 等）启动的 ferry mcp，或终端里正在运行的 ferry 命令。关掉它们后点「确定」继续。$\r$\n$\r$\n（安装过程记在 $TEMP\ferry-install.log）"
    ferry_pre_done:
  ${EndIf}
  Pop $6
  Pop $7
  Pop $8
  Pop $9
  Pop $R9
!macroend

;  ferry 命令加入 PATH：第一次安装时问一次，回答记在 HKCU\Software\Ferry 的 PathChoice（yes / no）。
;  升级、重装时卸载旧版本会把它从 PATH 去掉，装好后按记下的回答再加回来，不再问。设置里也可以随时加入、移除。
!macro NSIS_HOOK_POSTINSTALL
  ReadRegStr $R7 HKCU "Software\Ferry" "PathChoice"
  ${If} $R7 == ""
  ${AndIfNot} ${Silent}
    nsExec::Exec '"$INSTDIR\ferry.exe" path check'
    Pop $R8
    ${If} $R8 == 1
      MessageBox MB_YESNO|MB_ICONQUESTION "把 ferry 命令加入 PATH 吗？$\r$\n$\r$\n加入后在 cmd、PowerShell 和编程软件（Claude Code、Cursor 等）里可以直接输入 ferry。只改当前用户的设置，以后可以在 Ferry 的「设置 → 命令行与 MCP」里移除。" IDNO ferry_path_no
        StrCpy $R7 "yes"
        Goto ferry_path_done
      ferry_path_no:
        StrCpy $R7 "no"
      ferry_path_done:
      WriteRegStr HKCU "Software\Ferry" "PathChoice" $R7
    ${EndIf}
  ${EndIf}
  ${If} $R7 == "yes"
    nsExec::Exec '"$INSTDIR\ferry.exe" path add'
    Pop $R8
  ${EndIf}
!macroend

!macro NSIS_HOOK_PREUNINSTALL
  ${If} $UpdateMode <> 1
    nsExec::Exec '"$INSTDIR\ferry.exe" path remove --keep-choice'
    Pop $R8
  ${EndIf}
!macroend

!macro NSIS_HOOK_POSTUNINSTALL
  Delete "$INSTDIR\ferry.exe.old-*"
  ${If} $DeleteAppDataCheckboxState = 1
  ${AndIf} $UpdateMode <> 1
    ${If} ${FileExists} "$INSTDIR\data\*.*"
      MessageBox MB_YESNO|MB_ICONEXCLAMATION|MB_DEFBUTTON2 "将删除 Ferry 的全部数据：$\r$\n$INSTDIR\data$\r$\n$\r$\n包括设置、项目列表、密钥、主机公钥记录、构建日志、JDK 缓存。私钥删除后无法恢复，用它配置的免密登录会失效。$\r$\n（项目的 ferry.yaml 不在这里，不受影响；设置里手动指定的其他存放位置也不会删除。）$\r$\n$\r$\n确定删除吗？" /SD IDNO IDNO ferry_keep_data
        RMDir /r "$INSTDIR\data"
      ferry_keep_data:
    ${EndIf}
    ; 安装目录不可写时（或旧版本）数据在用户目录
    ${If} ${FileExists} "$APPDATA\Ferry\*.*"
      MessageBox MB_YESNO|MB_ICONEXCLAMATION|MB_DEFBUTTON2 "用户目录里还有 Ferry 的数据：$\r$\n$APPDATA\Ferry$\r$\n$\r$\n一并删除吗？" /SD IDNO IDNO ferry_keep_user_data
        RMDir /r "$APPDATA\Ferry"
      ferry_keep_user_data:
    ${EndIf}
    RMDir "$INSTDIR"
  ${EndIf}
!macroend
