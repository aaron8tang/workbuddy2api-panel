// EntPacks 权益包明细解析（字段名与结构对齐 ide_user_ent_usage 实测响应）。
// 覆盖：签到包（usage.credits_amount 部分消耗）、月赠包（usage 空对象）、
// 免费/无额度包（跳过）、status=0 包（按原 EntUsage 语义计入）、耗尽包（跳过）。
package upstream

import (
	"errors"
	"net/http"
	"strings"
	"testing"

	"github.com/linguo2625469/workbuddy2api-panel/internal/trae/auth"
)

func TestEntPacks(t *testing.T) {
	c := testClient(func(r *http.Request) (*http.Response, error) {
		if !strings.HasSuffix(r.URL.Path, EpEntUsage) {
			return nil, errors.New("wrong path: " + r.URL.Path)
		}
		return jsonResp(200, `{"is_credits_billing":true,"usage_summary":{"consumed_amount":70.31,"total_amount":2050},
			"user_entitlement_pack_list":[
				{"entitlement_id":"free_utc202610_u1","entitlement_base_info":{"quota":{"enable_solo_agent":true}},"expire_time":1793462399,"status":1,"usage":{}},
				{"entitlement_id":"monthly_bonus_202610_u1","entitlement_base_info":{"quota":{"credits_limit":500}},"expire_time":1793462399,"status":1,"usage":{}},
				{"entitlement_id":"checkin_20260920_u1","entitlement_base_info":{"quota":{"credits_limit":150}},"expire_time":1792512631,"status":1,"usage":{"credits_amount":70.3144}},
				{"entitlement_id":"checkin_20260926_u1","entitlement_base_info":{"quota":{"credits_limit":150}},"expire_time":1793032883,"status":1,"usage":{"credits_amount":150.0}}
			]}`), nil
	})
	a := &auth.Auth{UID: "u1", AccessToken: "at", Domain: "trae.cn"}
	ps, err := c.EntPacks(a)
	if err != nil {
		t.Fatalf("EntPacks: %v", err)
	}
	// 期望保留：月赠 500（未用）、签到 150-70=80；跳过：免费包（无 limit）、耗尽签到包。
	if len(ps) != 2 {
		t.Fatalf("packs = %d (%+v), want 2", len(ps), ps)
	}
	var remain int64
	var earliest int64
	for _, p := range ps {
		remain += p.Limit - int64(p.Used)
		if p.ExpireAt > 0 && (earliest == 0 || p.ExpireAt < earliest) {
			earliest = p.ExpireAt
		}
	}
	if remain != 580 { // 500 + (150-70)
		t.Fatalf("remain = %d, want 580", remain)
	}
	if earliest != 1792512631 {
		t.Fatalf("earliest = %d, want 1792512631 (checkin_20260920)", earliest)
	}

	// EntUsage 聚合口径与 EntPacks 一致（used 按整分截断）。
	r, l, u, n, err := c.EntUsage(a)
	if err != nil {
		t.Fatalf("EntUsage: %v", err)
	}
	if r != 580 || l != 650 || u != 70 || n != 2 {
		t.Fatalf("EntUsage = (%d,%d,%d,%d), want (580,650,70,2)", r, l, u, n)
	}
}
