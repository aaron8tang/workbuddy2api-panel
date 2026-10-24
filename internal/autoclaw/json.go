// json.go 小桥（便于测试注入与避免循环依赖）。
package autoclaw

import "encoding/json"

func jsonMarshal(v any) ([]byte, error)  { return json.Marshal(v) }
func jsonUnmarshal(b []byte, v any) error { return json.Unmarshal(b, v) }
