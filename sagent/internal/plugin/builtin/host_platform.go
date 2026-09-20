package builtin

import "runtime"

// isDarwin 平台标记：macOS 走降级采集路径（/proc、/sys 不存在）
var isDarwin = runtime.GOOS == "darwin"
