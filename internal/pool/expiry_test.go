
package pool

import (
	"testing"
	"time"

)

// 临期优先：带到期日期的号中，到期最早者先被选中；
// 无到期的号排在所有带到期号之后。
func TestPickExpiryFirst(t *testing.T) {
	p := New("")
	p.Add(mkCred("late"))
	p.Add(mkCred("soon"))
	now := time.Now()
	// 每轮重置 lastUsed 相同 → 之前 LRU 下应按加入顺序交替；
	// 现在 soon 到期更早，应持续被先选（除它被排除时）。
	p.SetExpireAt("soon", now.Add(2*24*time.Hour))
	p.SetExpireAt("late", now.Add(30*24*time.Hour))

	for i := 0; i < 4; i++ {
		got := p.Pick()
		if got == nil || got.UID != "soon" {
			t.Fatalf("iter %d: Pick = %v, want soon (earliest expiry)", i, got)
		}
	}
}

// 到期信息缺失（zero）的号排在带到期信息之后；全无到期退化为轮询。
func TestPickExpiryUnknownGoesLast(t *testing.T) {
	p := New("")
	p.Add(mkCred("plain"))
	p.Add(mkCred("expiring"))
	p.SetExpireAt("expiring", time.Now().Add(3*24*time.Hour))
	// plain 无到期 → 每次都应先选 expiring
	if got := p.Pick(); got == nil || got.UID != "expiring" {
		t.Fatalf("Pick = %v, want expiring (expiry known beats unknown)", got)
	}
	// 停掉 expiring 后再退化为轮询 plain
	p.Disable("expiring", "test")
	if got := p.Pick(); got == nil || got.UID != "plain" {
		t.Fatalf("Pick = %v, want plain", got)
	}
}
