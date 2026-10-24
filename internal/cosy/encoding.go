package cosy

import (
	"encoding/base64"
	"fmt"
)

// Alphabet Qoder 系上行 body 使用的私有 base64 字母表（Encode=1 时生效）。
const Alphabet = "_doRTgHZBKcGVjlvpC,@aFSx#DPuNJme&i*MzLOEn)sUrthbf%Y^w.(kIQyXqWA!"

// Padding 私有字母表的填充字符。
const Padding = '$'

// bodyEncoding 私有字母表编码器。Strict 保证解码严格，便于定位越界字符。
var bodyEncoding = base64.NewEncoding(Alphabet).WithPadding(Padding).Strict()

// EncodeBody 私有字母表编码 + 外三段对调。
//
// 步骤：先用私有字母表做标准 base64，再把结果的首尾各 len/3 段互换：
//
//	std[n-a:] + std[a:n-a] + std[:a]，其中 a = n/3
func EncodeBody(raw []byte) []byte {
	encoded := []byte(bodyEncoding.EncodeToString(raw))
	return swapOuterThirds(encoded)
}

// DecodeBody EncodeBody 的逆操作，用于自测与排障。
func DecodeBody(encoded []byte) ([]byte, error) {
	src := swapOuterThirds(append([]byte(nil), encoded...))
	return bodyEncoding.DecodeString(string(src))
}

// MustDecodeBody 同 DecodeBody，出错时 panic，仅测试使用。
func MustDecodeBody(encoded []byte) []byte {
	out, err := DecodeBody(encoded)
	if err != nil {
		panic(fmt.Sprintf("cosy: decode body: %v", err))
	}
	return out
}

// swapOuterThirds 首尾各 q=len/3 段互换（自逆运算）。
func swapOuterThirds(src []byte) []byte {
	n := len(src)
	q := n / 3
	out := make([]byte, 0, n)
	out = append(out, src[n-q:]...)
	out = append(out, src[q:n-q]...)
	out = append(out, src[:q]...)
	return out
}
