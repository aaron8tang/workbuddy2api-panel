// time.go 时间桥（测试可注入）。
package autoclaw

import "time"

func timeNow() time.Time { return time.Now() }
